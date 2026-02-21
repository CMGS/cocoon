//go:build linux

package engine

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CMGS/cocoon/config"
)

func testVirtiofsdConfig(t *testing.T) *config.CocoonConfig {
	t.Helper()

	cfg := config.DefaultConfig()
	cfg.RebaseRootDir(t.TempDir())
	cfg.LogDir = t.TempDir()
	cfg.RuntimeDir = t.TempDir()
	return cfg
}

func TestVirtiofsdRuntimeStartSuccess(t *testing.T) {
	t.Parallel()

	cfg := testVirtiofsdConfig(t)
	vmID := "vm-test-virtiofsd-start"
	sharedDir := cfg.VMOCIMergedDir(vmID)
	socketPath := cfg.VMOCIRootfsVirtioFSSocketPath(vmID)

	mgr := newVirtiofsdRuntimeManager(cfg)

	removedSocket := ""
	mgr.removeFn = func(path string) error {
		removedSocket = path
		return nil
	}
	mgr.resolveBinaryFn = func(file string) (string, error) {
		if file != "virtiofsd" {
			t.Fatalf("resolve binary arg = %q, want virtiofsd", file)
		}
		return "/usr/libexec/virtiofsd", nil
	}
	mgr.launchFn = func(_ context.Context, binary string, args []string, logPath string) (int, error) {
		if binary != "/usr/libexec/virtiofsd" {
			t.Fatalf("binary = %q, want /usr/libexec/virtiofsd", binary)
		}
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "--shared-dir "+sharedDir) {
			t.Fatalf("args missing shared-dir: %v", args)
		}
		if !strings.Contains(joined, "--socket-path "+socketPath) {
			t.Fatalf("args missing socket-path: %v", args)
		}
		if !strings.Contains(joined, "--cache=auto") {
			t.Fatalf("args missing cache=auto: %v", args)
		}
		if !strings.Contains(joined, "--sandbox chroot") {
			t.Fatalf("args missing sandbox=chroot: %v", args)
		}
		if !strings.HasSuffix(logPath, vmID+"-virtiofsd.log") {
			t.Fatalf("log path = %q, want suffix %q", logPath, vmID+"-virtiofsd.log")
		}
		return 4321, nil
	}
	mgr.waitReadyFn = func(_ context.Context, gotSocket string, gotPID int, _ time.Duration) error {
		if gotSocket != socketPath {
			t.Fatalf("socket = %q, want %q", gotSocket, socketPath)
		}
		if gotPID != 4321 {
			t.Fatalf("pid = %d, want 4321", gotPID)
		}
		return nil
	}

	info, err := mgr.Start(t.Context(), vmID, sharedDir, socketPath)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if info.PID != 4321 {
		t.Fatalf("info.PID = %d, want 4321", info.PID)
	}
	if info.SocketPath != socketPath {
		t.Fatalf("info.SocketPath = %q, want %q", info.SocketPath, socketPath)
	}
	if info.ProcessName != "virtiofsd" {
		t.Fatalf("info.ProcessName = %q, want virtiofsd", info.ProcessName)
	}
	if removedSocket != socketPath {
		t.Fatalf("stale socket cleanup path = %q, want %q", removedSocket, socketPath)
	}
}

func TestVirtiofsdRuntimeStartReadyFailureStopsProcess(t *testing.T) {
	t.Parallel()

	cfg := testVirtiofsdConfig(t)
	vmID := "vm-test-virtiofsd-fail"
	sharedDir := cfg.VMOCIMergedDir(vmID)
	socketPath := cfg.VMOCIRootfsVirtioFSSocketPath(vmID)

	mgr := newVirtiofsdRuntimeManager(cfg)
	mgr.resolveBinaryFn = func(string) (string, error) { return "/usr/bin/virtiofsd", nil }
	mgr.launchFn = func(context.Context, string, []string, string) (int, error) { return 7788, nil }

	waitErr := errors.New("socket not ready")
	mgr.waitReadyFn = func(context.Context, string, int, time.Duration) error { return waitErr }

	stopCalled := false
	mgr.stopFn = func(pid int, expectedProc string, _ time.Duration) error {
		stopCalled = true
		if pid != 7788 {
			t.Fatalf("stop pid = %d, want 7788", pid)
		}
		if expectedProc != "virtiofsd" {
			t.Fatalf("expected process = %q, want virtiofsd", expectedProc)
		}
		return nil
	}
	removeCalls := 0
	mgr.removeFn = func(string) error {
		removeCalls++
		return nil
	}

	_, err := mgr.Start(t.Context(), vmID, sharedDir, socketPath)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), waitErr.Error()) {
		t.Fatalf("Start error = %v, want contains %q", err, waitErr)
	}
	if !stopCalled {
		t.Fatal("expected stopFn to be called on readiness failure")
	}
	if removeCalls < 2 {
		t.Fatalf("removeFn called %d times, want at least 2 (pre-clean + failure cleanup)", removeCalls)
	}
}

func TestVirtiofsdRuntimeStop(t *testing.T) {
	t.Parallel()

	cfg := testVirtiofsdConfig(t)
	vmID := "vm-test-virtiofsd-stop"
	socketPath := filepath.Join(t.TempDir(), "virtiofsd.sock")

	mgr := newVirtiofsdRuntimeManager(cfg)
	stopCalled := false
	mgr.stopFn = func(pid int, expectedProc string, _ time.Duration) error {
		stopCalled = true
		if pid != 9001 {
			t.Fatalf("pid = %d, want 9001", pid)
		}
		if expectedProc != "my-virtiofsd" {
			t.Fatalf("expectedProc = %q, want my-virtiofsd", expectedProc)
		}
		return nil
	}
	removedSocket := ""
	mgr.removeFn = func(path string) error {
		removedSocket = path
		return nil
	}

	if err := mgr.Stop(vmID, 9001, "my-virtiofsd", socketPath); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !stopCalled {
		t.Fatal("stopFn was not called")
	}
	if removedSocket != socketPath {
		t.Fatalf("remove socket path = %q, want %q", removedSocket, socketPath)
	}
}

func TestWaitForVirtiofsdSocket_DoesNotProbeConnect(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join(t.TempDir(), "virtiofsd.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}
	defer listener.Close()

	accepted := make(chan struct{}, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- struct{}{}
			_ = conn.Close()
		}
	}()

	if err := waitForVirtiofsdSocket(t.Context(), socketPath, os.Getpid(), 500*time.Millisecond); err != nil {
		t.Fatalf("waitForVirtiofsdSocket: %v", err)
	}

	select {
	case <-accepted:
		t.Fatal("waitForVirtiofsdSocket should not actively connect to the socket")
	case <-time.After(150 * time.Millisecond):
	}
}

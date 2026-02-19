package engine

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/types"
	"github.com/CMGS/cocoon/utils"
)

const (
	defaultVirtiofsdReadyTimeout = 5 * time.Second
	defaultVirtiofsdStopTimeout  = 3 * time.Second
)

type (
	virtiofsdResolveBinaryFunc func(file string) (string, error)
	virtiofsdLaunchFunc        func(ctx context.Context, binary string, args []string, logPath string) (int, error)
	virtiofsdWaitReadyFunc     func(ctx context.Context, socketPath string, pid int, timeout time.Duration) error
	virtiofsdStopFunc          func(pid int, expectedProc string, timeout time.Duration) error
	virtiofsdRemoveFunc        func(path string) error
)

type virtiofsdRuntimeInfo struct {
	PID         int
	SocketPath  string
	ProcessName string
}

type virtiofsdRuntimeManager struct {
	cfg             *config.CocoonConfig
	resolveBinaryFn virtiofsdResolveBinaryFunc
	launchFn        virtiofsdLaunchFunc
	waitReadyFn     virtiofsdWaitReadyFunc
	stopFn          virtiofsdStopFunc
	removeFn        virtiofsdRemoveFunc
}

func newVirtiofsdRuntimeManager(cfg *config.CocoonConfig) *virtiofsdRuntimeManager {
	return &virtiofsdRuntimeManager{
		cfg:             cfg,
		resolveBinaryFn: exec.LookPath,
		launchFn:        launchVirtiofsdProcess,
		waitReadyFn:     waitForVirtiofsdSocket,
		stopFn:          stopVirtiofsdProcess,
		removeFn:        os.Remove,
	}
}

func (m *virtiofsdRuntimeManager) Start(ctx context.Context, vmID, sharedDir, socketPath string) (*virtiofsdRuntimeInfo, error) {
	if !overlayRuntimeSupported() {
		return nil, fmt.Errorf("virtiofsd runtime is only supported on linux")
	}

	if strings.TrimSpace(sharedDir) == "" {
		return nil, fmt.Errorf("virtiofsd shared directory must not be empty")
	}
	if strings.TrimSpace(socketPath) == "" {
		socketPath = m.cfg.VMOCIRootfsVirtioFSSocketPath(vmID)
	}

	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil { //nolint:gosec // VM runtime paths are cocoon-managed
		return nil, fmt.Errorf("create virtiofsd socket directory: %w", err)
	}
	if err := m.removeFn(socketPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove stale virtiofsd socket %s: %w", socketPath, err)
	}

	binaryCandidate := strings.TrimSpace(m.cfg.VirtiofsdBinary)
	if binaryCandidate == "" {
		binaryCandidate = types.DefaultVirtiofsdProcess
	}
	resolvedBinary, err := m.resolveBinaryFn(binaryCandidate)
	if err != nil {
		return nil, fmt.Errorf("resolve virtiofsd binary %q: %w", binaryCandidate, err)
	}

	processName := filepath.Base(resolvedBinary)
	if processName == "" || processName == "." {
		processName = types.DefaultVirtiofsdProcess
	}

	args := buildVirtiofsdArgs(sharedDir, socketPath)
	logPath := m.cfg.VMVirtiofsdLogPath(vmID)
	pid, err := m.launchFn(ctx, resolvedBinary, args, logPath)
	if err != nil {
		return nil, fmt.Errorf("launch virtiofsd for %s: %w", vmID, err)
	}

	if err := m.waitReadyFn(ctx, socketPath, pid, defaultVirtiofsdReadyTimeout); err != nil {
		_ = m.stopFn(pid, processName, defaultVirtiofsdStopTimeout)
		_ = m.removeFn(socketPath)
		return nil, fmt.Errorf("wait for virtiofsd socket %s: %w", socketPath, err)
	}

	return &virtiofsdRuntimeInfo{
		PID:         pid,
		SocketPath:  socketPath,
		ProcessName: processName,
	}, nil
}

func (m *virtiofsdRuntimeManager) Stop(vmID string, pid int, expectedProc, socketPath string) error {
	if strings.TrimSpace(expectedProc) == "" {
		expectedProc = (&types.VMMetadataFile{}).VirtiofsdProcessName(m.cfg.VirtiofsdBinary)
	}
	if strings.TrimSpace(socketPath) == "" {
		socketPath = m.cfg.VMOCIRootfsVirtioFSSocketPath(vmID)
	}

	var errs []string
	if err := m.stopFn(pid, expectedProc, defaultVirtiofsdStopTimeout); err != nil {
		errs = append(errs, fmt.Sprintf("stop process: %v", err))
	}
	if err := m.removeFn(socketPath); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Sprintf("remove socket %s: %v", socketPath, err))
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func buildVirtiofsdArgs(sharedDir, socketPath string) []string {
	// Security baseline (docs/04.1 §6.6): keep the rootfs-serving virtiofsd
	// sandboxed via chroot and avoid weakening this default implicitly.
	return []string{
		"--shared-dir", sharedDir,
		"--socket-path", socketPath,
		"--sandbox", "chroot",
	}
}

func launchVirtiofsdProcess(ctx context.Context, binary string, args []string, logPath string) (int, error) {
	cmd := exec.CommandContext(ctx, binary, args...) //nolint:gosec // binary comes from trusted config
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	logFile, logErr := os.Create(logPath) //nolint:gosec // path is cocoon-managed
	if logErr == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}

	if err := cmd.Start(); err != nil {
		if logFile != nil {
			_ = logFile.Close()
		}
		return 0, err
	}

	pid := cmd.Process.Pid
	// Reap in background to avoid zombie processes if virtiofsd exits while
	// the CLI/manager process is still alive.
	go func(f *os.File) {
		_ = cmd.Wait()
		if f != nil {
			_ = f.Close()
		}
	}(logFile)
	return pid, nil
}

// waitForVirtiofsdSocket polls the virtiofsd Unix socket using dial-only
// (no stat-then-dial) to avoid a TOCTOU race where the socket file appears
// in the filesystem before virtiofsd is ready to accept connections.
func waitForVirtiofsdSocket(ctx context.Context, socketPath string, pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for time.Now().Before(deadline) {
		conn, dialErr := net.DialTimeout("unix", socketPath, 100*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			return nil
		}

		if pid > 0 && !utils.IsProcessAlive(pid) {
			return fmt.Errorf("virtiofsd process %d exited before socket became ready", pid)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("context canceled while waiting for virtiofsd socket: %w", ctx.Err())
		case <-ticker.C:
		}
	}
	return fmt.Errorf("timeout waiting for virtiofsd socket after %s", timeout)
}

func stopVirtiofsdProcess(pid int, expectedProc string, timeout time.Duration) error {
	if pid <= 0 {
		return nil
	}
	if strings.TrimSpace(expectedProc) == "" {
		expectedProc = types.DefaultVirtiofsdProcess
	}

	if !utils.ValidateProcess(pid, expectedProc) {
		if utils.IsProcessAlive(pid) {
			return fmt.Errorf("virtiofsd pid %d is not %s (PID reused by another process)", pid, expectedProc)
		}
		return nil
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find virtiofsd process %d: %w", pid, err)
	}

	if err := process.Signal(syscall.SIGTERM); err != nil {
		if !utils.IsProcessAlive(pid) {
			return nil
		}
		return fmt.Errorf("send SIGTERM to virtiofsd pid %d: %w", pid, err)
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !utils.IsProcessAlive(pid) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}

	if err := utils.ForceKillProcess(pid); err != nil {
		return fmt.Errorf("force kill virtiofsd pid %d: %w", pid, err)
	}
	return nil
}

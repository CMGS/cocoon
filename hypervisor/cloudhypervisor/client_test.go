package cloudhypervisor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/CMGS/cocoon/types"
)

// ---------------------------------------------------------------------------
// buildLaunchArgs tests
// ---------------------------------------------------------------------------

// PVH: --kernel on CLI; also sent via REST payload.kernel.
// UEFI: --firmware on CLI; REST API cannot load CLOUDHV.fd.
// Both strategies: VM resource config (cpus, memory, disk, serial, console)
// goes exclusively through REST vm.create — never on CLI.

func TestBuildLaunchArgs_PVH_KernelOnCLI(t *testing.T) {
	cfg := &types.VMConfig{
		BootStrategy: types.BootStrategyPVH,
		FirmwarePath: "/usr/share/firmware/pvh.bin",
		CPUs:         2,
		MemoryMB:     1024,
		OverlayPath:  "/tmp/overlay.qcow2",
		SerialLog:    "/tmp/serial.log",
	}
	args := buildLaunchArgs("/tmp/api.sock", cfg)

	assertContainsFlag(t, args, "--api-socket", "/tmp/api.sock")
	assertContainsFlag(t, args, "--kernel", "/usr/share/firmware/pvh.bin")
	assertNotContainsKey(t, args, "--firmware")
	// No TPM socket → no --tpm flag.
	assertNotContainsKey(t, args, "--tpm")
	// VM resource config goes via REST vm.create, NOT on CLI.
	assertNotContainsKey(t, args, "--cpus")
	assertNotContainsKey(t, args, "--memory")
	assertNotContainsKey(t, args, "--disk")
	assertNotContainsKey(t, args, "--serial")
	assertNotContainsKey(t, args, "--console")
}

func TestBuildLaunchArgs_UEFI_FirmwareOnCLI(t *testing.T) {
	cfg := &types.VMConfig{
		BootStrategy: types.BootStrategyUEFI,
		FirmwarePath: "/usr/share/firmware/CLOUDHV.fd",
		CPUs:         2,
		MemoryMB:     1024,
		OverlayPath:  "/tmp/overlay.qcow2",
		SerialLog:    "/tmp/serial.log",
	}
	args := buildLaunchArgs("/tmp/api.sock", cfg)

	assertContainsFlag(t, args, "--api-socket", "/tmp/api.sock")
	assertContainsFlag(t, args, "--firmware", "/usr/share/firmware/CLOUDHV.fd")
	assertNotContainsKey(t, args, "--kernel")
	// VM resource config goes via REST vm.create, NOT on CLI.
	assertNotContainsKey(t, args, "--cpus")
	assertNotContainsKey(t, args, "--memory")
	assertNotContainsKey(t, args, "--disk")
	assertNotContainsKey(t, args, "--serial")
	assertNotContainsKey(t, args, "--console")
}

func TestBuildLaunchArgs_PVH_WithTPM(t *testing.T) {
	cfg := &types.VMConfig{
		BootStrategy:  types.BootStrategyPVH,
		FirmwarePath:  "/usr/share/firmware/pvh.bin",
		CPUs:          2,
		MemoryMB:      1024,
		OverlayPath:   "/tmp/overlay.qcow2",
		SerialLog:     "/tmp/serial.log",
		TPMSocketPath: "/run/cocoon/vms/vm-abc/swtpm.sock",
	}
	args := buildLaunchArgs("/tmp/api.sock", cfg)

	assertContainsFlag(t, args, "--kernel", "/usr/share/firmware/pvh.bin")
	assertContainsFlag(t, args, "--tpm", "socket=/run/cocoon/vms/vm-abc/swtpm.sock")
	assertNotContainsKey(t, args, "--firmware")
}

func TestBuildLaunchArgs_UEFI_WithTPM(t *testing.T) {
	cfg := &types.VMConfig{
		BootStrategy:  types.BootStrategyUEFI,
		FirmwarePath:  "/usr/share/firmware/CLOUDHV.fd",
		CPUs:          2,
		MemoryMB:      1024,
		OverlayPath:   "/tmp/overlay.qcow2",
		SerialLog:     "/tmp/serial.log",
		TPMSocketPath: "/run/cocoon/vms/vm-abc/swtpm.sock",
	}
	args := buildLaunchArgs("/tmp/api.sock", cfg)

	// UEFI flags present.
	assertContainsFlag(t, args, "--firmware", "/usr/share/firmware/CLOUDHV.fd")
	// TPM flag also present.
	assertContainsFlag(t, args, "--tpm", "socket=/run/cocoon/vms/vm-abc/swtpm.sock")
}

func TestBuildLaunchArgs_SocketPathAlwaysPresent(t *testing.T) {
	strategies := []types.BootStrategy{
		types.BootStrategyPVH,
		types.BootStrategyUEFI,
	}

	for _, strategy := range strategies {
		t.Run(string(strategy), func(t *testing.T) {
			cfg := &types.VMConfig{
				BootStrategy: strategy,
				FirmwarePath: "/some/path",
				CPUs:         1,
				MemoryMB:     512,
				OverlayPath:  "/tmp/overlay.qcow2",
				SerialLog:    "/tmp/serial.log",
			}
			args := buildLaunchArgs("/run/cocoon/vms/abc/api.sock", cfg)
			assertContainsFlag(t, args, "--api-socket", "/run/cocoon/vms/abc/api.sock")
		})
	}
}

// ---------------------------------------------------------------------------
// isRetryable tests
// ---------------------------------------------------------------------------

func TestIsRetryable_APIError503(t *testing.T) {
	err := &apiError{StatusCode: 503, Message: "service unavailable"}
	if !isRetryable(err) {
		t.Error("expected HTTP 503 to be retryable")
	}
}

func TestIsRetryable_APIError500(t *testing.T) {
	err := &apiError{StatusCode: 500, Message: "internal server error"}
	if !isRetryable(err) {
		t.Error("expected HTTP 500 to be retryable")
	}
}

func TestIsVMAlreadyCreatedError(t *testing.T) {
	err := &apiError{
		StatusCode: 500,
		Message:    `PUT /api/v1/vm.create returned 500: ["Error from API","The VM could not be created","VM is already created"]`,
	}
	if !isVMAlreadyCreatedError(err) {
		t.Fatal("expected vm.create 'already created' error to be detected")
	}
}

func TestIsVMAlreadyCreatedError_FalseForOtherErrors(t *testing.T) {
	tests := []error{
		&apiError{StatusCode: 500, Message: "internal server error"},
		&apiError{StatusCode: 400, Message: "bad request"},
		errors.New("some other error"),
	}
	for _, err := range tests {
		if isVMAlreadyCreatedError(err) {
			t.Fatalf("expected false for error: %v", err)
		}
	}
}

func TestIsVMAlreadyBootedError(t *testing.T) {
	err := &apiError{
		StatusCode: 500,
		Message:    `PUT /api/v1/vm.boot returned 500: ["Error from API","The VM could not boot","invalid VM state transition: Running to Running"]`,
	}
	if !isVMAlreadyBootedError(err) {
		t.Fatal("expected vm.boot 'already running' error to be detected")
	}
}

func TestIsVMAlreadyBootedError_FalseForOtherErrors(t *testing.T) {
	tests := []error{
		&apiError{StatusCode: 500, Message: "internal server error"},
		&apiError{StatusCode: 400, Message: "bad request"},
		errors.New("some other error"),
	}
	for _, err := range tests {
		if isVMAlreadyBootedError(err) {
			t.Fatalf("expected false for error: %v", err)
		}
	}
}

func TestIsRetryable_APIError429(t *testing.T) {
	err := &apiError{StatusCode: 429, Message: "rate limited"}
	if !isRetryable(err) {
		t.Error("expected HTTP 429 to be retryable")
	}
}

func TestIsRetryable_APIError400(t *testing.T) {
	err := &apiError{StatusCode: 400, Message: "bad request"}
	if isRetryable(err) {
		t.Error("expected HTTP 400 to NOT be retryable")
	}
}

func TestIsRetryable_APIError404(t *testing.T) {
	err := &apiError{StatusCode: 404, Message: "not found"}
	if isRetryable(err) {
		t.Error("expected HTTP 404 to NOT be retryable")
	}
}

func TestIsRetryable_ContextCanceled(t *testing.T) {
	if isRetryable(context.Canceled) {
		t.Error("expected context.Canceled to NOT be retryable")
	}
}

func TestIsRetryable_ContextDeadlineExceeded(t *testing.T) {
	if isRetryable(context.DeadlineExceeded) {
		t.Error("expected context.DeadlineExceeded to NOT be retryable")
	}
}

func TestIsRetryable_NetOpError(t *testing.T) {
	opErr := &net.OpError{
		Op:  "dial",
		Net: "unix",
		Err: errors.New("connection refused"),
	}
	if !isRetryable(opErr) {
		t.Error("expected net.OpError (connection refused) to be retryable")
	}
}

func TestIsRetryable_Nil(t *testing.T) {
	if isRetryable(nil) {
		t.Error("expected nil error to NOT be retryable")
	}
}

func TestIsRetryable_WrappedConnectionRefused(t *testing.T) {
	inner := errors.New("connection refused")
	wrapped := fmt.Errorf("dial unix /tmp/api.sock: %w", inner)
	if !isRetryable(wrapped) {
		t.Error("expected wrapped 'connection refused' error to be retryable")
	}
}

// ---------------------------------------------------------------------------
// doWithRetry tests
// ---------------------------------------------------------------------------

func newTestClient() *client {
	return &client{
		maxRetries:  3,
		baseBackoff: 1 * time.Millisecond,
	}
}

func TestDoWithRetry_SucceedsFirstTry(t *testing.T) {
	c := newTestClient()
	calls := 0
	err := c.doWithRetry(t.Context(), func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call, got %d", calls)
	}
}

func TestDoWithRetry_RetriesTransientThenSucceeds(t *testing.T) {
	c := newTestClient()
	calls := 0
	err := c.doWithRetry(t.Context(), func() error {
		calls++
		if calls <= 2 {
			return &apiError{StatusCode: 503, Message: "unavailable"}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected nil error after retries, got: %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 calls (2 failures + 1 success), got %d", calls)
	}
}

func TestDoWithRetry_NonRetryableReturnsImmediately(t *testing.T) {
	c := newTestClient()
	calls := 0
	err := c.doWithRetry(t.Context(), func() error {
		calls++
		return &apiError{StatusCode: 400, Message: "bad request"}
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if calls != 1 {
		t.Fatalf("expected 1 call (no retry for non-retryable), got %d", calls)
	}
}

func TestDoWithRetry_ExhaustsRetries(t *testing.T) {
	c := newTestClient()
	calls := 0
	err := c.doWithRetry(t.Context(), func() error {
		calls++
		return &apiError{StatusCode: 503, Message: "unavailable"}
	})
	if err == nil {
		t.Fatal("expected error after exhausting retries, got nil")
	}
	// maxRetries=3 means 1 initial + 3 retries = 4 total calls.
	expectedCalls := c.maxRetries + 1
	if calls != expectedCalls {
		t.Fatalf("expected %d calls, got %d", expectedCalls, calls)
	}
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// assertContainsFlag checks that args contains "--key value" consecutively.
func assertContainsFlag(t *testing.T, args []string, key, value string) {
	t.Helper()
	for i := 0; i < len(args)-1; i++ {
		if args[i] == key && args[i+1] == value {
			return
		}
	}
	t.Errorf("expected args to contain %s %s, got %v", key, value, args)
}

// assertNotContainsKey checks that args does not contain the given key.
func assertNotContainsKey(t *testing.T, args []string, key string) {
	t.Helper()
	if slices.Contains(args, key) {
		t.Errorf("expected args to NOT contain %s, got %v", key, args)
	}
}


package cloudhypervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/CMGS/cocoon/hypervisor"
)

// ---------------------------------------------------------------------------
// buildLaunchArgs tests
// ---------------------------------------------------------------------------

// CLI only carries --api-socket. Everything else (firmware, cpus, memory,
// disk, serial, console, tpm) goes via REST vm.create payload.

func TestBuildLaunchArgs_OnlyAPISocket(t *testing.T) {
	args := buildLaunchArgs("/tmp/api.sock")

	assertContainsFlag(t, args, "--api-socket", "/tmp/api.sock")
	if len(args) != 2 {
		t.Fatalf("expected exactly 2 args (--api-socket + path), got %v", args)
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
// GetConsolePTYPath tests
// ---------------------------------------------------------------------------

// startFakeVMInfoServer creates an HTTP server that listens on a Unix socket
// and serves GET /api/v1/vm.info with the provided handler. It returns the
// socket path. The server is automatically cleaned up when the test finishes.
//
// We use /tmp for the socket because macOS enforces a 104-byte limit on Unix
// socket paths and t.TempDir() paths often exceed that.
func startFakeVMInfoServer(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()

	dir, err := os.MkdirTemp("/tmp", "cocoon-test-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	socketPath := filepath.Join(dir, "api.sock")

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on unix socket %s: %v", socketPath, err)
	}

	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })

	return socketPath
}

// vmInfoJSON builds a minimal JSON response for GET /api/v1/vm.info with the
// given console mode and file.
func vmInfoJSON(consoleMode, consoleFile string) []byte {
	info := hypervisor.CHVMInfo{
		Config: hypervisor.CHVMConfig{
			CPUs:   hypervisor.CHCPUConfig{BootVCPUs: 1, MaxVCPUs: 1},
			Memory: hypervisor.CHMemoryConfig{Size: 512 * 1024 * 1024},
			Serial: hypervisor.CHSerialConfig{Mode: "Null"},
			Console: hypervisor.CHConsoleConfig{
				Mode: consoleMode,
				File: consoleFile,
			},
		},
		State: "Running",
	}
	data, _ := json.Marshal(info)
	return data
}

func TestGetConsolePTYPath_PtyModeWithFile(t *testing.T) {
	wantPath := "/dev/pts/3"

	socketPath := startFakeVMInfoServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/vm.info" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(vmInfoJSON("Pty", wantPath))
	})

	c := &client{
		httpTimeout: 5 * time.Second,
		maxRetries:  0,
		baseBackoff: 1 * time.Millisecond,
	}

	got, err := c.GetConsolePTYPath(t.Context(), socketPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != wantPath {
		t.Fatalf("expected PTY path %q, got %q", wantPath, got)
	}
}

func TestGetConsolePTYPath_OffMode(t *testing.T) {
	socketPath := startFakeVMInfoServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/vm.info" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(vmInfoJSON("Off", ""))
	})

	c := &client{
		httpTimeout: 5 * time.Second,
		maxRetries:  0,
		baseBackoff: 1 * time.Millisecond,
	}

	got, err := c.GetConsolePTYPath(t.Context(), socketPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("expected empty string for Off mode, got %q", got)
	}
}

func TestGetConsolePTYPath_PtyModeEmptyFile(t *testing.T) {
	socketPath := startFakeVMInfoServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/vm.info" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Console mode is "Pty" but CH has not yet allocated a PTY, so File is empty.
		_, _ = w.Write(vmInfoJSON("Pty", ""))
	})

	c := &client{
		httpTimeout: 5 * time.Second,
		maxRetries:  0,
		baseBackoff: 1 * time.Millisecond,
	}

	got, err := c.GetConsolePTYPath(t.Context(), socketPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("expected empty string when Pty mode has no file yet, got %q", got)
	}
}

func TestGetConsolePTYPath_GetVMInfoError(t *testing.T) {
	// Point at a non-existent socket so GetVMInfo fails with a connection error.
	dir := t.TempDir()
	badSocket := filepath.Join(dir, "nonexistent.sock")

	// Ensure the socket file does not exist.
	_ = os.Remove(badSocket)

	c := &client{
		httpTimeout: 2 * time.Second,
		maxRetries:  0,
		baseBackoff: 1 * time.Millisecond,
	}

	got, err := c.GetConsolePTYPath(t.Context(), badSocket)
	if err == nil {
		t.Fatal("expected error when socket does not exist, got nil")
	}
	if got != "" {
		t.Fatalf("expected empty string on error, got %q", got)
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

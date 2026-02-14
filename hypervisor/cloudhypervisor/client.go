package cloudhypervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/hypervisor"
	"github.com/CMGS/cocoon/types"
	"github.com/CMGS/cocoon/utils"
)

// Compile-time interface check.
var _ hypervisor.Client = (*client)(nil)

// Default retry parameters for CH REST API calls.
const (
	defaultMaxRetries  = 3
	defaultBaseBackoff = 100 * time.Millisecond
)

// client implements the hypervisor.Client interface using HTTP over Unix socket
// for the CH REST API and os/exec for process management.
type client struct {
	cfg *config.CocoonConfig

	// httpClient is configured with a Unix-socket transport.
	// The socketPath is set per-request via the helper methods, so this
	// client uses a default dialer; per-socket clients are created on
	// demand by newHTTPClient.
	httpTimeout time.Duration

	// Retry configuration for transient CH REST API errors.
	maxRetries  int
	baseBackoff time.Duration
}

// New creates a new hypervisor client backed by the given Cocoon config.
func New(cfg *config.CocoonConfig) hypervisor.Client {
	return &client{
		cfg:         cfg,
		httpTimeout: 30 * time.Second,
		maxRetries:  defaultMaxRetries,
		baseBackoff: defaultBaseBackoff,
	}
}

// ---------------------------------------------------------------------------
// Process management
// ---------------------------------------------------------------------------

// buildLaunchArgs constructs the Cloud Hypervisor CLI arguments for a given
// VM configuration. It selects the correct firmware flag based on the boot
// strategy: --kernel for UEFI, --firmware for PVH (the default).
func buildLaunchArgs(socketPath string, _ *types.VMConfig) []string {
	// Launch CH in pure daemon mode with only the API socket.
	// All VM configuration (firmware, cpus, memory, disks, etc.) is sent
	// via the PUT /api/v1/vm.create REST call. Passing --firmware or
	// --kernel on the CLI causes newer CH versions (v38+) to auto-create
	// a VM, which then conflicts with the subsequent vm.create API call.
	return []string{
		"--api-socket", socketPath,
	}
}

// Launch starts a Cloud Hypervisor process for the given VM.
func (c *client) Launch(ctx context.Context, vmID string, cfg *types.VMConfig) (int, error) {
	// Ensure runtime directory exists.
	runtimeDir := c.cfg.VMRuntimeDir(vmID)
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil { //nolint:gosec // G301: VM runtime dir needs to be world-readable for CH process
		return 0, fmt.Errorf("create runtime dir %s: %w", runtimeDir, err)
	}

	socketPath := c.cfg.VMSocketPath(vmID)

	// Best-effort cleanup of stale runtime files from a previous crash.
	// Prevents "Address already in use" if a stale socket remains.
	_ = os.Remove(socketPath)
	_ = os.Remove(c.cfg.VMPIDPath(vmID))

	// Build CH command-line arguments.
	// Only pass --api-socket and firmware flag on the CLI. All VM resource
	// configuration (cpus, memory, disks, serial, console) is sent via the
	// PUT /api/v1/vm.create REST call so there is no conflict between CLI
	// flags and the REST API.
	args := buildLaunchArgs(socketPath, cfg)

	cmd := exec.CommandContext(ctx, c.cfg.CHBinary, args...) //nolint:gosec // CHBinary is a trusted config value, not user input

	// Detach the CH process from the parent process group so it survives
	// if cocoon exits unexpectedly.
	configureCHProcess(cmd)

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start cloud-hypervisor: %w", err)
	}

	pid := cmd.Process.Pid

	// Write PID file.
	pidPath := c.cfg.VMPIDPath(vmID)
	if err := utils.WritePIDFile(pidPath, pid); err != nil {
		// Best-effort kill; the process was already started.
		_ = cmd.Process.Kill()
		return 0, fmt.Errorf("write PID file %s: %w", pidPath, err)
	}

	// Wait for the API socket to appear so callers can immediately issue
	// REST requests after Launch returns.
	if err := c.WaitForSocket(ctx, socketPath, 5*time.Second); err != nil {
		// Socket never appeared; kill the process.
		_ = cmd.Process.Kill()
		return 0, fmt.Errorf("wait for socket %s: %w", socketPath, err)
	}

	// Release the OS process handle. The CH process is intentionally
	// orphaned -- it outlives the cocoon CLI invocation. Process.Release()
	// detaches Go's handle so the process is not waited on. This avoids
	// leaking a goroutine per Launch() call.
	_ = cmd.Process.Release()

	return pid, nil
}

// Shutdown performs a graceful shutdown of the VM, falling back to SIGKILL.
func (c *client) Shutdown(ctx context.Context, vmID string, timeout time.Duration) error {
	socketPath := c.cfg.VMSocketPath(vmID)

	// Step 1: send ACPI power-button event.
	if err := c.PowerButton(ctx, socketPath); err != nil {
		return fmt.Errorf("power-button for %s: %w", vmID, err)
	}

	// Step 2: poll until the process exits or the timeout fires.
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("shutdown canceled for %s: %w", vmID, ctx.Err())
		case <-ticker.C:
			if !c.IsAlive(vmID) {
				c.cleanupRuntimeFiles(vmID)
				return nil
			}
			if time.Now().After(deadline) {
				// Timeout reached; force kill.
				return c.ForceKill(vmID)
			}
		}
	}
}

// ForceKill sends SIGKILL to the CH process for the given VM.
// It validates the PID identity before killing to prevent sending signals
// to an unrelated process if the PID was reused by the OS.
func (c *client) ForceKill(vmID string) error {
	pidPath := c.cfg.VMPIDPath(vmID)
	pid, err := utils.ReadPIDFile(pidPath)
	if err != nil {
		return fmt.Errorf("read PID for %s: %w", vmID, err)
	}
	if !utils.ValidateProcess(pid, "cloud-hypervisor") {
		if utils.IsProcessAlive(pid) {
			// PID exists but name doesn't match — genuinely reused by another
			// process. Don't kill, but preserve PID file for diagnostics.
			return fmt.Errorf("PID %d for %s is not cloud-hypervisor (PID reused by another process)", pid, vmID)
		}
		// Process is gone. Clean up stale runtime files.
		c.cleanupRuntimeFiles(vmID)
		return nil
	}
	if err := utils.ForceKillProcess(pid); err != nil {
		return fmt.Errorf("force kill %s (pid %d): %w", vmID, pid, err)
	}
	c.cleanupRuntimeFiles(vmID)
	return nil
}

// IsAlive returns true if the CH process for the VM is still running.
// It validates the process name to guard against PID reuse.
func (c *client) IsAlive(vmID string) bool {
	pidPath := c.cfg.VMPIDPath(vmID)
	pid, err := utils.ReadPIDFile(pidPath)
	if err != nil {
		return false
	}
	return utils.ValidateProcess(pid, "cloud-hypervisor")
}

// cleanupRuntimeFiles removes the PID file and API socket for a VM.
// Best-effort: errors are ignored since files may already be gone.
func (c *client) cleanupRuntimeFiles(vmID string) {
	_ = os.Remove(c.cfg.VMPIDPath(vmID))
	_ = os.Remove(c.cfg.VMSocketPath(vmID))
}

// ---------------------------------------------------------------------------
// CH REST API
// ---------------------------------------------------------------------------

// CreateVM sends PUT /api/v1/vm.create.
func (c *client) CreateVM(ctx context.Context, socketPath string, vmCfg *hypervisor.CHVMConfig) error {
	body, err := json.Marshal(vmCfg)
	if err != nil {
		return fmt.Errorf("marshal vm config: %w", err)
	}
	return c.doWithRetry(ctx, func() error {
		err := c.doPUT(ctx, socketPath, "/api/v1/vm.create", body)
		if isVMAlreadyCreatedError(err) {
			log.Printf("CH API reported VM already created for %s; treating vm.create as idempotent success", socketPath)
			return nil
		}
		return err
	})
}

// BootVM sends PUT /api/v1/vm.boot.
func (c *client) BootVM(ctx context.Context, socketPath string) error {
	return c.doWithRetry(ctx, func() error {
		return c.doPUT(ctx, socketPath, "/api/v1/vm.boot", nil)
	})
}

// ShutdownVM sends PUT /api/v1/vm.shutdown.
func (c *client) ShutdownVM(ctx context.Context, socketPath string) error {
	return c.doWithRetry(ctx, func() error {
		return c.doPUT(ctx, socketPath, "/api/v1/vm.shutdown", nil)
	})
}

// PowerButton sends PUT /api/v1/vm.power-button.
func (c *client) PowerButton(ctx context.Context, socketPath string) error {
	return c.doWithRetry(ctx, func() error {
		return c.doPUT(ctx, socketPath, "/api/v1/vm.power-button", nil)
	})
}

// DeleteVM sends PUT /api/v1/vm.delete.
func (c *client) DeleteVM(ctx context.Context, socketPath string) error {
	return c.doWithRetry(ctx, func() error {
		return c.doPUT(ctx, socketPath, "/api/v1/vm.delete", nil)
	})
}

// GetVMInfo sends GET /api/v1/vm.info and decodes the JSON response.
func (c *client) GetVMInfo(ctx context.Context, socketPath string) (*hypervisor.CHVMInfo, error) {
	var info *hypervisor.CHVMInfo
	err := c.doWithRetry(ctx, func() error {
		var innerErr error
		info, innerErr = c.doGetVMInfo(ctx, socketPath)
		return innerErr
	})
	return info, err
}

// doGetVMInfo is the single-attempt implementation of GetVMInfo.
func (c *client) doGetVMInfo(ctx context.Context, socketPath string) (*hypervisor.CHVMInfo, error) {
	hc := c.newHTTPClient(socketPath)

	url := "http://localhost/api/v1/vm.info"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, &apiError{
			StatusCode: resp.StatusCode,
			Message:    fmt.Sprintf("GET %s returned %d: %s", url, resp.StatusCode, string(respBody)),
		}
	}

	var info hypervisor.CHVMInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("decode vm.info response: %w", err)
	}
	return &info, nil
}

// ---------------------------------------------------------------------------
// Retry logic
// ---------------------------------------------------------------------------

// apiError represents an HTTP error response from the CH REST API.
// It carries the status code so the retry logic can classify it.
type apiError struct {
	StatusCode int
	Message    string
}

func (e *apiError) Error() string { return e.Message }

// isVMAlreadyCreatedError checks whether CH returned the known "VM is already
// created" API error. Some CH versions may report this on vm.create even when
// the VM is effectively in CREATED state.
func isVMAlreadyCreatedError(err error) bool {
	if err == nil {
		return false
	}
	var ae *apiError
	if !errors.As(err, &ae) {
		return false
	}
	if ae.StatusCode != http.StatusInternalServerError {
		return false
	}
	return strings.Contains(strings.ToLower(ae.Message), "vm is already created")
}

// isRetryable determines whether an error is transient and should be retried.
// Retryable: connection refused, connection reset, HTTP 5xx, HTTP 429.
// Not retryable: HTTP 4xx (except 429), context.Canceled, context.DeadlineExceeded.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}

	// Never retry on context cancellation or overall deadline exceeded.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	// Check for API errors with HTTP status codes.
	var ae *apiError
	if errors.As(err, &ae) {
		switch {
		case ae.StatusCode == http.StatusTooManyRequests: // 429
			return true
		case ae.StatusCode >= 400 && ae.StatusCode < 500:
			return false // client errors are permanent
		case ae.StatusCode >= 500:
			return true // server errors are transient
		}
		return false
	}

	// Connection refused or connection reset (CH not yet accepting on socket
	// or dropped the connection). Only retry on these specific transient
	// conditions, not all net.OpError.
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if opErr.Op == "dial" || opErr.Op == "read" || opErr.Op == "write" {
			errMsg := opErr.Err.Error()
			if strings.Contains(errMsg, "connection refused") ||
				strings.Contains(errMsg, "connection reset") {
				return true
			}
		}
		return false
	}

	// Check for common transient error strings (connection refused/reset
	// wrapped in generic fmt.Errorf by the http client).
	errMsg := err.Error()
	if strings.Contains(errMsg, "connection refused") ||
		strings.Contains(errMsg, "connection reset") {
		return true
	}

	return false
}

// doWithRetry executes fn with exponential backoff retry for transient errors.
// It uses the client's configured maxRetries and baseBackoff.
func (c *client) doWithRetry(ctx context.Context, fn func() error) error {
	var lastErr error
	backoff := c.baseBackoff

	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		lastErr = fn()
		if lastErr == nil {
			return nil
		}

		// Do not retry non-retryable errors.
		if !isRetryable(lastErr) {
			return lastErr
		}

		// Do not retry if this was the last attempt.
		if attempt == c.maxRetries {
			break
		}

		// Log the retry attempt.
		log.Printf("CH API transient error (attempt %d/%d): %v; retrying in %s",
			attempt+1, c.maxRetries+1, lastErr, backoff)

		// Wait with jitter: backoff +/- 25%, floored at baseBackoff/4 to
		// prevent negative or near-zero sleep durations.
		jitter := time.Duration(rand.Int64N(int64(backoff/2))) - backoff/4 //nolint:gosec // G404: jitter does not need cryptographic randomness
		wait := backoff + jitter
		if minBackoff := c.baseBackoff / 4; wait < minBackoff {
			wait = minBackoff
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("retry canceled: %w", ctx.Err())
		case <-time.After(wait):
		}

		// Exponential backoff: 100ms -> 200ms -> 400ms.
		backoff *= 2
	}

	return fmt.Errorf("after %d retries: %w", c.maxRetries, lastErr)
}

// ---------------------------------------------------------------------------
// Utilities
// ---------------------------------------------------------------------------

// WaitForSocket blocks until the socket at socketPath exists and is connectable,
// or the timeout/context expires.
func (c *client) WaitForSocket(ctx context.Context, socketPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("context canceled while waiting for socket %s: %w", socketPath, ctx.Err())
		case <-ticker.C:
			if time.Now().After(deadline) {
				return fmt.Errorf("timeout waiting for socket %s after %s", socketPath, timeout)
			}
			// Check if the socket file exists.
			if _, err := os.Stat(socketPath); err != nil {
				continue
			}
			// Attempt a TCP-style dial to confirm CH is accepting connections.
			if err := c.CheckSocketConnectivity(socketPath); err == nil {
				return nil
			}
		}
	}
}

// CheckSocketConnectivity dials the Unix socket and immediately closes the
// connection. Returns nil if the socket is reachable.
func (c *client) CheckSocketConnectivity(socketPath string) error {
	conn, err := net.DialTimeout("unix", socketPath, 2*time.Second)
	if err != nil {
		return fmt.Errorf("socket %s not accessible: %w", socketPath, err)
	}
	_ = conn.Close()
	return nil
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// newHTTPClient returns an *http.Client that dials the given Unix socket.
func (c *client) newHTTPClient(socketPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
		Timeout: c.httpTimeout,
	}
}

// doPUT is a helper for PUT requests that expect 204 No Content on success.
func (c *client) doPUT(ctx context.Context, socketPath, path string, body []byte) error {
	hc := c.newHTTPClient(socketPath)

	url := "http://localhost" + path

	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, reqBody)
	if err != nil {
		return fmt.Errorf("create request for %s: %w", path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("PUT %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNoContent {
		respBody, _ := io.ReadAll(resp.Body)
		return &apiError{
			StatusCode: resp.StatusCode,
			Message:    fmt.Sprintf("PUT %s returned %d: %s", path, resp.StatusCode, string(respBody)),
		}
	}

	return nil
}

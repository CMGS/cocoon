package hypervisor

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
	"github.com/CMGS/cocoon/types"
	"github.com/CMGS/cocoon/utils"
)

// Compile-time interface check.
var _ Client = (*client)(nil)

// Default retry parameters for CH REST API calls.
const (
	defaultMaxRetries  = 3
	defaultBaseBackoff = 100 * time.Millisecond
)

// client implements the Client interface using HTTP over Unix socket for the
// CH REST API and os/exec for process management.
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

// NewClient creates a new hypervisor client backed by the given Cocoon config.
func NewClient(cfg *config.CocoonConfig) Client {
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
func buildLaunchArgs(socketPath string, cfg *types.VMConfig) []string {
	args := []string{
		"--api-socket", socketPath,
	}

	firmwarePath := cfg.FirmwarePath
	if firmwarePath != "" {
		switch cfg.BootStrategy {
		case types.BootStrategyUEFIOnly:
			// Cloud Hypervisor uses --kernel for UEFI firmware (CLOUDHV.fd).
			args = append(args, "--kernel", firmwarePath)
		default:
			// PVH and pvh_then_uefi both use --firmware for the initial attempt.
			args = append(args, "--firmware", firmwarePath)
		}
	}

	return args
}

// Launch starts a Cloud Hypervisor process for the given VM.
func (c *client) Launch(ctx context.Context, vmID string, cfg *types.VMConfig) (int, error) {
	// Ensure runtime directory exists.
	runtimeDir := c.cfg.VMRuntimeDir(vmID)
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil { //nolint:gosec // G301: VM runtime dir needs to be world-readable for CH process
		return 0, fmt.Errorf("create runtime dir %s: %w", runtimeDir, err)
	}

	socketPath := c.cfg.VMSocketPath(vmID)

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
func (c *client) ForceKill(vmID string) error {
	pidPath := c.cfg.VMPIDPath(vmID)
	pid, err := utils.ReadPIDFile(pidPath)
	if err != nil {
		return fmt.Errorf("read PID for %s: %w", vmID, err)
	}
	if err := utils.ForceKillProcess(pid); err != nil {
		return fmt.Errorf("force kill %s (pid %d): %w", vmID, pid, err)
	}
	return nil
}

// IsAlive returns true if the CH process for the VM is still running.
func (c *client) IsAlive(vmID string) bool {
	pidPath := c.cfg.VMPIDPath(vmID)
	pid, err := utils.ReadPIDFile(pidPath)
	if err != nil {
		return false
	}
	return utils.IsProcessAlive(pid)
}

// ---------------------------------------------------------------------------
// CH REST API
// ---------------------------------------------------------------------------

// CreateVM sends PUT /api/v1/vm.create.
func (c *client) CreateVM(ctx context.Context, socketPath string, vmCfg *CHVMConfig) error {
	body, err := json.Marshal(vmCfg)
	if err != nil {
		return fmt.Errorf("marshal vm config: %w", err)
	}
	return c.doWithRetry(ctx, func() error {
		return c.doPUT(ctx, socketPath, "/api/v1/vm.create", body)
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
func (c *client) GetVMInfo(ctx context.Context, socketPath string) (*CHVMInfo, error) {
	var info *CHVMInfo
	err := c.doWithRetry(ctx, func() error {
		var innerErr error
		info, innerErr = c.doGetVMInfo(ctx, socketPath)
		return innerErr
	})
	return info, err
}

// doGetVMInfo is the single-attempt implementation of GetVMInfo.
func (c *client) doGetVMInfo(ctx context.Context, socketPath string) (*CHVMInfo, error) {
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

	var info CHVMInfo
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

// isRetryable determines whether an error is transient and should be retried.
// Retryable: connection refused, HTTP 500, HTTP 503, net timeout.
// Not retryable: HTTP 4xx (except 429), context.Canceled.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}

	// Never retry on context cancellation.
	if errors.Is(err, context.Canceled) {
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
		case ae.StatusCode == http.StatusInternalServerError,
			ae.StatusCode == http.StatusServiceUnavailable:
			return true
		}
		return false
	}

	// Connection refused (CH not yet accepting on socket).
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}

	// Check for net timeout.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	// context.DeadlineExceeded is transient (per-request timeout, not user cancel).
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	// Check for common transient error strings (connection refused wrapped
	// in generic fmt.Errorf by the http client).
	if strings.Contains(err.Error(), "connection refused") {
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

		// Wait with jitter: backoff +/- 25%.
		jitter := time.Duration(rand.Int64N(int64(backoff/2))) - backoff/4 //nolint:gosec // G404: jitter does not need cryptographic randomness
		select {
		case <-ctx.Done():
			return fmt.Errorf("retry canceled: %w", ctx.Err())
		case <-time.After(backoff + jitter):
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

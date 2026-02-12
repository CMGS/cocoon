package hypervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/types"
	"github.com/CMGS/cocoon/utils"
)

// Compile-time interface check.
var _ Client = (*client)(nil)

// client implements the Client interface using HTTP over Unix socket for the
// CH REST API and os/exec for process management.
type client struct {
	cfg *config.CocoonConfig

	// httpClient is configured with a Unix-socket transport.
	// The socketPath is set per-request via the helper methods, so this
	// client uses a default dialer; per-socket clients are created on
	// demand by newHTTPClient.
	httpTimeout time.Duration
}

// NewClient creates a new hypervisor client backed by the given Cocoon config.
func NewClient(cfg *config.CocoonConfig) Client {
	return &client{
		cfg:         cfg,
		httpTimeout: 30 * time.Second,
	}
}

// ---------------------------------------------------------------------------
// Process management
// ---------------------------------------------------------------------------

// Launch starts a Cloud Hypervisor process for the given VM.
func (c *client) Launch(ctx context.Context, vmID string, cfg *types.VMConfig) (int, error) {
	// Ensure runtime directory exists.
	runtimeDir := c.cfg.VMRuntimeDir(vmID)
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil { //nolint:gosec // G301: VM runtime dir needs to be world-readable for CH process
		return 0, fmt.Errorf("create runtime dir %s: %w", runtimeDir, err)
	}

	socketPath := c.cfg.VMSocketPath(vmID)

	// Build CH command-line arguments.
	// Only pass --api-socket and --firmware on the CLI. All VM resource
	// configuration (cpus, memory, disks, serial, console) is sent via the
	// PUT /api/v1/vm.create REST call so there is no conflict between CLI
	// flags and the REST API.
	args := []string{
		"--api-socket", socketPath,
	}

	// Add firmware flag based on boot strategy.
	firmwarePath := cfg.FirmwarePath
	if firmwarePath != "" {
		args = append(args, "--firmware", firmwarePath)
	}

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

	// Release the exec.Cmd's process handle; the CH process is intentionally
	// orphaned (it outlives the cocoon CLI invocation).
	// TODO: implement proper background-process release.
	go func() { _ = cmd.Wait() }()

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
	return c.doPUT(ctx, socketPath, "/api/v1/vm.create", body)
}

// BootVM sends PUT /api/v1/vm.boot.
func (c *client) BootVM(ctx context.Context, socketPath string) error {
	return c.doPUT(ctx, socketPath, "/api/v1/vm.boot", nil)
}

// ShutdownVM sends PUT /api/v1/vm.shutdown.
func (c *client) ShutdownVM(ctx context.Context, socketPath string) error {
	return c.doPUT(ctx, socketPath, "/api/v1/vm.shutdown", nil)
}

// PowerButton sends PUT /api/v1/vm.power-button.
func (c *client) PowerButton(ctx context.Context, socketPath string) error {
	return c.doPUT(ctx, socketPath, "/api/v1/vm.power-button", nil)
}

// DeleteVM sends PUT /api/v1/vm.delete.
func (c *client) DeleteVM(ctx context.Context, socketPath string) error {
	return c.doPUT(ctx, socketPath, "/api/v1/vm.delete", nil)
}

// GetVMInfo sends GET /api/v1/vm.info and decodes the JSON response.
func (c *client) GetVMInfo(ctx context.Context, socketPath string) (*CHVMInfo, error) {
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
		return nil, fmt.Errorf("GET %s returned %d: %s", url, resp.StatusCode, string(respBody))
	}

	var info CHVMInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("decode vm.info response: %w", err)
	}
	return &info, nil
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
		return fmt.Errorf("PUT %s returned %d: %s", path, resp.StatusCode, string(respBody))
	}

	return nil
}

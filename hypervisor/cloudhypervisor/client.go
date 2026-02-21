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
	"path/filepath"
	"strings"
	"syscall"
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

// buildLaunchArgs constructs the Cloud Hypervisor CLI arguments.
//
// The CLI only carries --api-socket. All VM configuration (firmware,
// cpus, memory, disk, serial, console, tpm) goes through the REST
// vm.create payload.
func buildLaunchArgs(socketPath string) []string {
	return []string{"--api-socket", socketPath}
}

// Launch starts a Cloud Hypervisor process for the given VM.
func (c *client) Launch(ctx context.Context, vmID string, cfg *types.VMConfig) (int, error) {
	// Ensure runtime directory exists.
	runtimeDir := c.cfg.VMRuntimeDir(vmID)
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil { //nolint:gosec // G301: restricted to owner — VM runtime state
		return 0, fmt.Errorf("create runtime dir %s: %w", runtimeDir, err)
	}

	socketPath := c.cfg.VMSocketPath(vmID)

	// Best-effort cleanup of stale runtime files from a previous crash.
	// Prevents "Address already in use" if a stale socket remains.
	_ = os.Remove(socketPath)
	_ = os.Remove(c.cfg.VMPIDPath(vmID))
	c.stopSwtpm(vmID) // Clean up stale swtpm from previous crash.

	// Start swtpm companion process for TPM emulation (must be running
	// before CH so the socket is ready for --tpm).
	swtpmStarted := false
	if cfg.TPMSocketPath != "" {
		if err := c.startSwtpm(vmID, cfg.TPMSocketPath); err != nil {
			return 0, fmt.Errorf("start swtpm for %s: %w", vmID, err)
		}
		swtpmStarted = true
	}

	// cleanupOnFail stops swtpm if it was started and CH launch fails.
	// Prevents leaked swtpm processes on every error path below.
	cleanupOnFail := func() {
		if swtpmStarted {
			c.stopSwtpm(vmID)
		}
	}

	// Build CH command-line arguments.
	// Only --api-socket is passed on the CLI. All VM configuration including
	// firmware, cpus, memory, disks, serial, and console is sent via the
	// PUT /api/v1/vm.create REST call so there is no conflict between CLI
	// flags and the REST API.
	args := buildLaunchArgs(socketPath)

	// CH is a long-lived daemon whose lifetime must exceed the CLI invocation
	// that started it. We use exec.Command (not CommandContext) because the
	// process is Release()'d after socket readiness — CommandContext's cancel
	// goroutine cannot kill a released process and would leak a goroutine.
	// Context cancellation is handled manually in the polling loop below.
	cmd := exec.Command(c.cfg.CHBinary, args...) //nolint:gosec // CHBinary is a trusted config value, not user input

	// Detach the CH process from the parent process group so it survives
	// if cocoon exits unexpectedly.
	configureCHProcess(cmd)

	// Write CH stderr to a log file for diagnostics on startup failure.
	chLogPath := c.cfg.VMCHLogPath(vmID)
	chLogFile, chLogErr := os.Create(chLogPath) //nolint:gosec // G304: path derived from trusted config
	if chLogErr == nil {
		cmd.Stdout = chLogFile
		cmd.Stderr = chLogFile
	}

	if err := cmd.Start(); err != nil {
		if chLogFile != nil {
			_ = chLogFile.Close()
		}
		cleanupOnFail()
		return 0, fmt.Errorf("start cloud-hypervisor: %w", err)
	}

	pid := cmd.Process.Pid

	// Write PID file.
	pidPath := c.cfg.VMPIDPath(vmID)
	if err := utils.WritePIDFile(pidPath, pid); err != nil {
		// Best-effort kill; the process was already started.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if chLogFile != nil {
			_ = chLogFile.Close()
		}
		cleanupOnFail()
		return 0, fmt.Errorf("write PID file %s: %w", pidPath, err)
	}

	// Wait for the API socket to appear so callers can immediately issue
	// REST requests after Launch returns. Check process liveness to
	// fast-fail if CH crashes on startup instead of waiting the full timeout.
	socketTimeout := 5 * time.Second
	deadline := time.Now().Add(socketTimeout)
	for time.Now().Before(deadline) {
		// Check socket existence then connectivity.
		if _, statErr := os.Stat(socketPath); statErr == nil {
			if connErr := c.CheckSocketConnectivity(socketPath); connErr == nil {
				// Socket ready. Release the process handle so CH is fully detached
				// from the Go runtime and lives as an independent OS process.
				// cmd.Wait() must NOT be used here: it keeps a Go-side reference to
				// the process, causing the runtime to kill it on CLI exit.
				// With Release(), CH is reparented to init and outlives the CLI.
				_ = cmd.Process.Release()
				if chLogFile != nil {
					_ = chLogFile.Close()
				}
				return pid, nil
			}
		}
		// Fast fail: if CH already exited, don't wait the full timeout.
		if !utils.IsProcessAlive(pid) {
			_ = cmd.Wait()
			if chLogFile != nil {
				_ = chLogFile.Close()
			}
			cleanupOnFail()
			stderr := readCHLog(chLogPath)
			return 0, fmt.Errorf("cloud-hypervisor exited immediately: %s", stderr)
		}
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			if chLogFile != nil {
				_ = chLogFile.Close()
			}
			cleanupOnFail()
			return 0, fmt.Errorf("context canceled waiting for CH socket: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}

	// Timeout. Kill and report diagnostics.
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if chLogFile != nil {
		_ = chLogFile.Close()
	}
	cleanupOnFail()
	stderr := readCHLog(chLogPath)
	return 0, fmt.Errorf("CH socket %s did not appear within %s: %s", socketPath, socketTimeout, stderr)
}

// Shutdown performs a graceful shutdown of the VM.
//
// Shutdown strategy (three tiers):
//  1. ACPI power-button: asks the guest OS to shut down gracefully.
//  2. vm.shutdown API: if the guest does not respond within the timeout,
//     tells Cloud Hypervisor to shut down at the VMM level, which flushes
//     all block backends (qcow2 metadata, dirty pages) before stopping.
//  3. SIGKILL: last resort if the vm.shutdown API also fails.
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
			// Do not rely on IsAlive() here because it validates process name
			// against current config (c.cfg.CHBinary). If the binary path/name
			// changes after VM start, IsAlive() can return false even while the
			// original CH process is still running.
			stopped, probeErr := c.isVMProcessStopped(vmID)
			if probeErr != nil {
				return fmt.Errorf("probe VM %s process state: %w", vmID, probeErr)
			}
			if stopped {
				c.cleanupRuntimeFiles(vmID)
				return nil
			}
			if time.Now().After(deadline) {
				// Guest did not respond to ACPI power-button in time.
				// Try vm.shutdown API to flush disk backends before killing.
				return c.shutdownWithFallback(ctx, vmID, socketPath)
			}
		}
	}
}

// shutdownWithFallback stops the CH process when the guest did not respond
// to an ACPI power-button (or was never sent one).
//
// Sequence: vm.shutdown (stop vCPUs, flush backends) → SIGTERM (graceful
// CH process exit) → SIGKILL (last resort).
func (c *client) shutdownWithFallback(ctx context.Context, vmID, socketPath string) error {
	// vm.shutdown stops the guest but the CH process stays alive.
	// Best-effort; if the API fails, proceed to signal-based shutdown.
	if err := c.ShutdownVM(ctx, socketPath); err != nil {
		log.Printf("vm.shutdown API failed for %s (proceeding to SIGTERM): %v", vmID, err)
	}

	return c.terminateProcess(vmID)
}

// terminateProcess sends SIGTERM to the CH process, waits briefly for
// graceful exit, then falls back to SIGKILL.
func (c *client) terminateProcess(vmID string) error {
	defer c.cleanupRuntimeFiles(vmID)

	pid, err := utils.ReadPIDFile(c.cfg.VMPIDPath(vmID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil // already gone
		}
		return fmt.Errorf("read PID for %s: %w", vmID, err)
	}
	if !utils.IsProcessAlive(pid) {
		return nil
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find CH process %d: %w", pid, err)
	}

	// SIGTERM: ask CH to clean up and exit gracefully.
	if err := process.Signal(syscall.SIGTERM); err != nil {
		if !utils.IsProcessAlive(pid) {
			return nil
		}
		log.Printf("SIGTERM failed for CH pid %d (%s): %v; falling back to SIGKILL", pid, vmID, err)
		return utils.ForceKillProcess(pid)
	}

	// Wait up to 5 seconds for graceful exit.
	const gracePeriod = 5 * time.Second
	deadline := time.Now().Add(gracePeriod)
	for time.Now().Before(deadline) {
		if !utils.IsProcessAlive(pid) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	log.Printf("CH pid %d (%s) did not exit after SIGTERM; falling back to SIGKILL", pid, vmID)
	return utils.ForceKillProcess(pid)
}

// isVMProcessStopped reports whether the CH process for vmID has exited.
// It intentionally checks PID liveness (not process-name identity) so stop
// polling does not misclassify a live VM as exited when CHBinary config is
// changed after the VM started.
func (c *client) isVMProcessStopped(vmID string) (bool, error) {
	pid, err := utils.ReadPIDFile(c.cfg.VMPIDPath(vmID))
	if err != nil {
		if os.IsNotExist(err) {
			// If PID file is missing but socket is still connectable, treat VM as
			// running and avoid deleting runtime artifacts prematurely.
			if connErr := c.CheckSocketConnectivity(c.cfg.VMSocketPath(vmID)); connErr == nil {
				return false, nil
			}
			return true, nil
		}
		return false, err
	}
	return !utils.IsProcessAlive(pid), nil
}

// ForceKill sends SIGKILL to the CH process for the given VM.
// It validates the PID identity before killing to prevent sending signals
// to an unrelated process if the PID was reused by the OS.
func (c *client) ForceKill(vmID string) error {
	// Always clean up companion processes (swtpm) and stale runtime files,
	// regardless of whether the CH kill succeeds. This prevents swtpm leaks
	// when ForceKill encounters an error (e.g., PID file missing, SIGKILL
	// timeout). cleanupRuntimeFiles is idempotent and best-effort.
	defer c.cleanupRuntimeFiles(vmID)

	pidPath := c.cfg.VMPIDPath(vmID)
	pid, err := utils.ReadPIDFile(pidPath)
	if err != nil {
		return fmt.Errorf("read PID for %s: %w", vmID, err)
	}
	expectedProc := filepath.Base(c.cfg.CHBinary)
	if !utils.ValidateProcess(pid, expectedProc) {
		if utils.IsProcessAlive(pid) {
			// PID exists but name doesn't match — genuinely reused by another
			// process. Don't kill, but preserve PID file for diagnostics.
			return fmt.Errorf("PID %d for %s is not %s (PID reused by another process)", pid, vmID, expectedProc)
		}
		// Process is gone. Cleanup handled by deferred cleanupRuntimeFiles.
		return nil
	}
	if err := utils.ForceKillProcess(pid); err != nil {
		return fmt.Errorf("force kill %s (pid %d): %w", vmID, pid, err)
	}
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
	return utils.ValidateProcess(pid, filepath.Base(c.cfg.CHBinary))
}

// cleanupRuntimeFiles removes the PID file and API socket for a VM,
// and stops the swtpm companion process.
// Best-effort: errors are ignored since files may already be gone.
func (c *client) cleanupRuntimeFiles(vmID string) {
	c.stopSwtpm(vmID)
	_ = os.Remove(c.cfg.VMPIDPath(vmID))
	_ = os.Remove(c.cfg.VMSocketPath(vmID))
}

// startSwtpm launches a swtpm process for TPM 2.0 emulation.
// The swtpm process must be running before the CH process is started so the
// TPM socket is ready for CH's --tpm flag.
func (c *client) startSwtpm(vmID string, tpmSocketPath string) error {
	tpmStateDir := c.cfg.VMTPMStateDir(vmID)
	if err := os.MkdirAll(tpmStateDir, 0o700); err != nil { //nolint:gosec // G301: restricted to owner — TPM state
		return fmt.Errorf("create TPM state dir %s: %w", tpmStateDir, err)
	}

	// Remove stale socket from previous run.
	_ = os.Remove(tpmSocketPath)

	args := []string{
		"socket",
		"--tpmstate", fmt.Sprintf("dir=%s", tpmStateDir),
		"--ctrl", fmt.Sprintf("type=unixio,path=%s", tpmSocketPath),
		"--flags", "startup-clear",
		"--tpm2",
	}

	// swtpm is a long-lived daemon that must outlive the CLI command that
	// started it. We use exec.Command (not CommandContext) because the
	// process is Release()'d below — CommandContext's cancel goroutine
	// cannot kill a released process and would leak a goroutine.
	// Lifecycle is managed explicitly via PID file and stopSwtpm().
	cmd := exec.Command("swtpm", args...) //nolint:gosec // args are constructed from trusted config paths
	configureCHProcess(cmd)

	// Write swtpm stderr to a log file for diagnostics on failure.
	swtpmLogPath := c.cfg.VMSwtpmLogPath(vmID)
	logFile, logErr := os.Create(swtpmLogPath) //nolint:gosec // G304: path derived from trusted config
	if logErr == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}

	if err := cmd.Start(); err != nil {
		if logFile != nil {
			_ = logFile.Close()
		}
		return fmt.Errorf("start swtpm: %w", err)
	}

	pid := cmd.Process.Pid

	pidPath := c.cfg.VMSwtpmPIDPath(vmID)
	if err := utils.WritePIDFile(pidPath, pid); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if logFile != nil {
			_ = logFile.Close()
		}
		return fmt.Errorf("write swtpm PID file: %w", err)
	}

	// Wait for the TPM socket to appear. Do NOT Release() the process
	// handle yet so we can detect early exit and call Wait() for cleanup.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(tpmSocketPath); err == nil {
			// Socket ready. Release the process handle so swtpm is fully detached
			// from the Go runtime and lives as an independent OS process.
			// See Launch() for rationale — same reasoning applies here.
			_ = cmd.Process.Release()
			if logFile != nil {
				_ = logFile.Close()
			}
			return nil
		}
		// Fast fail: if swtpm already exited, don't wait the full 5s.
		if !utils.IsProcessAlive(pid) {
			_ = cmd.Wait()
			if logFile != nil {
				_ = logFile.Close()
			}
			stderr := readSwtpmLog(swtpmLogPath)
			return fmt.Errorf("swtpm exited immediately: %s", stderr)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Timeout. Kill and report diagnostics.
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if logFile != nil {
		_ = logFile.Close()
	}
	stderr := readSwtpmLog(swtpmLogPath)
	c.stopSwtpm(vmID)
	return fmt.Errorf("swtpm socket %s did not appear within 5s: %s", tpmSocketPath, stderr)
}

// readProcessLog reads a process log file and returns its contents for error messages.
func readProcessLog(path string) string {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path derived from trusted config
	if err != nil || len(data) == 0 {
		return "(no output)"
	}
	return strings.TrimSpace(string(data))
}

// readSwtpmLog reads the swtpm log file and returns its contents for error messages.
func readSwtpmLog(path string) string { return readProcessLog(path) }

// readCHLog reads the CH stdout/stderr log file and returns its contents for error messages.
func readCHLog(path string) string { return readProcessLog(path) }

// stopSwtpm terminates the swtpm companion process for a VM.
// Best-effort: silently ignores missing PID files or already-exited processes.
func (c *client) stopSwtpm(vmID string) {
	pidPath := c.cfg.VMSwtpmPIDPath(vmID)
	pid, err := utils.ReadPIDFile(pidPath)
	if err != nil {
		return // No PID file — nothing to stop.
	}
	if utils.ValidateProcess(pid, "swtpm") {
		_ = utils.ForceKillProcess(pid)
	}
	_ = os.Remove(pidPath)
	_ = os.Remove(c.cfg.VMTPMSocketPath(vmID))
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
	logVMCreatePayload(socketPath, vmCfg, body)
	return c.doWithRetry(ctx, func() error {
		err := c.doPUT(ctx, socketPath, "/api/v1/vm.create", body)
		if isVMAlreadyCreatedError(err) {
			log.Printf("CH API reported VM already created for %s; treating vm.create as idempotent success", socketPath)
			return nil
		}
		return err
	})
}

func logVMCreatePayload(socketPath string, vmCfg *hypervisor.CHVMConfig, fallbackBody []byte) {
	prettyBody, err := json.MarshalIndent(vmCfg, "", "  ")
	if err != nil {
		prettyBody = fallbackBody
	}
	log.Printf("CH RPC vm.create request: socket=%s payload=%s", socketPath, string(prettyBody))
}

// BootVM sends PUT /api/v1/vm.boot.
func (c *client) BootVM(ctx context.Context, socketPath string) error {
	return c.doWithRetry(ctx, func() error {
		err := c.doPUT(ctx, socketPath, "/api/v1/vm.boot", nil)
		if isVMAlreadyBootedError(err) {
			log.Printf("CH API reported VM already running for %s; treating vm.boot as idempotent success", socketPath)
			return nil
		}
		return err
	})
}

// isVMAlreadyBootedError checks whether CH returned a "Running to Running"
// transition error, indicating the VM was already booted (e.g., auto-booted
// by CH when --firmware was passed with full config on CLI).
func isVMAlreadyBootedError(err error) bool {
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
	return strings.Contains(strings.ToLower(ae.Message), "running to running")
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

// GetConsolePTYPath retrieves the virtio-console PTY path for a running VM.
// Returns an empty string (no error) if the console is not in Pty mode.
func (c *client) GetConsolePTYPath(ctx context.Context, socketPath string) (string, error) {
	info, err := c.GetVMInfo(ctx, socketPath)
	if err != nil {
		return "", fmt.Errorf("get VM info: %w", err)
	}
	if info.Config.Console.Mode != "Pty" {
		return "", nil
	}
	return info.Config.Console.File, nil
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

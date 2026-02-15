# Cloud Hypervisor Integration Guide

**Version**: 1.1
**Status**: Implemented
**Phase**: Phase 1
**Last Updated**: 2026-02-15

## Executive Summary

This document specifies how Cocoon integrates with Cloud Hypervisor to manage VM processes, control lifecycle operations, and handle multi-VM scenarios. It covers the process model, socket management, HTTP API integration, and crash recovery strategies.

## Table of Contents

1. [Process Model Decision](#1-process-model-decision)
2. [Socket Naming and Multi-VM Organization](#2-socket-naming-and-multi-vm-organization)
3. [Cloud Hypervisor Process Lifecycle](#3-cloud-hypervisor-process-lifecycle)
4. [HTTP over Unix Socket in Go](#4-http-over-unix-socket-in-go)
5. [REST API Complete Mapping](#5-rest-api-complete-mapping)
6. [Crash Recovery and Reconciliation](#6-crash-recovery-and-reconciliation)
7. [Implementation Examples](#7-implementation-examples)
8. [Testing Strategy](#8-testing-strategy)

---

## 1. Process Model Decision

### 1.1 Selected Approach: One CH Process Per VM

**Decision**: Cocoon uses **one Cloud Hypervisor process per VM** (Approach A).

**Rationale**:

1. **Strong Isolation**:
   - Each VM has a dedicated CH process
   - Process crash only affects one VM
   - Independent resource management (CPU affinity, cgroup limits)

2. **Simplified Lifecycle**:
   - VM deletion = kill process + cleanup files
   - No shared state between VMs
   - Easier to implement graceful shutdown per VM

3. **Debugging and Monitoring**:
   - Clear 1:1 mapping: VM ID → Process PID
   - Per-VM logs and metrics
   - Straightforward process inspection with `ps`, `top`, `htop`

4. **Fault Tolerance**:
   - One VM crash doesn't affect others
   - No single point of failure
   - Independent restart policies per VM

5. **API Simplicity**:
   - Each CH process exposes its own API socket
   - No need for VM ID multiplexing in API requests
   - Direct communication: Cocoon ↔ CH (via socket) ↔ VM

**Trade-offs**:

| Aspect | One Process Per VM | Shared Daemon |
|--------|-------------------|---------------|
| Isolation | Excellent | Moderate |
| Resource Overhead | ~50MB per CH process | Lower (shared) |
| Complexity | Low | High |
| Fault Tolerance | Excellent | Single point of failure |
| Scalability | High (100s of VMs) | Very High (1000s of VMs) |

**Recommendation**: For typical Cocoon use cases, the one-process-per-VM model is optimal. If scaling to 1000+ concurrent VMs becomes necessary, a shared daemon can be considered in a future version.

---

## 2. Socket Naming and Multi-VM Organization

### 2.1 Directory Structure

> See [05-storage-management.md § Canonical Filesystem Layout](./05-storage-management.md#canonical-filesystem-layout-normative) for the authoritative filesystem layout specification.

VM data is split across two locations by data lifetime:

**Persistent state** (`/var/lib/cocoon/`) — survives reboot, used by reconcile/GC:
```
/var/lib/cocoon/vms/
├── vm-abc-123/
│   ├── overlay.qcow2          # VM's COW overlay disk
│   ├── config.json            # VM configuration (immutable)
│   ├── metadata.json          # Runtime metadata (authoritative)
│   ├── metadata.lock          # File lock for atomic updates
│   └── tpm/                   # TPM 2.0 persistent state (when TPM enabled)
├── vm-def-456/
│   ├── overlay.qcow2
│   ├── config.json
│   ├── metadata.json
│   └── metadata.lock
└── ...
```

**Runtime/ephemeral** (`/run/cocoon/`) — cleared on reboot, rebuilt by reconcile:
```
/run/cocoon/vms/
├── vm-abc-123/
│   ├── api.sock               # Cloud Hypervisor API socket
│   ├── ch.pid                 # CH process PID file
│   ├── swtpm.sock             # swtpm Unix socket (when TPM enabled)
│   └── swtpm.pid              # swtpm process PID file (when TPM enabled)
├── vm-def-456/
│   ├── api.sock
│   └── ch.pid
└── ...
```

**Logs** (`/var/log/cocoon/`) — persisted across reboots:
```
/var/log/cocoon/
├── vm-abc-123-serial.log      # Serial console output
├── vm-abc-123-ch.log          # Cloud Hypervisor stderr (startup diagnostics)
├── vm-abc-123-swtpm.log       # swtpm stderr (when TPM enabled)
├── vm-def-456-serial.log
└── ...
```

**Why this separation?**
- **Crash recovery**: Metadata in `/var/lib` survives power loss; `/run` is tmpfs and cleared on reboot
- **Reconcile correctness**: `cocoon doctor` scans `/var/lib/cocoon/vms/` for authoritative state, then checks `/run` for runtime liveness
- **Isolation**: Each VM has its own directory in both locations
- **Discovery**: List `/var/lib/cocoon/vms/` for all VMs (including stopped); list `/run/cocoon/vms/` for runtime sockets only

### 2.2 Socket Naming Convention

All path helpers are methods on `config.CocoonConfig`, making the root directories configurable:

```go
// config/config.go

// CocoonConfig holds global Cocoon configuration.
type CocoonConfig struct {
    RootDir    string // Persistent data root (default: /var/lib/cocoon)
    RuntimeDir string // Runtime/tmpfs root    (default: /run/cocoon)
    LogDir     string // Log directory          (default: /var/log/cocoon)
    CHBinary   string // Cloud Hypervisor binary path
    // ... (firmware paths, defaults, timeouts, etc.)
}

// Per-VM path helpers (methods on CocoonConfig):

func (c *CocoonConfig) VMSocketPath(vmID string) string {
    return filepath.Join(c.RuntimeDir, "vms", vmID, "api.sock")
}

// Example:
// cfg.RuntimeDir = "/run/cocoon"
// cfg.VMSocketPath("vm-abc-123") => "/run/cocoon/vms/vm-abc-123/api.sock"
```

### 2.3 PID File Management

PID read/write helpers live in the `utils` package and take a file path:

```go
// utils/process.go

// WritePIDFile writes a PID to a file.
func WritePIDFile(path string, pid int) error {
    return os.WriteFile(path, []byte(strconv.Itoa(pid)), 0o644)
}

// ReadPIDFile reads a PID from a file.
func ReadPIDFile(path string) (int, error) {
    data, err := os.ReadFile(path)
    if err != nil {
        return 0, err
    }
    return strconv.Atoi(strings.TrimSpace(string(data)))
}

// The PID file path is derived from CocoonConfig:
// pidPath := cfg.VMPIDPath(vmID)
// utils.WritePIDFile(pidPath, pid)
// pid, err := utils.ReadPIDFile(pidPath)
```

### 2.4 Serial Log Path

```go
// config/config.go (method on CocoonConfig)

func (c *CocoonConfig) VMSerialLogPath(vmID string) string {
    return filepath.Join(c.LogDir, vmID+"-serial.log")
}

func (c *CocoonConfig) VMCHLogPath(vmID string) string {
    return filepath.Join(c.LogDir, vmID+"-ch.log")
}
```

### 2.5 Metadata File

Runtime metadata stored per VM in persistent storage (`/var/lib`).
`VMMetadataFile` is the mutable runtime state (updated on every state transition).
`VMConfig` is the immutable configuration (written once at creation).

```go
// types/metadata.go

type VMMetadataFile struct {
    VMID          string `json:"vm_id"`
    State         string `json:"state"`          // "CREATING","CREATED","STARTING","RUNNING","STOPPING","STOPPED","ERROR","DELETED"
    PreviousState string `json:"previous_state"`

    // Runtime (changes with each start/stop cycle)
    ProcessPID       int    `json:"process_pid,omitempty"`
    BootTime         string `json:"boot_time,omitempty"`
    LastBootMode     string `json:"last_boot_mode,omitempty"`
    LastFirmwarePath string `json:"last_firmware_path,omitempty"`

    // Error tracking
    LastError     string `json:"last_error,omitempty"`
    LastErrorType string `json:"last_error_type,omitempty"`
    LastErrorAt   string `json:"last_error_at,omitempty"` // RFC 3339
    ErrorCount    int    `json:"error_count"`

    // Lifecycle flags
    AutoRemove bool `json:"auto_remove,omitempty"`

    // Timestamps (RFC 3339 strings)
    UpdatedAt string `json:"updated_at"`
    StartedAt string `json:"started_at,omitempty"`
    StoppedAt string `json:"stopped_at,omitempty"`

    // Schema version
    SchemaVersion int `json:"schema_version"`
}

// types/config.go (immutable, written once at creation)

type VMConfig struct {
    VMID          string `json:"vm_id"`
    Name          string `json:"name"`
    ImageRef      string `json:"image_ref"`
    BaseKey       string `json:"base_key"`
    Arch          string `json:"arch"`
    BootStrategy  string `json:"boot_strategy"`
    FirmwarePath  string `json:"firmware_path"`
    TPMSocketPath string `json:"tpm_socket_path,omitempty"`
    CPUs          int    `json:"cpus"`
    MemoryMB      int64  `json:"memory_mb"`
    DiskSize      string `json:"disk_size"`
    BaseImagePath string `json:"base_image_path"`
    OverlayPath   string `json:"overlay_path"`
    SerialLog     string `json:"serial_log"`      // in VMConfig, not metadata
    SocketPath    string `json:"socket_path"`      // in VMConfig, not metadata
    CreatedAt     string `json:"created_at"`       // RFC 3339
    SchemaVersion int    `json:"schema_version"`
}

// Metadata path is derived from CocoonConfig:
// metaPath := cfg.VMMetadataPath(vmID)
//   => filepath.Join(cfg.RootDir, "vms", vmID, "metadata.json")
```

**Valid VM states** (defined as `VMState` constants in `types/state.go`):

| State      | Description |
|------------|-------------|
| `CREATING` | VM creation in progress |
| `CREATED`  | Created but not yet started |
| `STARTING` | CH process launched, boot in progress |
| `RUNNING`  | Guest is booted and running |
| `STOPPING` | Graceful shutdown in progress |
| `STOPPED`  | Process exited cleanly |
| `ERROR`    | Crash, timeout, or failed transition |
| `DELETED`  | Marked for removal |

---

## 3. Cloud Hypervisor Process Lifecycle

### 3.1 Launch Process

**Command Structure**:

```go
// buildLaunchArgs constructs CLI arguments for cloud-hypervisor.
//
// The CLI only carries --api-socket. All VM configuration (firmware,
// cpus, memory, disk, serial, console, tpm) goes through the REST
// vm.create payload.
func buildLaunchArgs(socketPath string) []string {
    return []string{"--api-socket", socketPath}
}
```

All VM resource configuration (firmware path, CPUs, memory, disks, serial, console, TPM) is sent via the `PUT /api/v1/vm.create` REST call after the process starts. This avoids conflicts between CLI flags and the REST API and keeps the launch path uniform regardless of boot strategy or optional features.

**Launch Sequence** (implemented by `(c *client) Launch` in `hypervisor/cloudhypervisor/client.go`):

1. **Create runtime directory and clean up stale files** (persistent dirs are created during `cocoon create`):
   ```go
   runtimeDir := c.cfg.VMRuntimeDir(vmID)
   os.MkdirAll(runtimeDir, 0o755)

   // Best-effort cleanup of stale runtime files from a previous crash.
   socketPath := c.cfg.VMSocketPath(vmID)
   _ = os.Remove(socketPath)
   _ = os.Remove(c.cfg.VMPIDPath(vmID))
   c.stopSwtpm(vmID) // Clean up stale swtpm from previous crash.
   ```

2. **Start swtpm companion** (if TPM enabled):
   ```go
   swtpmStarted := false
   if cfg.TPMSocketPath != "" {
       if err := c.startSwtpm(vmID, cfg.TPMSocketPath); err != nil {
           return 0, fmt.Errorf("start swtpm for %s: %w", vmID, err)
       }
       swtpmStarted = true
   }
   // cleanupOnFail stops swtpm if CH launch fails.
   cleanupOnFail := func() {
       if swtpmStarted { c.stopSwtpm(vmID) }
   }
   ```

3. **Start CH process with stderr capture and process group isolation**:
   ```go
   args := buildLaunchArgs(socketPath)
   cmd := exec.CommandContext(ctx, c.cfg.CHBinary, args...)
   configureCHProcess(cmd) // Sets Setpgid so CH survives if cocoon exits

   chLogPath := c.cfg.VMCHLogPath(vmID)
   chLogFile, _ := os.Create(chLogPath)
   cmd.Stderr = chLogFile
   if err := cmd.Start(); err != nil {
       cleanupOnFail()
       return 0, fmt.Errorf("start cloud-hypervisor: %w", err)
   }
   ```

4. **Write PID file and wait for socket with fast-fail and context cancellation**:
   ```go
   pid := cmd.Process.Pid
   pidPath := c.cfg.VMPIDPath(vmID)
   utils.WritePIDFile(pidPath, pid)

   deadline := time.Now().Add(5 * time.Second)
   for time.Now().Before(deadline) {
       // Check socket existence then connectivity.
       if _, statErr := os.Stat(socketPath); statErr == nil {
           if connErr := c.CheckSocketConnectivity(socketPath); connErr == nil {
               cmd.Process.Release() // detach; CH outlives CLI
               return pid, nil
           }
       }
       // Fast fail: if CH already exited, report stderr immediately.
       if !utils.IsProcessAlive(pid) {
           cleanupOnFail()
           stderr := readCHLog(chLogPath)
           return 0, fmt.Errorf("cloud-hypervisor exited immediately: %s", stderr)
       }
       select {
       case <-ctx.Done():
           cmd.Process.Kill()
           cleanupOnFail()
           return 0, fmt.Errorf("context canceled waiting for CH socket: %w", ctx.Err())
       case <-time.After(100 * time.Millisecond):
       }
   }
   // Timeout — kill CH, report stderr.
   cmd.Process.Kill()
   cleanupOnFail()
   stderr := readCHLog(chLogPath)
   return 0, fmt.Errorf("CH socket did not appear within 5s: %s", stderr)
   ```

The fast-fail check (`utils.IsProcessAlive`) and CH stderr capture (`c.cfg.VMCHLogPath`) ensure that if CH crashes on startup (e.g., invalid firmware, missing kernel), the error is reported immediately with the actual CH error message rather than waiting the full 5-second timeout.

### 3.2 Monitoring Process Health

**Check if CH process is alive** (in `utils/process.go`):

```go
// utils.IsProcessAlive checks if a process with the given PID exists.
func IsProcessAlive(pid int) bool {
    if pid <= 0 {
        return false
    }
    process, err := os.FindProcess(pid)
    if err != nil {
        return false
    }
    return process.Signal(syscall.Signal(0)) == nil
}

// utils.ValidateProcess checks if a process is alive AND matches the
// expected name. Guards against PID reuse after a crash.
// Implementation is platform-specific (Linux reads /proc, macOS uses ps).
func ValidateProcess(pid int, expectedName string) bool {
    if !IsProcessAlive(pid) {
        return false
    }
    return validateProcessImpl(pid, expectedName)
}
```

**Check socket connectivity** (method on client):

```go
// CheckSocketConnectivity dials the Unix socket and immediately closes.
// Returns nil if the socket is reachable.
func (c *client) CheckSocketConnectivity(socketPath string) error {
    conn, err := net.DialTimeout("unix", socketPath, 2*time.Second)
    if err != nil {
        return fmt.Errorf("socket %s not accessible: %w", socketPath, err)
    }
    _ = conn.Close()
    return nil
}
```

### 3.3 Graceful Shutdown

The `Shutdown` method on the hypervisor client uses a ticker-based poll with context cancellation and calls `cleanupRuntimeFiles` (which also stops swtpm) on success:

```go
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
                c.cleanupRuntimeFiles(vmID) // removes PID, socket, stops swtpm
                return nil
            }
            if time.Now().After(deadline) {
                return c.ForceKill(vmID) // timeout; SIGKILL
            }
        }
    }
}

// cleanupRuntimeFiles removes the PID file and API socket for a VM,
// and stops the swtpm companion process.
func (c *client) cleanupRuntimeFiles(vmID string) {
    c.stopSwtpm(vmID)
    _ = os.Remove(c.cfg.VMPIDPath(vmID))
    _ = os.Remove(c.cfg.VMSocketPath(vmID))
}
```

**ForceKill validates PID identity** before sending SIGKILL to guard against PID reuse:

```go
func (c *client) ForceKill(vmID string) error {
    pidPath := c.cfg.VMPIDPath(vmID)
    pid, err := utils.ReadPIDFile(pidPath)
    if err != nil {
        return fmt.Errorf("read PID for %s: %w", vmID, err)
    }
    if !utils.ValidateProcess(pid, "cloud-hypervisor") {
        if utils.IsProcessAlive(pid) {
            // PID reused by another process — don't kill.
            return fmt.Errorf("PID %d for %s is not cloud-hypervisor", pid, vmID)
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
```

### 3.4 VM Deletion Flow

The Delete method on the manager (`vm/engine/manager.go`) performs a comprehensive cleanup. Note: it **never** calls `vm.delete` on the CH REST API; instead it goes straight to `ForceKill` to terminate the process, then removes all artifacts:

```go
// (m *manager) Delete removes a VM and all its resources.
// Transition: CREATED/STOPPED/ERROR -> DELETED.
// If the VM is RUNNING, force must be true.
func (m *manager) Delete(ctx context.Context, vmID string, force bool) error {
    meta, err := m.LoadMetadata(vmID)
    // ... (best-effort: continues cleanup even if metadata missing)

    // Step 1: If running, require --force. Attempt graceful stop first.
    if meta.State == "RUNNING" {
        if !force {
            return types.ErrVMRunning
        }
        if stopErr := m.Stop(ctx, vmID, 10*time.Second); stopErr != nil {
            _ = m.hyper.ForceKill(vmID)
        }
    }

    // Step 2: Force kill if process might still be alive.
    if m.hyper.IsAlive(vmID) {
        _ = m.hyper.ForceKill(vmID)
    }
    // Safety net: kill by metadata PID if PID file already cleaned up.
    if meta.ProcessPID > 0 && utils.ValidateProcess(meta.ProcessPID, "cloud-hypervisor") {
        _ = utils.ForceKillProcess(meta.ProcessPID)
    }

    // Step 3: Transition metadata to DELETED state.
    _ = m.TransitionState(vmID, types.VMStateDeleted, "delete requested")

    // Step 4: Unpin reference (remove VM from base image's reference list).
    vmCfg, _ := m.LoadConfig(vmID)
    if vmCfg != nil && vmCfg.BaseKey != "" {
        _ = m.refCounter.RemoveReference(vmCfg.BaseKey, vmID)
    }

    // Step 5: Remove COW overlay.
    _ = m.cowMgr.RemoveOverlay(vmID)

    // Step 6: Remove name from index.
    if vmCfg != nil && vmCfg.Name != "" {
        _ = RemoveName(m.cfg, vmCfg.Name)
    }

    // Step 7: Remove VM directories (persistent + runtime).
    _ = os.RemoveAll(m.cfg.VMPersistDir(vmID))
    _ = os.RemoveAll(m.cfg.VMRuntimeDir(vmID))

    // Step 8: Remove log files.
    _ = os.Remove(m.cfg.VMSerialLogPath(vmID))
    _ = os.Remove(m.cfg.VMCHLogPath(vmID))
    _ = os.Remove(m.cfg.VMSwtpmLogPath(vmID))

    return nil
}
```

### 3.5 Crash Detection

Crash detection is handled by the reconciler's `determineActualState` method, which compares metadata state against actual process/socket probes:

```go
// determineActualState probes the system to find the actual VM state.
// Uses priority: process status > socket > metadata.
func (m *manager) determineActualState(meta *types.VMMetadataFile, vmCfg *types.VMConfig) types.VMState {
    processValid := utils.ValidateProcess(meta.ProcessPID, "cloud-hypervisor")
    socketConnectable := canConnectToSocket(vmCfg.SocketPath)

    switch types.VMState(meta.State) {
    case types.VMStateRunning:
        if processValid && socketConnectable {
            return types.VMStateRunning
        }
        return types.VMStateError // crashed

    case types.VMStateStarting:
        if processValid {
            return types.VMStateStarting // still booting
        }
        return types.VMStateError // process died during start

    case types.VMStateStopping:
        if processValid {
            return types.VMStateStopping // still shutting down
        }
        return types.VMStateStopped // process exited

    case types.VMStateStopped:
        if utils.IsProcessAlive(meta.ProcessPID) {
            return types.VMStateError // unexpected orphan process
        }
        return types.VMStateStopped
    // ...
    }
}
```

---

## 4. HTTP over Unix Socket in Go

### 4.1 Client Implementation

Cloud Hypervisor exposes its API via HTTP over a Unix socket. The client is an unexported `client` struct that creates HTTP clients per socket via `newHTTPClient(socketPath)`:

```go
package cloudhypervisor

// client implements the hypervisor.Client interface using HTTP over Unix
// socket for the CH REST API and os/exec for process management.
type client struct {
    cfg         *config.CocoonConfig
    httpTimeout time.Duration
    maxRetries  int
    baseBackoff time.Duration
}

// New creates a new hypervisor client backed by the given Cocoon config.
func New(cfg *config.CocoonConfig) hypervisor.Client {
    return &client{
        cfg:         cfg,
        httpTimeout: 30 * time.Second,
        maxRetries:  3,              // defaultMaxRetries
        baseBackoff: 100 * time.Millisecond, // defaultBaseBackoff
    }
}

// newHTTPClient returns an *http.Client that dials the given Unix socket.
// A new client is created per socket (per-request) rather than sharing one.
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
```

### 4.2 Making Requests

All REST API methods take `ctx context.Context` and `socketPath string` parameters:

**PUT request example**:

```go
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
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusNoContent {
        respBody, _ := io.ReadAll(resp.Body)
        return &apiError{
            StatusCode: resp.StatusCode,
            Message:    fmt.Sprintf("PUT %s returned %d: %s", path, resp.StatusCode, string(respBody)),
        }
    }
    return nil
}
```

**GET request example**:

```go
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
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        respBody, _ := io.ReadAll(resp.Body)
        return nil, &apiError{StatusCode: resp.StatusCode, Message: string(respBody)}
    }

    var info hypervisor.CHVMInfo
    if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
        return nil, fmt.Errorf("decode vm.info response: %w", err)
    }
    return &info, nil
}
```

### 4.3 Context and Timeout Handling

All public methods accept `context.Context` and use `http.NewRequestWithContext`. The context is checked during retry sleep via `select` on `ctx.Done()`. See Section 5.3 for the retry implementation.

---

## 5. REST API Complete Mapping

### 5.1 Cloud Hypervisor API Overview

Cloud Hypervisor exposes a REST API for VM management. All endpoints return:
- `204 No Content` on success (for mutating operations)
- `200 OK` with JSON body (for queries)
- `4xx/5xx` with error details on failure

### 5.2 VM Lifecycle Operations

#### PUT /api/v1/vm.create

**Purpose**: Create a VM with specified configuration

**Request**:
```go
type CHVMConfig struct {
    Payload *CHPayloadConfig `json:"payload,omitempty"`
    CPUs    CHCPUConfig      `json:"cpus"`
    Memory  CHMemoryConfig   `json:"memory"`
    Disks   []CHDiskConfig   `json:"disks,omitempty"`
    Serial  CHSerialConfig   `json:"serial"`
    Console CHConsoleConfig  `json:"console"`
    TPM     *CHTPMConfig     `json:"tpm,omitempty"`
}

// CHPayloadConfig specifies the boot firmware/kernel for the VM.
// For UEFI boot (cloud images), set Firmware to the CLOUDHV.fd path.
// For direct kernel boot (OCI VM images), set Kernel to the extracted kernel path.
// Initramfs and Cmdline are used for direct kernel boot (Phase 2).
type CHPayloadConfig struct {
    Firmware  string `json:"firmware,omitempty"`
    Kernel    string `json:"kernel,omitempty"`
    Initramfs string `json:"initramfs,omitempty"`
    Cmdline   string `json:"cmdline,omitempty"`
}

type CHCPUConfig struct {
    BootVCPUs int `json:"boot_vcpus"`
    MaxVCPUs  int `json:"max_vcpus"` // Required by newer CH versions; pinned to boot_vcpus
}

type CHMemoryConfig struct {
    Size int64 `json:"size"` // In bytes
}

type CHDiskConfig struct {
    Path     string `json:"path"`
    ReadOnly bool   `json:"readonly,omitempty"`
}

type CHSerialConfig struct {
    Mode string `json:"mode"` // "Null", "Tty", "File"
    File string `json:"file,omitempty"`
}

type CHConsoleConfig struct {
    Mode string `json:"mode"` // "Off", "Tty", "File", "Pty"
}

// CHTPMConfig specifies the TPM 2.0 emulator socket path.
type CHTPMConfig struct {
    Socket string `json:"socket"`
}
```

**Implementation** (all methods take `ctx` and `socketPath`, use `doWithRetry`):
```go
func (c *client) CreateVM(ctx context.Context, socketPath string, vmCfg *hypervisor.CHVMConfig) error {
    body, _ := json.Marshal(vmCfg)
    return c.doWithRetry(ctx, func() error {
        err := c.doPUT(ctx, socketPath, "/api/v1/vm.create", body)
        if isVMAlreadyCreatedError(err) {
            return nil // treat as idempotent success
        }
        return err
    })
}
```

#### PUT /api/v1/vm.boot

**Purpose**: Boot the VM (after vm.create)

**Request**: No body

**Implementation**:
```go
func (c *client) BootVM(ctx context.Context, socketPath string) error {
    return c.doWithRetry(ctx, func() error {
        err := c.doPUT(ctx, socketPath, "/api/v1/vm.boot", nil)
        if isVMAlreadyBootedError(err) {
            return nil // treat as idempotent success
        }
        return err
    })
}
```

#### PUT /api/v1/vm.shutdown

**Purpose**: Gracefully shutdown the VM (ACPI)

**Request**: No body

**Implementation**:
```go
func (c *client) ShutdownVM(ctx context.Context, socketPath string) error {
    return c.doWithRetry(ctx, func() error {
        return c.doPUT(ctx, socketPath, "/api/v1/vm.shutdown", nil)
    })
}
```

#### PUT /api/v1/vm.power-button

**Purpose**: Send ACPI power button event

**Implementation**:
```go
func (c *client) PowerButton(ctx context.Context, socketPath string) error {
    return c.doWithRetry(ctx, func() error {
        return c.doPUT(ctx, socketPath, "/api/v1/vm.power-button", nil)
    })
}
```

#### PUT /api/v1/vm.delete

**Purpose**: Delete the VM and free resources inside CH

**Request**: No body

**Implementation**:
```go
func (c *client) DeleteVM(ctx context.Context, socketPath string) error {
    return c.doWithRetry(ctx, func() error {
        return c.doPUT(ctx, socketPath, "/api/v1/vm.delete", nil)
    })
}
```

#### GET /api/v1/vm.info

**Purpose**: Get current VM information

**Response**:
```go
type CHVMInfo struct {
    Config           CHVMConfig `json:"config"`
    State            string     `json:"state"` // "Created", "Running", "Shutdown", "Paused"
    MemoryActualSize int64      `json:"memory_actual_size"`
}
```

**Implementation**:
```go
func (c *client) GetVMInfo(ctx context.Context, socketPath string) (*hypervisor.CHVMInfo, error) {
    var info *hypervisor.CHVMInfo
    err := c.doWithRetry(ctx, func() error {
        var innerErr error
        info, innerErr = c.doGetVMInfo(ctx, socketPath)
        return innerErr
    })
    return info, err
}
```

### 5.3 Error Handling and Retries

The client uses a generic `doWithRetry` with exponential backoff and jitter, plus a typed `isRetryable` error classifier:

```go
// apiError represents an HTTP error response from the CH REST API.
type apiError struct {
    StatusCode int
    Message    string
}

// isRetryable determines whether an error is transient and should be retried.
// Retryable: connection refused, connection reset, HTTP 5xx, HTTP 429.
// Not retryable: HTTP 4xx (except 429), context.Canceled, context.DeadlineExceeded.
func isRetryable(err error) bool {
    if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
        return false
    }
    var ae *apiError
    if errors.As(err, &ae) {
        if ae.StatusCode == http.StatusTooManyRequests { return true }
        if ae.StatusCode >= 400 && ae.StatusCode < 500 { return false }
        if ae.StatusCode >= 500 { return true }
    }
    var opErr *net.OpError
    if errors.As(err, &opErr) {
        errMsg := opErr.Err.Error()
        return strings.Contains(errMsg, "connection refused") ||
               strings.Contains(errMsg, "connection reset")
    }
    return false
}

// doWithRetry executes fn with exponential backoff + jitter.
// Default: maxRetries=3, baseBackoff=100ms -> 200ms -> 400ms.
func (c *client) doWithRetry(ctx context.Context, fn func() error) error {
    var lastErr error
    backoff := c.baseBackoff

    for attempt := 0; attempt <= c.maxRetries; attempt++ {
        lastErr = fn()
        if lastErr == nil {
            return nil
        }
        if !isRetryable(lastErr) {
            return lastErr
        }
        if attempt == c.maxRetries {
            break
        }

        // Wait with jitter: backoff +/- 25%, floored at baseBackoff/4.
        jitter := time.Duration(rand.Int64N(int64(backoff/2))) - backoff/4
        wait := backoff + jitter
        if wait < c.baseBackoff/4 {
            wait = c.baseBackoff / 4
        }
        select {
        case <-ctx.Done():
            return fmt.Errorf("retry canceled: %w", ctx.Err())
        case <-time.After(wait):
        }
        backoff *= 2 // exponential
    }

    return fmt.Errorf("after %d retries: %w", c.maxRetries, lastErr)
}
```

### 5.4 Complete API Client Interface

```go
package hypervisor

type Client interface {
    // --- Process management ---
    Launch(ctx context.Context, vmID string, cfg *types.VMConfig) (pid int, err error)
    Shutdown(ctx context.Context, vmID string, timeout time.Duration) error
    ForceKill(vmID string) error
    IsAlive(vmID string) bool

    // --- CH REST API over Unix socket ---
    CreateVM(ctx context.Context, socketPath string, vmCfg *CHVMConfig) error
    BootVM(ctx context.Context, socketPath string) error
    ShutdownVM(ctx context.Context, socketPath string) error
    PowerButton(ctx context.Context, socketPath string) error
    DeleteVM(ctx context.Context, socketPath string) error
    GetVMInfo(ctx context.Context, socketPath string) (*CHVMInfo, error)

    // --- Utilities ---
    WaitForSocket(ctx context.Context, socketPath string, timeout time.Duration) error
    CheckSocketConnectivity(socketPath string) error
}
```

---

## 6. Crash Recovery and Reconciliation

### 6.1 Startup Reconciliation

When Cocoon starts (or when `cocoon doctor` runs), it must reconcile the expected state (metadata) with the actual state (running processes). The reconciler is a method on the manager:

**Reconciliation sequence**:

```go
// (m *manager) Reconcile scans all VMs and detects inconsistencies between
// metadata and actual system state.
// fix=true: attempt to repair inconsistencies.
// force=true: also kill zombie processes and force-move stuck VMs to ERROR.
func (m *manager) Reconcile(ctx context.Context, fix bool, force bool) ([]vm.Inconsistency, error) {
    // Step 1: Scan cfg.VMDir() for VM directories (authoritative source).
    entries, _ := os.ReadDir(m.cfg.VMDir())

    var inconsistencies []vm.Inconsistency
    knownPIDs := make(map[int]string) // pid -> vmID

    for _, entry := range entries {
        vmID := entry.Name()

        // Step 2: Load config.json and metadata.json.
        vmCfg, _ := m.LoadConfig(vmID)
        meta, _ := m.LoadMetadata(vmID)

        // Step 3: Check references.json contains vmID under base_key.
        // (detects crash between pin and creation completion)

        // Step 4: Determine actual state by probing the system.
        actualState := m.determineActualState(meta, vmCfg)
        metaState := types.VMState(meta.State)

        if actualState != metaState {
            inconsistencies = append(inconsistencies, vm.Inconsistency{
                VMID:          vmID,
                Type:          vm.InconsistencyStateMismatch,
                Details:       fmt.Sprintf("metadata=%s, actual=%s", meta.State, actualState),
                ExpectedState: meta.State,
                ActualState:   string(actualState),
            })
        }

        // Step 5: Detect zombie resources (stale PID files, orphan sockets).
        // Step 6: Check overlay existence.
    }

    // Step 7: Detect dangling references in references.json.
    // Step 8: Detect stale entries in name-index.json.

    // Step 9: Apply fixes if requested.
    if fix {
        for i := range inconsistencies {
            m.applyFix(&inconsistencies[i], force)
        }
    }

    // Step 10: Detect orphaned cloud-hypervisor and swtpm processes
    // not tracked by any VM (scans /proc).
    orphans := detectOrphanedProcesses(knownPIDs)
    inconsistencies = append(inconsistencies, orphans...)

    return inconsistencies, nil
}
```

### 6.2 Crash Handling

Crashed VMs (metadata says RUNNING/STARTING but process is dead) are transitioned to `ERROR` state. There is no "crashed" state; all crash scenarios map to `VMStateError`:

```go
// fixStateMismatch updates metadata.json to reflect the actual system state.
func (m *manager) fixStateMismatch(inc *vm.Inconsistency, force bool) error {
    meta, _ := m.LoadMetadata(inc.VMID)
    actualState := types.VMState(inc.ActualState)

    switch actualState {
    case types.VMStateError:
        meta.PreviousState = meta.State
        meta.State = string(types.VMStateError)
        meta.LastError = fmt.Sprintf("reconciliation: was %s but actual state is ERROR", inc.ExpectedState)
        meta.ErrorCount++
        // Kill zombie process if present and force is set.
        if force && meta.ProcessPID > 0 && utils.ValidateProcess(meta.ProcessPID, "cloud-hypervisor") {
            _ = syscall.Kill(meta.ProcessPID, syscall.SIGKILL)
        }
        meta.ProcessPID = 0
        return m.SaveMetadata(meta)

    case types.VMStateStopped:
        meta.PreviousState = meta.State
        meta.State = string(types.VMStateStopped)
        meta.ProcessPID = 0
        meta.StoppedAt = time.Now().UTC().Format(time.RFC3339)
        return m.SaveMetadata(meta)
    }
    // ...
}
```

### 6.3 Orphaned Process Detection

The reconciler scans `/proc` for `cloud-hypervisor` and `swtpm` processes not tracked by any VM's metadata:

```go
// detectOrphanedProcesses scans /proc for cloud-hypervisor and swtpm
// processes that are not tracked by any VM's metadata.
func detectOrphanedProcesses(knownPIDs map[int]string) []vm.Inconsistency {
    var orphans []vm.Inconsistency
    entries, _ := os.ReadDir("/proc")
    for _, entry := range entries {
        pid, err := strconv.Atoi(entry.Name())
        if err != nil { continue }
        if _, known := knownPIDs[pid]; known { continue }
        for _, procName := range []string{"cloud-hypervisor", "swtpm"} {
            if utils.ValidateProcess(pid, procName) {
                orphans = append(orphans, vm.Inconsistency{
                    Type:    vm.InconsistencyZombieProcess,
                    Details: fmt.Sprintf("orphaned %s process PID=%d", procName, pid),
                })
            }
        }
    }
    return orphans
}
```

### 6.4 PID File Validation

PID identity is validated via `utils.ValidateProcess`, which checks both liveness and process name (platform-specific: Linux reads `/proc/{pid}/cmdline`, macOS uses `ps`):

```go
func validatePIDFile(vmID string) error {
    pidPath := cfg.VMPIDPath(vmID)
    pid, err := utils.ReadPIDFile(pidPath)
    if err != nil {
        return err
    }

    if !utils.ValidateProcess(pid, "cloud-hypervisor") {
        return fmt.Errorf("PID %d is not a cloud-hypervisor process", pid)
    }

    return nil
}
```

---

## 7. Implementation Examples

### 7.1 Complete VM Lifecycle Example

```go
package main

import (
    "context"
    "log"
    "time"

    "github.com/CMGS/cocoon/config"
    "github.com/CMGS/cocoon/hypervisor/cloudhypervisor"
    "github.com/CMGS/cocoon/types"
    "github.com/CMGS/cocoon/vm/engine"
)

func main() {
    ctx := context.Background()

    // Step 1: Load configuration (configurable root dirs)
    cfg := config.DefaultConfig()
    cfg.EnsureDirs()

    // Step 2: Create hypervisor client
    hyper := cloudhypervisor.New(cfg)

    // Step 3: Create VM manager (with ref counter, COW manager, image manager)
    mgr := engine.New(cfg, hyper, refCounter, cowMgr, imgMgr)

    // Step 4: Create VM (provisions image, overlay, config, metadata, pins reference)
    vmCfg, err := mgr.Create(ctx, &vm.CreateOptions{
        Image:    "docker.io/library/ubuntu:22.04",
        Name:     "my-vm",
        CPUs:     2,
        MemoryMB: 2048,
    })
    if err != nil {
        log.Fatalf("Failed to create VM: %v", err)
    }
    // VM is now in CREATED state.

    // Step 5: Start VM (Launch CH, vm.create, vm.boot -> STARTING -> RUNNING)
    // Start() handles: CH launch (with swtpm if TPM), REST vm.create + vm.boot,
    // PID recording, boot detection, state transitions.
    err = mgr.Start(ctx, vmCfg.VMID)
    if err != nil {
        log.Fatalf("Failed to start VM: %v", err)
    }
    log.Printf("VM %s started successfully", vmCfg.VMID)

    // Step 6: Monitor VM
    time.Sleep(60 * time.Second)

    // Step 7: Stop VM (RUNNING -> STOPPING -> STOPPED)
    err = mgr.Stop(ctx, vmCfg.VMID, 30*time.Second)
    if err != nil {
        log.Fatalf("Failed to stop VM: %v", err)
    }

    // Step 8: Delete VM (STOPPED -> DELETED, removes all artifacts)
    err = mgr.Delete(ctx, vmCfg.VMID, false)
    if err != nil {
        log.Fatalf("Failed to delete VM: %v", err)
    }

    log.Printf("VM %s lifecycle complete", vmCfg.VMID)
}
```

### 7.2 High-Concurrency VM Creation

```go
func CreateMultipleVMs(count int) error {
    var wg sync.WaitGroup
    errors := make(chan error, count)

    for i := 0; i < count; i++ {
        wg.Add(1)
        go func(index int) {
            defer wg.Done()

            vmID := fmt.Sprintf("vm-concurrent-%03d", index)
            err := CreateAndStartVM(vmID)
            if err != nil {
                errors <- fmt.Errorf("VM %s failed: %w", vmID, err)
            }
        }(i)
    }

    wg.Wait()
    close(errors)

    // Check for errors
    for err := range errors {
        log.Printf("ERROR: %v", err)
    }

    return nil
}
```

---

## 8. Testing Strategy

### 8.1 Unit Tests

Test individual components:

```go
func TestSocketPathGeneration(t *testing.T) {
    cfg := config.DefaultConfig()
    vmID := "vm-test-123"
    expected := "/run/cocoon/vms/vm-test-123/api.sock"

    actual := cfg.VMSocketPath(vmID)

    if actual != expected {
        t.Errorf("Expected %s, got %s", expected, actual)
    }
}

func TestPIDFileOperations(t *testing.T) {
    tmpDir := t.TempDir()
    pidPath := filepath.Join(tmpDir, "ch.pid")
    testPID := 12345

    // Write PID
    err := utils.WritePIDFile(pidPath, testPID)
    if err != nil {
        t.Fatalf("Failed to write PID: %v", err)
    }

    // Read PID
    readPID, err := utils.ReadPIDFile(pidPath)
    if err != nil {
        t.Fatalf("Failed to read PID: %v", err)
    }

    if readPID != testPID {
        t.Errorf("Expected PID %d, got %d", testPID, readPID)
    }
}
```

### 8.2 Integration Tests

Test with real Cloud Hypervisor:

```go
func TestVMLifecycle(t *testing.T) {
    if testing.Short() {
        t.Skip("Skipping integration test")
    }

    ctx := context.Background()
    cfg := config.DefaultConfig()
    mgr := setupTestManager(cfg) // wire up real dependencies

    // Create and start VM
    vmCfg, err := mgr.Create(ctx, &vm.CreateOptions{Image: "test-image"})
    if err != nil {
        t.Fatalf("Failed to create VM: %v", err)
    }

    err = mgr.Start(ctx, vmCfg.VMID)
    if err != nil {
        t.Fatalf("Failed to start VM: %v", err)
    }

    // Verify VM is running
    meta, _ := mgr.LoadMetadata(vmCfg.VMID)
    if meta.State != string(types.VMStateRunning) {
        t.Errorf("Expected state RUNNING, got %s", meta.State)
    }

    // Stop VM
    err = mgr.Stop(ctx, vmCfg.VMID, 30*time.Second)
    if err != nil {
        t.Errorf("Failed to stop VM: %v", err)
    }

    // Cleanup
    mgr.Delete(ctx, vmCfg.VMID, true)
}
```

### 8.3 Crash Recovery Tests

```go
func TestCrashRecovery(t *testing.T) {
    ctx := context.Background()
    cfg := config.DefaultConfig()
    mgr := setupTestManager(cfg)

    // Create and start VM
    vmCfg, _ := mgr.Create(ctx, &vm.CreateOptions{Image: "test-image"})
    mgr.Start(ctx, vmCfg.VMID)

    // Simulate crash by killing process
    meta, _ := mgr.LoadMetadata(vmCfg.VMID)
    syscall.Kill(meta.ProcessPID, syscall.SIGKILL)

    // Wait for process to die
    time.Sleep(1 * time.Second)

    // Run reconciliation
    inconsistencies, err := mgr.Reconcile(ctx, true /* fix */, false /* force */)
    if err != nil {
        t.Errorf("Reconciliation failed: %v", err)
    }

    // Verify crash was detected and state moved to ERROR
    meta, _ = mgr.LoadMetadata(vmCfg.VMID)
    if meta.State != string(types.VMStateError) {
        t.Errorf("Expected state 'ERROR', got '%s'", meta.State)
    }

    // Verify inconsistency was reported
    found := false
    for _, inc := range inconsistencies {
        if inc.VMID == vmCfg.VMID && inc.Type == vm.InconsistencyStateMismatch {
            found = true
        }
    }
    if !found {
        t.Error("Expected state mismatch inconsistency not found")
    }
}
```

---

## Summary

This document provides a comprehensive guide to integrating Cloud Hypervisor with Cocoon:

1. **Process Model**: One CH process per VM for strong isolation
2. **Filesystem Contract**: Persistent state in `/var/lib/cocoon/vms/{vm-id}/` (including `tpm/` subdirectory), runtime in `/run/cocoon/vms/{vm-id}/` (including `swtpm.sock` and `swtpm.pid`), logs in `/var/log/cocoon/` (serial, CH stderr, swtpm stderr)
3. **CLI Minimalism**: CH CLI only receives `--api-socket`; all VM configuration (firmware, CPUs, memory, disks, serial, console, TPM) goes through the REST `vm.create` payload
4. **Companion Process Lifecycle**: swtpm (TPM emulation) starts before CH and is cleaned up on all error paths via `cleanupOnFail()` closure
5. **Startup Diagnostics**: CH stderr captured to log file; fast-fail with process liveness check during socket wait
6. **HTTP Client**: Per-socket HTTP clients via `newHTTPClient(socketPath)`, with `doWithRetry` exponential backoff + jitter and typed `isRetryable` classifier
7. **Crash Recovery**: Method-based reconciliation `(m *manager) Reconcile(ctx, fix, force)`, orphan cleanup (CH + swtpm processes), reference and name index repair

---

**References**:
- Cloud Hypervisor API: https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/vmm/src/api/openapi/cloud-hypervisor.yaml
- Boot Contract: `docs/01-boot-contract.md`
- Installation Guide: `docs/02-installation.md`

# Cloud Hypervisor Integration Guide

**Version**: 1.0
**Status**: Draft
**Priority**: P0 - CRITICAL

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

**Recommendation**: For AI Agent sandboxes and typical Cocoon use cases, the one-process-per-VM model is optimal. If scaling to 1000+ concurrent VMs becomes necessary, a shared daemon can be considered in a future version.

---

## 2. Socket Naming and Multi-VM Organization

### 2.1 Directory Structure

All VM-related files are organized under `/run/cocoon/vms/{vm-id}/`:

```
/run/cocoon/
└── vms/
    ├── vm-abc-123/
    │   └── api.sock           # Cloud Hypervisor API socket
    ├── vm-def-456/
    │   └── api.sock
    └── ...

/var/log/cocoon/
├── vm-abc-123-serial.log      # Serial console output
├── vm-def-456-serial.log
└── ...
```

**Why this structure?**
- **Isolation**: Each VM has its own directory
- **Cleanup**: Delete entire directory to remove all VM files
- **Discovery**: List `/run/cocoon/vms/` to find all active VMs
- **Debugging**: All VM files in one place

### 2.2 Socket Naming Convention

```go
// Socket path generation
func GetVMSocketPath(vmID string) string {
    return filepath.Join("/run/cocoon/vms", vmID, "api.sock")
}

// Example:
// VM ID: "vm-abc-123"
// Socket: "/run/cocoon/vms/vm-abc-123/api.sock"
```

### 2.3 PID File Management

```go
// PID file path
func GetVMPIDPath(vmID string) string {
    return filepath.Join("/run/cocoon/vms", vmID, "ch.pid")
}

// Write PID after CH process starts
func WritePID(vmID string, pid int) error {
    pidPath := GetVMPIDPath(vmID)
    return os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0644)
}

// Read PID for monitoring
func ReadPID(vmID string) (int, error) {
    pidPath := GetVMPIDPath(vmID)
    data, err := os.ReadFile(pidPath)
    if err != nil {
        return 0, err
    }
    return strconv.Atoi(strings.TrimSpace(string(data)))
}
```

### 2.4 Serial Log Path

```go
func GetVMSerialLogPath(vmID string) string {
    return fmt.Sprintf("/var/log/cocoon/%s-serial.log", vmID)
}
```

### 2.5 Metadata File

Runtime metadata stored per VM:

```go
type VMMetadata struct {
    VMID        string    `json:"vm_id"`
    State       string    `json:"state"`        // "booting", "running", "stopped"
    ProcessPID  int       `json:"process_pid"`
    SocketPath  string    `json:"socket_path"`
    SerialLog   string    `json:"serial_log"`
    CreatedAt   time.Time `json:"created_at"`
    UpdatedAt   time.Time `json:"updated_at"`
}

func GetVMMetadataPath(vmID string) string {
    return filepath.Join("/run/cocoon/vms", vmID, "metadata.json")
}
```

---

## 3. Cloud Hypervisor Process Lifecycle

### 3.1 Launch Process

**Command Structure**:

```go
func LaunchCloudHypervisor(vmID string, config *VMConfig) (*exec.Cmd, error) {
    socketPath := GetVMSocketPath(vmID)
    serialLog := GetVMSerialLogPath(vmID)

    args := []string{
        "--api-socket", socketPath,
        "--disk", fmt.Sprintf("path=%s", config.DiskPath),
        "--cpus", fmt.Sprintf("boot=%d", config.CPUs),
        "--memory", fmt.Sprintf("size=%dM", config.MemoryMB),
        "--serial", fmt.Sprintf("file=%s", serialLog),
        "--console", "off",
    }

    // Cloud-init is served via metadata server (169.254.169.254), no ISO needed

    cmd := exec.Command("cloud-hypervisor", args...)
    return cmd, nil
}
```

**Launch Sequence**:

1. **Create VM directory**:
   ```go
   vmDir := filepath.Join("/run/cocoon/vms", vmID)
   os.MkdirAll(vmDir, 0755)
   ```

2. **Start CH process**:
   ```go
   cmd, _ := LaunchCloudHypervisor(vmID, config)
   err := cmd.Start()
   if err != nil {
       return fmt.Errorf("failed to start cloud-hypervisor: %w", err)
   }
   ```

3. **Wait for socket to appear**:
   ```go
   socketPath := GetVMSocketPath(vmID)
   timeout := time.After(5 * time.Second)
   ticker := time.NewTicker(100 * time.Millisecond)
   defer ticker.Stop()

   for {
       select {
       case <-ticker.C:
           if _, err := os.Stat(socketPath); err == nil {
               // Socket exists, CH is ready
               goto SocketReady
           }
       case <-timeout:
           return fmt.Errorf("timeout waiting for CH socket")
       }
   }
   SocketReady:
   ```

4. **Store PID in metadata**:
   ```go
   WritePID(vmID, cmd.Process.Pid)

   metadata := &VMMetadata{
       VMID:       vmID,
       State:      "booting",
       ProcessPID: cmd.Process.Pid,
       SocketPath: socketPath,
       SerialLog:  serialLog,
       CreatedAt:  time.Now(),
       UpdatedAt:  time.Now(),
   }
   SaveMetadata(vmID, metadata)
   ```

### 3.2 Monitoring Process Health

**Check if CH process is alive**:

```go
func IsProcessAlive(pid int) bool {
    process, err := os.FindProcess(pid)
    if err != nil {
        return false
    }

    // Send signal 0 (no-op) to check process existence
    err = process.Signal(syscall.Signal(0))
    return err == nil
}

func MonitorVM(vmID string) error {
    pid, err := ReadPID(vmID)
    if err != nil {
        return fmt.Errorf("failed to read PID: %w", err)
    }

    if !IsProcessAlive(pid) {
        return fmt.Errorf("CH process (PID %d) not running", pid)
    }

    return nil
}
```

**Check socket connectivity**:

```go
func CheckSocketConnectivity(socketPath string) error {
    conn, err := net.Dial("unix", socketPath)
    if err != nil {
        return fmt.Errorf("socket not accessible: %w", err)
    }
    conn.Close()
    return nil
}
```

### 3.3 Graceful Shutdown

**Step 1: Send ACPI Shutdown via API**:

```go
func ShutdownVM(vmID string, timeout time.Duration) error {
    client := NewCHClient(GetVMSocketPath(vmID))

    // Send ACPI power button event
    err := client.PowerButton()
    if err != nil {
        return fmt.Errorf("failed to send ACPI shutdown: %w", err)
    }

    // Wait for process to exit gracefully
    pid, _ := ReadPID(vmID)
    deadline := time.Now().Add(timeout)

    for time.Now().Before(deadline) {
        if !IsProcessAlive(pid) {
            return nil // Process exited
        }
        time.Sleep(500 * time.Millisecond)
    }

    // Timeout reached, force kill
    return forceKillProcess(pid)
}
```

**Step 2: Force Kill on Timeout**:

```go
func forceKillProcess(pid int) error {
    process, err := os.FindProcess(pid)
    if err != nil {
        return err
    }

    // Send SIGKILL
    err = process.Signal(syscall.SIGKILL)
    if err != nil {
        return fmt.Errorf("failed to kill process: %w", err)
    }

    // Wait for process to actually exit
    _, err = process.Wait()
    return err
}
```

### 3.4 VM Deletion Flow

**Complete cleanup sequence**:

```go
func DeleteVM(vmID string, force bool) error {
    metadata, err := LoadMetadata(vmID)
    if err != nil {
        return fmt.Errorf("VM not found: %w", err)
    }

    // Step 1: Stop VM if running
    if metadata.State == "running" || metadata.State == "booting" {
        if !force {
            return fmt.Errorf("VM is running, use --force to delete")
        }
        err := ShutdownVM(vmID, 10*time.Second)
        if err != nil {
            // Force kill if graceful shutdown fails
            forceKillProcess(metadata.ProcessPID)
        }
    }

    // Step 2: Send vm.delete to CH (if socket still exists)
    socketPath := GetVMSocketPath(vmID)
    if _, err := os.Stat(socketPath); err == nil {
        client := NewCHClient(socketPath)
        client.DeleteVM() // Ignore errors, we're deleting anyway
    }

    // Step 3: Remove VM directory
    vmDir := filepath.Join("/run/cocoon/vms", vmID)
    err = os.RemoveAll(vmDir)
    if err != nil {
        return fmt.Errorf("failed to remove VM directory: %w", err)
    }

    return nil
}
```

### 3.5 Crash Detection

**Detect if CH process died unexpectedly**:

```go
func DetectCrash(vmID string) (bool, error) {
    metadata, err := LoadMetadata(vmID)
    if err != nil {
        return false, err
    }

    // Check if metadata says VM is running
    if metadata.State != "running" && metadata.State != "booting" {
        return false, nil // VM is not supposed to be running
    }

    // Check if process is actually alive
    if !IsProcessAlive(metadata.ProcessPID) {
        return true, fmt.Errorf("VM crashed: process %d not found", metadata.ProcessPID)
    }

    return false, nil
}
```

---

## 4. HTTP over Unix Socket in Go

### 4.1 Client Implementation

Cloud Hypervisor exposes its API via HTTP over a Unix socket. Here's how to implement the client in Go:

```go
package client

import (
    "context"
    "fmt"
    "net"
    "net/http"
    "time"
)

type CHClient struct {
    httpClient *http.Client
    baseURL    string
    socketPath string
}

func NewCHClient(socketPath string) *CHClient {
    return &CHClient{
        httpClient: &http.Client{
            Transport: &http.Transport{
                DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
                    // Dial the Unix socket instead of TCP
                    return net.Dial("unix", socketPath)
                },
            },
            Timeout: 30 * time.Second,
        },
        baseURL:    "http://localhost", // Host doesn't matter for Unix sockets
        socketPath: socketPath,
    }
}
```

### 4.2 Making Requests

**PUT request example**:

```go
func (c *CHClient) CreateVM(config *VMConfig) error {
    url := fmt.Sprintf("%s/api/v1/vm.create", c.baseURL)

    jsonData, err := json.Marshal(config)
    if err != nil {
        return err
    }

    req, err := http.NewRequest("PUT", url, bytes.NewBuffer(jsonData))
    if err != nil {
        return err
    }
    req.Header.Set("Content-Type", "application/json")

    resp, err := c.httpClient.Do(req)
    if err != nil {
        return fmt.Errorf("failed to send request: %w", err)
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusNoContent {
        body, _ := io.ReadAll(resp.Body)
        return fmt.Errorf("CH returned error: %d - %s", resp.StatusCode, string(body))
    }

    return nil
}
```

**GET request example**:

```go
func (c *CHClient) GetVMInfo() (*VMInfo, error) {
    url := fmt.Sprintf("%s/api/v1/vm.info", c.baseURL)

    resp, err := c.httpClient.Get(url)
    if err != nil {
        return nil, fmt.Errorf("failed to get VM info: %w", err)
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        return nil, fmt.Errorf("CH returned status %d", resp.StatusCode)
    }

    var info VMInfo
    err = json.NewDecoder(resp.Body).Decode(&info)
    if err != nil {
        return nil, fmt.Errorf("failed to decode response: %w", err)
    }

    return &info, nil
}
```

### 4.3 Context and Timeout Handling

**With context**:

```go
func (c *CHClient) CreateVMWithContext(ctx context.Context, config *VMConfig) error {
    url := fmt.Sprintf("%s/api/v1/vm.create", c.baseURL)

    jsonData, err := json.Marshal(config)
    if err != nil {
        return err
    }

    req, err := http.NewRequestWithContext(ctx, "PUT", url, bytes.NewBuffer(jsonData))
    if err != nil {
        return err
    }
    req.Header.Set("Content-Type", "application/json")

    resp, err := c.httpClient.Do(req)
    if err != nil {
        return fmt.Errorf("failed to send request: %w", err)
    }
    defer resp.Body.Close()

    // Handle response...
    return nil
}
```

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
type VMConfig struct {
    CPUs     CPUConfig     `json:"cpus"`
    Memory   MemoryConfig  `json:"memory"`
    Disks    []DiskConfig  `json:"disks,omitempty"`
    Serial   SerialConfig  `json:"serial"`
    Console  ConsoleConfig `json:"console"`
}

type CPUConfig struct {
    BootVCPUs int `json:"boot_vcpus"`
}

type MemoryConfig struct {
    Size int64 `json:"size"` // In bytes
}

type DiskConfig struct {
    Path     string `json:"path"`
    ReadOnly bool   `json:"readonly,omitempty"`
}

type SerialConfig struct {
    Mode string `json:"mode"` // "Null", "Tty", "File"
    File string `json:"file,omitempty"`
}

type ConsoleConfig struct {
    Mode string `json:"mode"` // "Off", "Tty", "File"
}
```

**Implementation**:
```go
func (c *CHClient) CreateVM(config *VMConfig) error {
    url := fmt.Sprintf("%s/api/v1/vm.create", c.baseURL)
    jsonData, _ := json.Marshal(config)

    req, _ := http.NewRequest("PUT", url, bytes.NewBuffer(jsonData))
    req.Header.Set("Content-Type", "application/json")

    resp, err := c.httpClient.Do(req)
    if err != nil {
        return err
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusNoContent {
        return fmt.Errorf("create failed: %d", resp.StatusCode)
    }

    return nil
}
```

#### PUT /api/v1/vm.boot

**Purpose**: Boot the VM (after vm.create)

**Request**: No body

**Implementation**:
```go
func (c *CHClient) BootVM() error {
    url := fmt.Sprintf("%s/api/v1/vm.boot", c.baseURL)
    req, _ := http.NewRequest("PUT", url, nil)

    resp, err := c.httpClient.Do(req)
    if err != nil {
        return err
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusNoContent {
        return fmt.Errorf("boot failed: %d", resp.StatusCode)
    }

    return nil
}
```

#### PUT /api/v1/vm.shutdown

**Purpose**: Gracefully shutdown the VM (ACPI power button)

**Request**: No body

**Implementation**:
```go
func (c *CHClient) ShutdownVM() error {
    url := fmt.Sprintf("%s/api/v1/vm.shutdown", c.baseURL)
    req, _ := http.NewRequest("PUT", url, nil)

    resp, err := c.httpClient.Do(req)
    if err != nil {
        return err
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusNoContent {
        return fmt.Errorf("shutdown failed: %d", resp.StatusCode)
    }

    return nil
}
```

#### PUT /api/v1/vm.power-button

**Purpose**: Send ACPI power button event (same as vm.shutdown)

**Implementation**:
```go
func (c *CHClient) PowerButton() error {
    url := fmt.Sprintf("%s/api/v1/vm.power-button", c.baseURL)
    req, _ := http.NewRequest("PUT", url, nil)

    resp, err := c.httpClient.Do(req)
    if err != nil {
        return err
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusNoContent {
        return fmt.Errorf("power-button failed: %d", resp.StatusCode)
    }

    return nil
}
```

#### PUT /api/v1/vm.delete

**Purpose**: Delete the VM and free resources

**Request**: No body

**Implementation**:
```go
func (c *CHClient) DeleteVM() error {
    url := fmt.Sprintf("%s/api/v1/vm.delete", c.baseURL)
    req, _ := http.NewRequest("PUT", url, nil)

    resp, err := c.httpClient.Do(req)
    if err != nil {
        return err
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusNoContent {
        return fmt.Errorf("delete failed: %d", resp.StatusCode)
    }

    return nil
}
```

#### GET /api/v1/vm.info

**Purpose**: Get current VM information

**Response**:
```go
type VMInfo struct {
    Config VMConfig     `json:"config"`
    State  string       `json:"state"` // "Created", "Running", "Shutdown", "Paused"
    Memory MemoryStatus `json:"memory_actual_size"`
}
```

**Implementation**:
```go
func (c *CHClient) GetVMInfo() (*VMInfo, error) {
    url := fmt.Sprintf("%s/api/v1/vm.info", c.baseURL)

    resp, err := c.httpClient.Get(url)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        return nil, fmt.Errorf("get info failed: %d", resp.StatusCode)
    }

    var info VMInfo
    err = json.NewDecoder(resp.Body).Decode(&info)
    return &info, err
}
```

### 5.3 Error Handling and Retries

**Retry logic for transient failures**:

```go
func (c *CHClient) BootVMWithRetry(maxRetries int) error {
    var lastErr error

    for i := 0; i < maxRetries; i++ {
        err := c.BootVM()
        if err == nil {
            return nil
        }

        lastErr = err

        // Check if error is retryable
        if strings.Contains(err.Error(), "connection refused") {
            time.Sleep(time.Duration(i+1) * time.Second)
            continue
        }

        // Non-retryable error
        return err
    }

    return fmt.Errorf("boot failed after %d retries: %w", maxRetries, lastErr)
}
```

### 5.4 Complete API Client Interface

```go
package client

type CloudHypervisorAPI interface {
    // Lifecycle
    CreateVM(config *VMConfig) error
    BootVM() error
    ShutdownVM() error
    PowerButton() error
    DeleteVM() error

    // Information
    GetVMInfo() (*VMInfo, error)

    // Resource management
    ResizeVM(cpus int, memory int64) error

    // Advanced
    PauseVM() error
    ResumeVM() error
    SnapshotVM(path string) error
}
```

---

## 6. Crash Recovery and Reconciliation

### 6.1 Startup Reconciliation

When Cocoon starts, it must reconcile the expected state (metadata) with the actual state (running processes).

**Reconciliation sequence**:

```go
func ReconcileOnStartup() error {
    // Step 1: Scan /run/cocoon/vms/ for VM directories
    vmDirs, err := os.ReadDir("/run/cocoon/vms")
    if err != nil {
        return err
    }

    for _, vmDir := range vmDirs {
        vmID := vmDir.Name()

        // Step 2: Load metadata
        metadata, err := LoadMetadata(vmID)
        if err != nil {
            log.Printf("WARNING: Failed to load metadata for %s: %v", vmID, err)
            continue
        }

        // Step 3: Check if CH process is alive
        processAlive := IsProcessAlive(metadata.ProcessPID)

        // Step 4: Reconcile state
        switch metadata.State {
        case "running", "booting":
            if !processAlive {
                log.Printf("CRASH DETECTED: VM %s (PID %d) crashed", vmID, metadata.ProcessPID)
                handleCrashedVM(vmID, metadata)
            } else {
                log.Printf("VM %s is running (PID %d)", vmID, metadata.ProcessPID)
            }

        case "stopped":
            if processAlive {
                log.Printf("ORPHAN DETECTED: VM %s stopped but process %d still running", vmID, metadata.ProcessPID)
                forceKillProcess(metadata.ProcessPID)
            }
            // Clean up stopped VM resources
            cleanupStoppedVM(vmID)
        }
    }

    return nil
}
```

### 6.2 Crash Handling

**Handle crashed VMs**:

```go
func handleCrashedVM(vmID string, metadata *VMMetadata) error {
    log.Printf("Handling crashed VM: %s", vmID)

    // Step 1: Update metadata state
    metadata.State = "crashed"
    metadata.UpdatedAt = time.Now()
    SaveMetadata(vmID, metadata)

    // Step 2: Clean up socket (if exists)
    socketPath := GetVMSocketPath(vmID)
    os.Remove(socketPath)

    // Step 3: Archive serial log for debugging
    serialLog := GetVMSerialLogPath(vmID)
    if _, err := os.Stat(serialLog); err == nil {
        archivePath := fmt.Sprintf("%s.crash-%d", serialLog, time.Now().Unix())
        os.Rename(serialLog, archivePath)
        log.Printf("Serial log archived: %s", archivePath)
    }

    // Step 4: Optionally attempt restart
    // (if restart policy is enabled)

    return nil
}
```

### 6.3 Orphaned Socket Cleanup

**Remove stale sockets**:

```go
func cleanupOrphanedSockets() error {
    vmDirs, err := os.ReadDir("/run/cocoon/vms")
    if err != nil {
        return err
    }

    for _, vmDir := range vmDirs {
        vmID := vmDir.Name()
        socketPath := GetVMSocketPath(vmID)

        // Check if socket exists
        if _, err := os.Stat(socketPath); err != nil {
            continue // Socket doesn't exist
        }

        // Try to connect to socket
        err = CheckSocketConnectivity(socketPath)
        if err != nil {
            // Socket is stale, remove it
            log.Printf("Removing stale socket: %s", socketPath)
            os.Remove(socketPath)
        }
    }

    return nil
}
```

### 6.4 PID File Validation

**Ensure PID files are accurate**:

```go
func validatePIDFile(vmID string) error {
    pid, err := ReadPID(vmID)
    if err != nil {
        return err
    }

    if !IsProcessAlive(pid) {
        return fmt.Errorf("PID %d not alive", pid)
    }

    // Check if PID is actually a cloud-hypervisor process
    cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
    if err != nil {
        return fmt.Errorf("failed to read cmdline: %w", err)
    }

    if !strings.Contains(string(cmdline), "cloud-hypervisor") {
        return fmt.Errorf("PID %d is not cloud-hypervisor", pid)
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
    "fmt"
    "log"
    "time"
)

func main() {
    vmID := "vm-example-001"

    // Step 1: Create VM directory
    err := SetupVMDirectory(vmID)
    if err != nil {
        log.Fatalf("Failed to setup VM directory: %v", err)
    }

    // Step 2: Launch Cloud Hypervisor process
    config := &VMConfig{
        DiskPath:     "/var/lib/cocoon/images/vm-example-001.qcow2",
        CPUs:         2,
        MemoryMB:     2048,
        // Cloud-init data served via metadata server at 169.254.169.254
    }

    cmd, err := LaunchCloudHypervisor(vmID, config)
    if err != nil {
        log.Fatalf("Failed to launch CH: %v", err)
    }

    // Step 3: Wait for socket
    socketPath := GetVMSocketPath(vmID)
    err = WaitForSocket(socketPath, 5*time.Second)
    if err != nil {
        log.Fatalf("Socket not ready: %v", err)
    }

    // Step 4: Store metadata
    metadata := &VMMetadata{
        VMID:       vmID,
        State:      "booting",
        ProcessPID: cmd.Process.Pid,
        SocketPath: socketPath,
        CreatedAt:  time.Now(),
    }
    SaveMetadata(vmID, metadata)

    // Step 5: Boot VM via API
    client := NewCHClient(socketPath)
    err = client.BootVM()
    if err != nil {
        log.Fatalf("Failed to boot VM: %v", err)
    }

    // Step 6: Update state
    metadata.State = "running"
    SaveMetadata(vmID, metadata)

    log.Printf("VM %s started successfully (PID %d)", vmID, cmd.Process.Pid)

    // Step 7: Monitor VM
    time.Sleep(60 * time.Second)

    // Step 8: Shutdown VM
    err = ShutdownVM(vmID, 30*time.Second)
    if err != nil {
        log.Fatalf("Failed to shutdown VM: %v", err)
    }

    // Step 9: Cleanup
    err = DeleteVM(vmID, false)
    if err != nil {
        log.Fatalf("Failed to delete VM: %v", err)
    }

    log.Printf("VM %s lifecycle complete", vmID)
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
    vmID := "vm-test-123"
    expected := "/run/cocoon/vms/vm-test-123/api.sock"

    actual := GetVMSocketPath(vmID)

    if actual != expected {
        t.Errorf("Expected %s, got %s", expected, actual)
    }
}

func TestPIDFileOperations(t *testing.T) {
    vmID := "vm-test-456"
    testPID := 12345

    // Write PID
    err := WritePID(vmID, testPID)
    if err != nil {
        t.Fatalf("Failed to write PID: %v", err)
    }

    // Read PID
    readPID, err := ReadPID(vmID)
    if err != nil {
        t.Fatalf("Failed to read PID: %v", err)
    }

    if readPID != testPID {
        t.Errorf("Expected PID %d, got %d", testPID, readPID)
    }

    // Cleanup
    os.RemoveAll(filepath.Join("/run/cocoon/vms", vmID))
}
```

### 8.2 Integration Tests

Test with real Cloud Hypervisor:

```go
func TestVMLifecycle(t *testing.T) {
    if testing.Short() {
        t.Skip("Skipping integration test")
    }

    vmID := "vm-integration-test"

    // Create and start VM
    err := CreateAndStartVM(vmID)
    if err != nil {
        t.Fatalf("Failed to create VM: %v", err)
    }

    // Verify VM is running
    metadata, _ := LoadMetadata(vmID)
    if !IsProcessAlive(metadata.ProcessPID) {
        t.Errorf("VM process not alive")
    }

    // Shutdown VM
    err = ShutdownVM(vmID, 30*time.Second)
    if err != nil {
        t.Errorf("Failed to shutdown VM: %v", err)
    }

    // Cleanup
    DeleteVM(vmID, true)
}
```

### 8.3 Crash Recovery Tests

```go
func TestCrashRecovery(t *testing.T) {
    vmID := "vm-crash-test"

    // Start VM
    CreateAndStartVM(vmID)

    // Simulate crash by killing process
    pid, _ := ReadPID(vmID)
    syscall.Kill(pid, syscall.SIGKILL)

    // Wait for process to die
    time.Sleep(1 * time.Second)

    // Run reconciliation
    err := ReconcileOnStartup()
    if err != nil {
        t.Errorf("Reconciliation failed: %v", err)
    }

    // Verify crash was detected
    metadata, _ := LoadMetadata(vmID)
    if metadata.State != "crashed" {
        t.Errorf("Expected state 'crashed', got '%s'", metadata.State)
    }
}
```

---

## Summary

This document provides a comprehensive guide to integrating Cloud Hypervisor with Cocoon:

1. **Process Model**: One CH process per VM for strong isolation
2. **Socket Organization**: `/run/cocoon/vms/{vm-id}/` structure
3. **Lifecycle Management**: Launch, monitor, shutdown, delete
4. **HTTP Client**: HTTP over Unix socket implementation
5. **API Mapping**: Complete REST API integration
6. **Crash Recovery**: Startup reconciliation and orphan cleanup

**Implementation Priority**:
1. Socket management and process lifecycle (P0)
2. HTTP client and API integration (P0)
3. Crash detection and recovery (P1)
4. Testing and validation (P1)

**Next Steps**:
- Implement the CHClient package
- Create VM lifecycle manager
- Build reconciliation system
- Write comprehensive tests

---

**References**:
- Cloud Hypervisor API: https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/vmm/src/api/openapi/cloud-hypervisor.yaml
- Boot Contract: `docs/01-boot-contract.md`
- Installation Guide: `docs/02-installation.md`

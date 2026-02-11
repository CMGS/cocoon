# Boot Contract Specification

**Version**: 1.0
**Status**: Draft
**Priority**: P0 - CRITICAL FOUNDATION DOCUMENT

## Executive Summary

This document defines the **Boot Contract** - the core specification for how Cocoon transforms OCI container images into bootable virtual machines using Cloud Hypervisor. Without this contract, the entire Cocoon project cannot function.

The boot contract establishes:
1. How VMs are configured and booted (UEFI vs Direct Kernel)
2. How the guest OS initializes and runs agent tasks
3. How I/O channels (serial, vsock, virtiofs) are configured
4. How VM lifecycle operations (start, stop, delete) behave
5. What an OCI image must contain to be bootable

## Table of Contents

1. [Boot Path Decision](#1-boot-path-decision)
2. [Guest Init Model](#2-guest-init-model)
3. [I/O Mechanisms](#3-io-mechanisms)
4. [Lifecycle Semantics](#4-lifecycle-semantics)
5. [VM Configuration Schema](#5-vm-configuration-schema)
6. [OCI to Bootable Bridge](#6-oci-to-bootable-bridge)
7. [Implementation Checklist](#7-implementation-checklist)

---

## 1. Boot Path Decision

### 1.1 Selected Boot Mode: **UEFI Boot**

**Decision**: Cocoon uses **UEFI boot** as the primary boot method for maximum OS compatibility and production readiness.

**Rationale**:
- ✅ **Universal OS support**: Works with Ubuntu, Debian, Fedora, Alpine, and any modern Linux distribution
- ✅ **Standard firmware interface**: UEFI is the industry-standard firmware interface
- ✅ **Bootloader flexibility**: Supports GRUB, systemd-boot, and other standard bootloaders
- ✅ **Production proven**: Used by all major cloud providers (AWS, GCP, Azure)
- ✅ **Secure Boot capable**: Enables future secure boot support

**Alternative (Not Selected)**: Direct Kernel Boot (PVH)
- ❌ Requires pre-extracting kernel and initrd from images
- ❌ More complex boot argument management
- ❌ Limited OS compatibility (kernel must support PVH)
- ⚠️ May be added as an optimization path later

### 1.2 UEFI Boot Configuration

#### 1.2.1 Firmware Requirements

```go
type UEFIConfig struct {
    // Firmware path - must be OVMF (Open Virtual Machine Firmware)
    FirmwarePath string // Default: "/usr/share/OVMF/OVMF_CODE.fd"

    // NVRAM template for UEFI variables (optional, Cloud Hypervisor handles this)
    NVRAMTemplate string // Default: "" (Cloud Hypervisor creates ephemeral NVRAM)

    // UEFI boot order (implicitly handled by disk configuration)
    // Cloud Hypervisor boots from first disk with EFI System Partition
}
```

#### 1.2.2 Required UEFI Components

For a VM to boot via UEFI, the root disk **must contain**:

1. **EFI System Partition (ESP)**:
   - Type: FAT32
   - Mounted at: `/boot/efi` (or `/efi`)
   - Contains: UEFI bootloader (e.g., `/EFI/BOOT/BOOTX64.EFI`)
   - Size: Minimum 100MB

2. **UEFI Bootloader**:
   - GRUB2: Most common, installed in ESP
   - systemd-boot: Lightweight alternative
   - Must be configured to load kernel and initrd

3. **Boot Configuration**:
   - GRUB: `/boot/grub/grub.cfg`
   - systemd-boot: `/boot/loader/entries/*.conf`

#### 1.2.3 Cloud Hypervisor UEFI Boot Command

```bash
cloud-hypervisor \
  --api-socket /var/run/cocoon/vm-123.sock \
  --disk path=/var/lib/cocoon/images/vm-123.qcow2 \
  --cpus boot=2 \
  --memory size=2G \
  --serial tty \
  --console off \
  --log-file /var/log/cocoon/vm-123.log
```

**Note**: Cloud Hypervisor automatically enables UEFI boot when no `--kernel` is specified.

### 1.3 Future: Direct Kernel Boot (Optional Optimization)

Direct kernel boot may be added later for specific use cases:

```go
type DirectKernelConfig struct {
    Kernel  string   // Path to extracted kernel (e.g., /boot/vmlinuz)
    Initrd  string   // Path to initrd (e.g., /boot/initrd.img)
    Cmdline []string // Kernel command line arguments
}

// Example cmdline
cmdline := []string{
    "console=ttyS0",
    "root=/dev/vda1",
    "rw",
    "init=/sbin/init",
}
```

**Use cases for direct kernel boot**:
- Ultra-fast boot times (<50ms) for ephemeral workloads
- Known, fixed kernel versions
- Testing and development

---

## 2. Guest Init Model

### 2.1 PID 1: systemd (Standard Linux Init)

**Decision**: Cocoon guests run **systemd** as PID 1.

**Rationale**:
- ✅ Standard in all modern Linux distributions
- ✅ Robust service management
- ✅ Journal logging (structured logs)
- ✅ Built-in dependency management
- ✅ Socket activation support
- ✅ Graceful shutdown handling

**Alternative (Not Selected)**: Custom minimal init (busybox init, tiny-init)
- ❌ Requires custom image building
- ❌ Less compatible with standard distributions
- ❌ More maintenance burden

### 2.2 Agent Task Injection

The **agent task** is the user-provided command that Cocoon executes inside the VM. There are three injection methods:

#### 2.2.1 Method 1: cloud-init (Recommended for Initial Implementation)

**Mechanism**: Inject task via cloud-init NoCloud datasource

```yaml
# /var/lib/cloud/seed/nocloud/user-data
#cloud-config

runcmd:
  - /usr/local/bin/cocoon-agent

write_files:
  - path: /usr/local/bin/cocoon-agent
    permissions: '0755'
    content: |
      #!/bin/bash
      # Agent task runner
      set -e

      # Read environment from file
      if [ -f /etc/cocoon/env ]; then
        source /etc/cocoon/env
      fi

      # Execute the agent command
      exec $COCOON_COMMAND

  - path: /etc/cocoon/env
    permissions: '0600'
    content: |
      COCOON_COMMAND="python3 /workspace/main.py"
      COCOON_WORKSPACE="/workspace"
      COCOON_TASK_ID="task-123"
```

**Pros**:
- ✅ Standard cloud VM initialization
- ✅ Works with any Linux distribution that has cloud-init
- ✅ No custom guest agent needed
- ✅ Rich configuration options

**Cons**:
- ⚠️ Requires cloud-init installed in OCI image
- ⚠️ Slight boot time overhead (~1-2 seconds)

#### 2.2.2 Method 2: systemd Unit (Future Optimization)

**Mechanism**: Inject a systemd service unit via virtiofs or qcow2 injection

```ini
# /etc/systemd/system/cocoon-agent.service
[Unit]
Description=Cocoon Agent Task
After=network.target

[Service]
Type=oneshot
EnvironmentFile=/etc/cocoon/env
WorkingDirectory=/workspace
ExecStart=/bin/bash -c "$COCOON_COMMAND"
StandardOutput=journal
StandardError=journal
RemainAfterExit=no

[Install]
WantedBy=multi-user.target
```

**Pros**:
- ✅ Fast boot (no cloud-init overhead)
- ✅ Direct systemd integration
- ✅ Better error handling

**Cons**:
- ⚠️ Requires image modification or virtiofs mount

#### 2.2.3 Method 3: vsock Agent (Advanced, Future)

**Mechanism**: Run a Cocoon agent daemon in guest that communicates via vsock

```go
// Guest agent (runs as systemd service)
func main() {
    conn, _ := vsock.Dial(vsock.Host, 9999)

    for {
        task := receiveTask(conn)
        result := executeTask(task)
        sendResult(conn, result)
    }
}
```

**Pros**:
- ✅ Dynamic task injection (no reboot needed)
- ✅ Bidirectional communication
- ✅ Real-time control

**Cons**:
- ⚠️ Requires custom guest agent
- ⚠️ More complex implementation
- ⚠️ Guest agent must be installed in OCI image

### 2.3 Selected Initial Implementation: cloud-init

**For Cocoon v1.0**, use **cloud-init** method for simplicity and compatibility.

**Requirements**:
1. OCI images must have `cloud-init` package installed
2. Cocoon CLI generates cloud-init ISO with task configuration
3. ISO attached as secondary disk during VM creation

**Example Workflow**:

```bash
# 1. Create cloud-init ISO
cocoon internal make-cloud-init-iso \
  --command "python3 /workspace/main.py" \
  --env WORKSPACE=/workspace \
  --output /tmp/cidata-123.iso

# 2. Boot VM with cloud-init disk
cloud-hypervisor \
  --disk path=/var/lib/cocoon/images/vm-123.qcow2 \
  --disk path=/tmp/cidata-123.iso,readonly=on
```

### 2.4 Environment Variables and Arguments

Agent tasks receive configuration via:

1. **Environment Variables** (via cloud-init or systemd):
   ```bash
   COCOON_TASK_ID=task-abc-123
   COCOON_WORKSPACE=/workspace
   COCOON_TIMEOUT=300
   COCOON_MEMORY_LIMIT=2G
   ```

2. **Command-line Arguments** (via `COCOON_COMMAND`):
   ```bash
   COCOON_COMMAND="python3 main.py --input data.json --output result.json"
   ```

3. **Configuration Files** (via virtiofs or cloud-init):
   ```bash
   /etc/cocoon/config.json
   /workspace/task-config.yaml
   ```

---

## 3. I/O Mechanisms

### 3.1 Serial Console (Primary I/O Channel)

**Purpose**: Capture stdout/stderr from guest VM

#### 3.1.1 Configuration

```go
type SerialConfig struct {
    // Serial console mode
    Mode string // "tty" or "file" or "pty"

    // Output file for serial logs
    OutputFile string // e.g., "/var/log/cocoon/vm-123-serial.log"

    // Enable ANSI color passthrough
    EnableColors bool // Default: false
}
```

#### 3.1.2 Cloud Hypervisor Serial Configuration

```bash
# PTY mode (for interactive/debugging)
cloud-hypervisor --serial tty --console off

# File mode (for production log capture)
cloud-hypervisor --serial file=/var/log/cocoon/vm-123-serial.log --console off
```

#### 3.1.3 Guest Kernel Configuration

Ensure guest kernel boots with serial console:

```bash
# GRUB configuration
GRUB_CMDLINE_LINUX="console=tty0 console=ttyS0,115200n8"
```

This outputs to both:
- `tty0`: Virtual screen (unused in headless mode)
- `ttyS0`: Serial port (captured by Cloud Hypervisor)

#### 3.1.4 Collecting Output in Go

```go
func streamSerialOutput(vmID string) error {
    logFile := fmt.Sprintf("/var/log/cocoon/%s-serial.log", vmID)

    file, err := os.Open(logFile)
    if err != nil {
        return err
    }
    defer file.Close()

    scanner := bufio.NewScanner(file)
    for scanner.Scan() {
        line := scanner.Text()

        // Parse and forward to user
        fmt.Println(line)

        // Optional: parse structured logs
        if strings.Contains(line, "COCOON_RESULT:") {
            // Extract structured output
        }
    }

    return scanner.Err()
}
```

#### 3.1.5 Structured Output Protocol

To distinguish agent output from system logs:

```bash
# Guest agent outputs
echo "COCOON_STATUS:RUNNING"
echo "COCOON_PROGRESS:50"
echo "COCOON_RESULT:SUCCESS"
echo "COCOON_OUTPUT_BEGIN"
cat result.json
echo "COCOON_OUTPUT_END"
```

Host-side Go code parses these markers.

### 3.2 vsock (Optional, Future Feature)

**Purpose**: High-performance bidirectional communication between host and guest

#### 3.2.1 Configuration

```go
type VsockConfig struct {
    // vsock Context ID (CID) - auto-assigned by Cloud Hypervisor
    GuestCID uint32 // Cloud Hypervisor assigns, host is always CID 2

    // Host listening port for guest connections
    HostPort uint32 // e.g., 9999
}
```

#### 3.2.2 Use Cases

- **File transfer**: Guest can send large files to host
- **Streaming data**: Real-time metrics, logs, screenshots
- **Control plane**: Host sends commands to guest agent
- **Health checks**: Guest reports liveness via vsock

#### 3.2.3 Cloud Hypervisor vsock Configuration

```bash
cloud-hypervisor \
  --vsock cid=3,socket=/var/run/cocoon/vm-123.vsock
```

#### 3.2.4 Go Implementation (Future)

```go
// Host-side vsock listener
func listenVsock(vmID string) error {
    socketPath := fmt.Sprintf("/var/run/cocoon/%s.vsock", vmID)

    listener, err := vsock.Listen(socketPath)
    if err != nil {
        return err
    }
    defer listener.Close()

    for {
        conn, err := listener.Accept()
        if err != nil {
            continue
        }

        go handleVsockConnection(conn)
    }
}
```

### 3.3 virtiofs (Optional, Future Feature)

**Purpose**: Shared filesystem between host and guest

#### 3.3.1 Configuration

```go
type VirtioFSConfig struct {
    // Host directory to share
    HostPath string // e.g., "/var/lib/cocoon/workspaces/task-123"

    // Guest mount point
    GuestMountPoint string // e.g., "/workspace"

    // Tag for identifying the fs
    Tag string // e.g., "workspace"

    // Read-only or read-write
    ReadOnly bool // Default: false
}
```

#### 3.3.2 Use Cases

- **Input data injection**: Host writes input files, guest reads them
- **Output collection**: Guest writes results, host reads them
- **Shared caching**: Share package caches, build artifacts

#### 3.3.3 Cloud Hypervisor virtiofs Configuration

```bash
cloud-hypervisor \
  --fs tag=workspace,socket=/var/run/cocoon/vm-123-fs.sock

# Separate virtiofsd process
virtiofsd \
  --socket-path=/var/run/cocoon/vm-123-fs.sock \
  --shared-dir=/var/lib/cocoon/workspaces/task-123 \
  --cache=auto
```

#### 3.3.4 Guest-side Mounting

```bash
# In guest (via cloud-init or systemd)
mount -t virtiofs workspace /workspace
```

**Security Note**: virtiofs has known security implications when sharing untrusted data. Use with caution in multi-tenant environments.

### 3.4 Summary: I/O Channel Selection

| Channel | Priority | Use Case | Complexity |
|---------|----------|----------|------------|
| **Serial Console** | **P0 - MUST HAVE** | stdout/stderr capture, structured logs | Low |
| vsock | P1 - Nice to Have | Bidirectional communication, large data transfer | Medium |
| virtiofs | P2 - Future | Shared filesystem, large files | High (security risks) |

**Recommendation**: Implement serial console first. Add vsock when bidirectional communication is needed. Add virtiofs only if use case demands shared filesystem.

---

## 4. Lifecycle Semantics

### 4.1 VM States

```go
type VMState string

const (
    VMStatePending   VMState = "pending"   // Created, not started
    VMStateBooting   VMState = "booting"   // Starting, waiting for guest ready
    VMStateRunning   VMState = "running"   // Guest OS running, agent executing
    VMStateStopping  VMState = "stopping"  // Shutdown initiated
    VMStateStopped   VMState = "stopped"   // VM cleanly shut down
    VMStateError     VMState = "error"     // Boot or runtime error
    VMStateDeleted   VMState = "deleted"   // Resources cleaned up
)
```

### 4.2 cocoon run (Create and Start)

**Command**: `cocoon run IMAGE [COMMAND]`

**Behavior**:

1. **Create VM Configuration**:
   - Allocate VM ID
   - Generate qcow2 disk from OCI image
   - Create cloud-init ISO with task command
   - Generate VM config file

2. **Start Cloud Hypervisor**:
   ```go
   cmd := exec.Command("cloud-hypervisor",
       "--api-socket", socketPath,
       "--disk", fmt.Sprintf("path=%s", diskPath),
       "--disk", fmt.Sprintf("path=%s,readonly=on", cloudInitISO),
       "--cpus", fmt.Sprintf("boot=%d", cpus),
       "--memory", fmt.Sprintf("size=%dM", memoryMB),
       "--serial", fmt.Sprintf("file=%s", serialLog),
       "--console", "off",
   )
   cmd.Start()
   ```

3. **Wait for Boot**:
   - Poll serial log for "login:" prompt or cloud-init completion marker
   - Timeout: 60 seconds (configurable)
   - On timeout: transition to ERROR state

4. **Monitor Execution**:
   - Stream serial output to user
   - Detect agent task completion (via exit markers or vsock)
   - Capture exit code

5. **Auto-cleanup** (if `--rm` flag):
   - Stop VM
   - Delete resources

**Error Handling**:
- Boot timeout: Kill CH process, mark VM as ERROR
- Guest panic: Detect via serial log, mark as ERROR
- CH process crash: Detect via process exit, mark as ERROR

### 4.3 cocoon stop (Graceful Shutdown)

**Command**: `cocoon stop VM_ID [--timeout SECONDS]`

**Behavior**:

1. **Check VM State**:
   - If not RUNNING or BOOTING: error "VM not running"

2. **Send ACPI Shutdown Signal**:
   ```go
   // Via Cloud Hypervisor API
   PUT /api/v1/vm.power-button
   ```
   This is equivalent to pressing physical power button (triggers ACPI shutdown)

3. **Wait for Graceful Shutdown**:
   - Timeout: 30 seconds (default, configurable via `--timeout`)
   - Monitor Cloud Hypervisor process exit
   - Guest systemd will handle shutdown (stops services, unmounts filesystems)

4. **Force Kill on Timeout**:
   ```go
   // If timeout reached and CH still running
   syscall.Kill(chProcess.Pid, syscall.SIGKILL)
   ```

5. **Verify Shutdown**:
   - Wait for CH process exit
   - Mark VM as STOPPED

**Exit Codes**:
- 0: Clean shutdown
- 1: Timeout, force killed
- 2: VM not found or not running

### 4.4 cocoon delete (Remove Resources)

**Command**: `cocoon delete VM_ID [--force]`

**Behavior**:

1. **Check VM State**:
   - If RUNNING: require `--force` flag or error "VM is running"
   - If RUNNING and `--force`: stop VM first

2. **Stop VM** (if `--force`):
   - Call `cocoon stop VM_ID --timeout 10`
   - If stop fails: force kill immediately

3. **Delete Resources**:
   ```go
   // Remove in order
   os.Remove(serialLogPath)      // Delete serial log
   os.Remove(cloudInitISOPath)   // Delete cloud-init ISO
   os.Remove(qcow2DiskPath)      // Delete VM disk
   os.Remove(apiSocketPath)      // Delete API socket
   os.Remove(configPath)         // Delete VM config

   // Remove from VM registry
   db.Delete(vmID)
   ```

4. **Mark as DELETED**:
   - Update state to DELETED
   - Log deletion event

**Safety**:
- Cannot delete RUNNING VM without `--force`
- Confirmation prompt if `--force` is used

### 4.5 cocoon kill (Force Terminate)

**Command**: `cocoon kill VM_ID`

**Behavior**:

1. **Find Cloud Hypervisor Process**:
   ```go
   pid, err := findCHProcess(vmID)
   ```

2. **Send SIGKILL**:
   ```go
   syscall.Kill(pid, syscall.SIGKILL)
   ```

3. **Mark as STOPPED**:
   - VM state: STOPPED
   - Note: Unclean shutdown, filesystem may be inconsistent

**Use Cases**:
- Hung VM that doesn't respond to `stop`
- Emergency cleanup
- Testing failure scenarios

### 4.6 Timeout and Retry Policies

#### 4.6.1 Boot Timeout

```go
type BootTimeoutConfig struct {
    // Maximum time to wait for guest OS to boot
    BootTimeout time.Duration // Default: 60s

    // Maximum retries on boot failure
    MaxBootRetries int // Default: 2

    // Backoff between retries
    RetryBackoff time.Duration // Default: 5s
}
```

**Behavior**:
- If boot times out: kill CH, clean resources, retry (up to MaxBootRetries)
- If all retries fail: mark VM as ERROR, report to user

#### 4.6.2 Stop Timeout

```go
type StopTimeoutConfig struct {
    // Time to wait for graceful ACPI shutdown
    GracefulTimeout time.Duration // Default: 30s

    // If force kill fails, wait before erroring
    ForceKillTimeout time.Duration // Default: 5s
}
```

**Behavior**:
- Send ACPI shutdown, wait GracefulTimeout
- If timeout: send SIGTERM, wait 5s
- If still running: send SIGKILL, wait ForceKillTimeout
- If still running: error "failed to kill VM"

#### 4.6.3 Task Execution Timeout

```go
type TaskTimeoutConfig struct {
    // Maximum time for agent task to complete
    TaskTimeout time.Duration // Default: 300s (5 min)

    // Action on timeout
    OnTimeout TimeoutAction // "stop" or "kill"
}
```

**Behavior**:
- Start timer when VM enters RUNNING state
- If timer expires:
  - If `OnTimeout == "stop"`: graceful stop
  - If `OnTimeout == "kill"`: force kill
- Report timeout error to user

---

## 5. VM Configuration Schema

### 5.1 Complete VMConfig Structure

```go
package config

import "time"

// VMConfig defines the complete configuration for a Cocoon VM
type VMConfig struct {
    // ===== Identity =====
    ID   string `json:"id"`   // Unique VM identifier (generated)
    Name string `json:"name"` // Human-readable name (optional)

    // ===== Boot Configuration =====
    Boot BootConfig `json:"boot"`

    // ===== Disk Configuration =====
    Disk DiskConfig `json:"disk"`

    // ===== Resource Allocation =====
    Resources ResourceConfig `json:"resources"`

    // ===== Runtime Configuration =====
    Runtime RuntimeConfig `json:"runtime"`

    // ===== Agent Task Configuration =====
    Task TaskConfig `json:"task"`

    // ===== I/O Configuration =====
    IO IOConfig `json:"io"`

    // ===== Timeout Configuration =====
    Timeouts TimeoutConfig `json:"timeouts"`

    // ===== Metadata =====
    CreatedAt time.Time `json:"created_at"`
    UpdatedAt time.Time `json:"updated_at"`
}

// BootConfig defines boot method and firmware
type BootConfig struct {
    // Boot mode: "uefi" or "direct-kernel"
    Mode BootMode `json:"mode"`

    // UEFI configuration (if Mode == "uefi")
    UEFI *UEFIConfig `json:"uefi,omitempty"`

    // Direct kernel configuration (if Mode == "direct-kernel")
    DirectKernel *DirectKernelConfig `json:"direct_kernel,omitempty"`
}

type BootMode string

const (
    BootModeUEFI         BootMode = "uefi"
    BootModeDirectKernel BootMode = "direct-kernel"
)

type UEFIConfig struct {
    // Path to OVMF firmware file
    FirmwarePath string `json:"firmware_path"`
    // Default: "/usr/share/OVMF/OVMF_CODE.fd"
}

type DirectKernelConfig struct {
    // Path to extracted kernel file
    Kernel string `json:"kernel"`

    // Path to initrd file
    Initrd string `json:"initrd"`

    // Kernel command line arguments
    Cmdline []string `json:"cmdline"`
}

// DiskConfig defines storage configuration
type DiskConfig struct {
    // Path to qcow2 root disk
    RootDiskPath string `json:"root_disk_path"`

    // Disk size (e.g., "10G", "20GB")
    Size string `json:"size"`

    // Source OCI image
    OCIImage string `json:"oci_image"`

    // Additional data disks (future)
    DataDisks []DataDisk `json:"data_disks,omitempty"`
}

type DataDisk struct {
    Path     string `json:"path"`
    ReadOnly bool   `json:"readonly"`
    Label    string `json:"label"`
}

// ResourceConfig defines CPU and memory allocation
type ResourceConfig struct {
    // Number of vCPUs
    CPUs int `json:"cpus"`

    // Memory in megabytes
    MemoryMB int64 `json:"memory_mb"`

    // CPU topology (future)
    Topology *CPUTopology `json:"topology,omitempty"`
}

type CPUTopology struct {
    Threads  int `json:"threads"`
    Cores    int `json:"cores"`
    Sockets  int `json:"sockets"`
}

// RuntimeConfig defines runtime paths and process info
type RuntimeConfig struct {
    // Cloud Hypervisor API socket path
    APISocket string `json:"api_socket"`

    // Cloud Hypervisor process ID (populated after start)
    ProcessID int `json:"process_id,omitempty"`

    // Working directory for VM files
    WorkDir string `json:"work_dir"`

    // State: pending, booting, running, stopping, stopped, error
    State VMState `json:"state"`
}

// TaskConfig defines the agent task to execute
type TaskConfig struct {
    // Command to execute in guest
    Command []string `json:"command"`

    // Environment variables for the task
    Env map[string]string `json:"env"`

    // Working directory in guest
    WorkingDir string `json:"working_dir"`

    // cloud-init ISO path (generated)
    CloudInitISO string `json:"cloud_init_iso"`
}

// IOConfig defines I/O channels
type IOConfig struct {
    // Serial console configuration
    Serial SerialConfig `json:"serial"`

    // vsock configuration (optional)
    Vsock *VsockConfig `json:"vsock,omitempty"`

    // virtiofs configuration (optional)
    VirtioFS *VirtioFSConfig `json:"virtiofs,omitempty"`
}

type SerialConfig struct {
    // Serial output mode: "file", "tty", "pty"
    Mode string `json:"mode"`

    // Path to serial log file
    LogFile string `json:"log_file"`
}

type VsockConfig struct {
    // Guest Context ID (assigned by Cloud Hypervisor)
    GuestCID uint32 `json:"guest_cid"`

    // Host socket path
    SocketPath string `json:"socket_path"`
}

type VirtioFSConfig struct {
    // Host directory to share
    HostPath string `json:"host_path"`

    // Tag for virtiofs device
    Tag string `json:"tag"`

    // Guest mount point
    MountPoint string `json:"mount_point"`

    // Read-only flag
    ReadOnly bool `json:"readonly"`
}

// TimeoutConfig defines timeout policies
type TimeoutConfig struct {
    // Boot timeout
    Boot time.Duration `json:"boot"`

    // Graceful stop timeout
    Stop time.Duration `json:"stop"`

    // Task execution timeout
    Task time.Duration `json:"task"`
}

type VMState string

const (
    VMStatePending  VMState = "pending"
    VMStateBooting  VMState = "booting"
    VMStateRunning  VMState = "running"
    VMStateStopping VMState = "stopping"
    VMStateStopped  VMState = "stopped"
    VMStateError    VMState = "error"
    VMStateDeleted  VMState = "deleted"
)
```

### 5.2 Default Configuration

```go
// NewDefaultVMConfig creates a VM config with sensible defaults
func NewDefaultVMConfig(id, ociImage string, command []string) *VMConfig {
    return &VMConfig{
        ID:   id,
        Name: "",

        Boot: BootConfig{
            Mode: BootModeUEFI,
            UEFI: &UEFIConfig{
                FirmwarePath: "/usr/share/OVMF/OVMF_CODE.fd",
            },
        },

        Disk: DiskConfig{
            RootDiskPath: fmt.Sprintf("/var/lib/cocoon/images/%s.qcow2", id),
            Size:         "10G",
            OCIImage:     ociImage,
        },

        Resources: ResourceConfig{
            CPUs:     2,
            MemoryMB: 2048,
        },

        Runtime: RuntimeConfig{
            APISocket: fmt.Sprintf("/var/run/cocoon/%s.sock", id),
            WorkDir:   fmt.Sprintf("/var/lib/cocoon/vms/%s", id),
            State:     VMStatePending,
        },

        Task: TaskConfig{
            Command:      command,
            Env:          map[string]string{},
            WorkingDir:   "/root",
            CloudInitISO: fmt.Sprintf("/var/lib/cocoon/cloud-init/%s.iso", id),
        },

        IO: IOConfig{
            Serial: SerialConfig{
                Mode:    "file",
                LogFile: fmt.Sprintf("/var/log/cocoon/%s-serial.log", id),
            },
        },

        Timeouts: TimeoutConfig{
            Boot: 60 * time.Second,
            Stop: 30 * time.Second,
            Task: 300 * time.Second,
        },

        CreatedAt: time.Now(),
        UpdatedAt: time.Now(),
    }
}
```

### 5.3 Configuration Persistence

Configurations are stored as JSON files:

```bash
/var/lib/cocoon/vms/
├── vm-abc-123/
│   ├── config.json          # VMConfig serialized
│   ├── state.json           # Runtime state
│   └── metadata.json        # User metadata, labels
```

---

## 6. OCI to Bootable Bridge

### 6.1 The Critical Question

**Q**: After converting OCI rootfs to qcow2, what makes it bootable?

**A**: The OCI image must contain a **complete bootable Linux system**, including:

1. Bootloader (GRUB or systemd-boot in ESP)
2. Linux kernel (in `/boot`)
3. initrd/initramfs (in `/boot`)
4. Init system (systemd in `/sbin/init`)
5. Essential system utilities (`/bin`, `/usr/bin`)

### 6.2 OCI Image Requirements

#### 6.2.1 Mandatory Components

For an OCI image to be bootable in Cocoon:

| Component | Path | Purpose | Verified By |
|-----------|------|---------|-------------|
| **Bootloader** | `/EFI/BOOT/BOOTX64.EFI` | UEFI boot entry | UEFI firmware |
| **Kernel** | `/boot/vmlinuz-*` | Linux kernel | Bootloader |
| **Initrd** | `/boot/initrd.img-*` | Initial RAM disk | Bootloader |
| **Init** | `/sbin/init` (or `/usr/lib/systemd/systemd`) | PID 1 process | Kernel |
| **Root filesystem** | `/`, `/bin`, `/usr`, `/etc` | Complete OS | Init system |

#### 6.2.2 Testing Bootability

To verify an OCI image is bootable:

```bash
# Check for required files
docker run --rm IMAGE ls -la /boot
docker run --rm IMAGE ls -la /EFI/BOOT
docker run --rm IMAGE ls -la /sbin/init

# Verify bootloader config
docker run --rm IMAGE cat /boot/grub/grub.cfg
```

**Checklist**:
- [ ] Kernel file exists: `/boot/vmlinuz-*`
- [ ] Initrd exists: `/boot/initrd.img-*`
- [ ] UEFI bootloader exists: `/EFI/BOOT/BOOTX64.EFI`
- [ ] GRUB config exists: `/boot/grub/grub.cfg`
- [ ] Init system exists: `/sbin/init`
- [ ] cloud-init installed: `cloud-init --version`

### 6.3 OCI Image Preparation (Image Builder)

#### 6.3.1 Base Image Requirements

Cocoon supports these base images out-of-the-box:

| Distribution | Base Image | Bootable? | Notes |
|--------------|------------|-----------|-------|
| Ubuntu | `ubuntu:22.04` | ✅ Yes | If built with `systemd` and kernel |
| Debian | `debian:12` | ✅ Yes | If built with `systemd` and kernel |
| Fedora | `fedora:39` | ✅ Yes | Full system images are bootable |
| Alpine | `alpine:3.19` | ⚠️ Requires work | Needs bootloader + kernel installation |
| Scratch | `scratch` | ❌ No | No OS, not bootable |

**Warning**: Most official Docker images (e.g., `python:3.11`, `node:20`) are **NOT bootable** because they:
- Lack bootloader
- Lack kernel
- May lack systemd

#### 6.3.2 Creating Bootable Images

**Option A**: Use pre-built cloud images

```dockerfile
# Start from Ubuntu cloud image (bootable)
FROM ubuntu:22.04

# Install cloud-init
RUN apt-get update && apt-get install -y \
    cloud-init \
    linux-image-generic \
    grub-efi-amd64

# Your application
COPY app /app
CMD ["/app/start.sh"]
```

**Option B**: Use Cocoon's `cocoon-base` image (future)

```dockerfile
# Cocoon provides pre-configured bootable base images
FROM cocoon/ubuntu:22.04

# Already has: kernel, grub, systemd, cloud-init
# Just add your app
COPY app /app
```

#### 6.3.3 Kernel and Bootloader Installation

If starting from minimal image:

```dockerfile
FROM ubuntu:22.04

# Install kernel and bootloader
RUN apt-get update && apt-get install -y \
    linux-image-generic \
    grub-efi-amd64 \
    grub-efi-amd64-bin \
    efibootmgr \
    cloud-init \
    systemd

# Install GRUB to /boot/efi
RUN grub-install --target=x86_64-efi --efi-directory=/boot/efi --boot-directory=/boot --removable

# Generate GRUB config
RUN update-grub
```

### 6.4 OCI to qcow2 Conversion Process

#### 6.4.1 High-Level Steps

```
OCI Image (layers) → Rootfs → qcow2 Disk → Bootable VM
```

#### 6.4.2 Detailed Conversion Process

```go
func convertOCIToQcow2(ociImage, outputPath, size string) error {
    // 1. Export OCI image to tar
    rootfsTar := "/tmp/rootfs.tar"
    cmd := exec.Command("docker", "export",
        "$(docker create " + ociImage + ")",
        "-o", rootfsTar)
    cmd.Run()

    // 2. Create empty qcow2 disk
    cmd = exec.Command("qemu-img", "create", "-f", "qcow2", outputPath, size)
    cmd.Run()

    // 3. Create partition table and filesystem
    cmd = exec.Command("virt-make-fs",
        "--format=qcow2",
        "--type=ext4",
        "--partition=gpt",
        "--size=" + size,
        rootfsTar,
        outputPath)
    cmd.Run()

    // 4. Install bootloader (if needed)
    cmd = exec.Command("virt-customize",
        "-a", outputPath,
        "--run-command", "grub-install /dev/vda",
        "--run-command", "update-grub")
    cmd.Run()

    // 5. Verify bootability
    if err := verifyBootable(outputPath); err != nil {
        return fmt.Errorf("image not bootable: %w", err)
    }

    return nil
}

func verifyBootable(qcow2Path string) error {
    // Use guestfish to verify
    cmd := exec.Command("guestfish", "-a", qcow2Path, "-i",
        "sh", "test -f /boot/vmlinuz-* && test -f /sbin/init")
    return cmd.Run()
}
```

#### 6.4.3 Tools Used

| Tool | Purpose | Package |
|------|---------|---------|
| `qemu-img` | Create qcow2 images | `qemu-utils` |
| `virt-make-fs` | Convert directory to disk | `libguestfs-tools` |
| `virt-customize` | Modify VM images | `libguestfs-tools` |
| `guestfish` | Inspect and modify images | `libguestfs-tools` |

### 6.5 Boot Verification

After conversion, Cocoon performs boot verification:

```go
func verifyBootContract(qcow2Path string) error {
    checks := []struct {
        name string
        cmd  []string
    }{
        {"Kernel exists", []string{"test", "-f", "/boot/vmlinuz-*"}},
        {"Initrd exists", []string{"test", "-f", "/boot/initrd.img-*"}},
        {"GRUB exists", []string{"test", "-f", "/boot/grub/grub.cfg"}},
        {"EFI bootloader", []string{"test", "-f", "/EFI/BOOT/BOOTX64.EFI"}},
        {"Init system", []string{"test", "-x", "/sbin/init"}},
        {"cloud-init", []string{"which", "cloud-init"}},
    }

    for _, check := range checks {
        cmd := exec.Command("guestfish", "-a", qcow2Path, "-i", "sh", strings.Join(check.cmd, " "))
        if err := cmd.Run(); err != nil {
            return fmt.Errorf("boot contract violation: %s failed", check.name)
        }
    }

    return nil
}
```

### 6.6 Boot Contract Summary

**Contract**: An OCI image is bootable in Cocoon if and only if:

1. ✅ Contains Linux kernel in `/boot/vmlinuz-*`
2. ✅ Contains initrd in `/boot/initrd.img-*`
3. ✅ Contains UEFI bootloader in `/EFI/BOOT/BOOTX64.EFI`
4. ✅ Contains GRUB config in `/boot/grub/grub.cfg` (or systemd-boot equivalent)
5. ✅ Contains init system at `/sbin/init`
6. ✅ Contains cloud-init package (for task injection)
7. ✅ Root filesystem has standard Linux FHS structure

**Violation**: If any requirement is missing, Cocoon will:
- Refuse to boot the image
- Provide clear error message indicating missing component
- Suggest remediation steps

---

## 7. Implementation Checklist

### 7.1 Phase 1: Minimal Bootable VM (P0)

- [ ] **Boot**:
  - [ ] Implement UEFI boot with Cloud Hypervisor
  - [ ] Detect OVMF firmware location
  - [ ] Handle boot timeout (60s default)

- [ ] **OCI to qcow2**:
  - [ ] Implement OCI image export
  - [ ] Implement qcow2 creation with `virt-make-fs`
  - [ ] Verify boot contract (kernel, bootloader, init)

- [ ] **Serial I/O**:
  - [ ] Implement serial log capture
  - [ ] Stream serial output to user
  - [ ] Parse structured output markers

- [ ] **cloud-init**:
  - [ ] Generate cloud-init ISO
  - [ ] Inject agent task command
  - [ ] Inject environment variables

- [ ] **Lifecycle**:
  - [ ] Implement `cocoon run` (create + start)
  - [ ] Implement `cocoon stop` (ACPI shutdown + timeout)
  - [ ] Implement `cocoon delete` (resource cleanup)
  - [ ] Implement `cocoon kill` (force terminate)

### 7.2 Phase 2: Production Readiness (P1)

- [ ] **Monitoring**:
  - [ ] VM state transitions
  - [ ] Boot time metrics
  - [ ] Task execution time
  - [ ] Resource usage (CPU, memory)

- [ ] **Error Handling**:
  - [ ] Boot failure retry logic
  - [ ] Guest panic detection
  - [ ] Cloud Hypervisor crash handling

- [ ] **Logging**:
  - [ ] Structured logs for all operations
  - [ ] Separate logs for host and guest
  - [ ] Log rotation

- [ ] **Testing**:
  - [ ] Boot contract verification tests
  - [ ] Lifecycle operation tests
  - [ ] Timeout and failure tests
  - [ ] Multiple VMs concurrency tests

### 7.3 Phase 3: Advanced Features (P2)

- [ ] **vsock**:
  - [ ] Implement vsock listener
  - [ ] Guest agent communication protocol
  - [ ] File transfer over vsock

- [ ] **virtiofs**:
  - [ ] Integrate virtiofsd
  - [ ] Workspace sharing
  - [ ] Security evaluation

- [ ] **Direct Kernel Boot**:
  - [ ] Extract kernel from OCI image
  - [ ] Generate kernel command line
  - [ ] Benchmark vs UEFI boot

- [ ] **Optimizations**:
  - [ ] Image caching
  - [ ] Copy-on-write disk snapshots
  - [ ] Pre-warmed VMs

---

## 8. References

### 8.1 External Documentation

- **Cloud Hypervisor API**: https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/vmm/src/api/openapi/cloud-hypervisor.yaml
- **UEFI Specification**: https://uefi.org/specifications
- **cloud-init Documentation**: https://cloudinit.readthedocs.io/
- **libguestfs Tools**: https://libguestfs.org/
- **virtio Specification**: https://docs.oasis-open.org/virtio/virtio/v1.1/virtio-v1.1.html

### 8.2 Related Cocoon Documents

- `00-overview.md`: Project overview (if exists)
- `02-runtime-architecture.md`: Runtime and VM management (to be written)
- `03-cli-design.md`: CLI interface design (to be written)

---

## Appendix A: Example Configurations

### A.1 Minimal Configuration

```json
{
  "id": "vm-abc-123",
  "boot": {
    "mode": "uefi",
    "uefi": {
      "firmware_path": "/usr/share/OVMF/OVMF_CODE.fd"
    }
  },
  "disk": {
    "root_disk_path": "/var/lib/cocoon/images/vm-abc-123.qcow2",
    "size": "10G",
    "oci_image": "ubuntu:22.04"
  },
  "resources": {
    "cpus": 2,
    "memory_mb": 2048
  },
  "task": {
    "command": ["python3", "/workspace/main.py"],
    "env": {
      "WORKSPACE": "/workspace"
    },
    "working_dir": "/root"
  }
}
```

### A.2 Full-Featured Configuration

```json
{
  "id": "vm-xyz-789",
  "name": "ml-training-job",
  "boot": {
    "mode": "uefi",
    "uefi": {
      "firmware_path": "/usr/share/OVMF/OVMF_CODE.fd"
    }
  },
  "disk": {
    "root_disk_path": "/var/lib/cocoon/images/vm-xyz-789.qcow2",
    "size": "50G",
    "oci_image": "python:3.11-ubuntu",
    "data_disks": [
      {
        "path": "/var/lib/cocoon/data/dataset.qcow2",
        "readonly": true,
        "label": "dataset"
      }
    ]
  },
  "resources": {
    "cpus": 8,
    "memory_mb": 16384,
    "topology": {
      "threads": 2,
      "cores": 4,
      "sockets": 1
    }
  },
  "runtime": {
    "api_socket": "/var/run/cocoon/vm-xyz-789.sock",
    "work_dir": "/var/lib/cocoon/vms/vm-xyz-789",
    "state": "running",
    "process_id": 12345
  },
  "task": {
    "command": ["python3", "-u", "train.py", "--epochs", "100"],
    "env": {
      "CUDA_VISIBLE_DEVICES": "0",
      "WORKSPACE": "/workspace",
      "DATASET": "/mnt/dataset"
    },
    "working_dir": "/workspace",
    "cloud_init_iso": "/var/lib/cocoon/cloud-init/vm-xyz-789.iso"
  },
  "io": {
    "serial": {
      "mode": "file",
      "log_file": "/var/log/cocoon/vm-xyz-789-serial.log"
    },
    "vsock": {
      "guest_cid": 3,
      "socket_path": "/var/run/cocoon/vm-xyz-789.vsock"
    },
    "virtiofs": {
      "host_path": "/var/lib/cocoon/workspaces/vm-xyz-789",
      "tag": "workspace",
      "mount_point": "/workspace",
      "readonly": false
    }
  },
  "timeouts": {
    "boot": "60s",
    "stop": "30s",
    "task": "7200s"
  },
  "created_at": "2024-01-15T10:30:00Z",
  "updated_at": "2024-01-15T10:30:05Z"
}
```

---

## Appendix B: Troubleshooting

### B.1 Boot Failures

**Symptom**: VM fails to boot, timeout after 60 seconds

**Possible Causes**:
1. Missing bootloader in OCI image
2. Missing kernel in `/boot`
3. Incorrect GRUB configuration
4. OVMF firmware not found

**Diagnosis**:
```bash
# Inspect qcow2 image
guestfish -a /var/lib/cocoon/images/vm-123.qcow2 -i
> ls /boot
> ls /EFI/BOOT
> cat /boot/grub/grub.cfg
```

**Remediation**:
- Rebuild OCI image with bootloader
- Use `cocoon-base` images
- Check OVMF firmware installation

### B.2 Serial Output Not Appearing

**Symptom**: No output in serial log file

**Possible Causes**:
1. Guest kernel not configured for serial console
2. Serial log file path incorrect
3. Permissions issue

**Diagnosis**:
```bash
# Check serial log exists
ls -la /var/log/cocoon/vm-123-serial.log

# Check Cloud Hypervisor process
ps aux | grep cloud-hypervisor

# Manually test serial
echo "test" > /dev/ttyS0  # in guest
```

**Remediation**:
- Add `console=ttyS0` to kernel cmdline
- Check file permissions
- Verify Cloud Hypervisor started correctly

### B.3 VM Won't Stop

**Symptom**: `cocoon stop` hangs, VM doesn't shut down

**Possible Causes**:
1. Guest OS not responding to ACPI
2. Init system hung
3. Cloud Hypervisor frozen

**Remediation**:
```bash
# Force kill
cocoon kill vm-123

# Check for zombie processes
ps aux | grep cloud-hypervisor
```

---

**End of Boot Contract Specification v1.0**

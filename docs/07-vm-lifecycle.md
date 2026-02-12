# VM Lifecycle Management

**Version**: 1.0
**Status**: Draft
**Priority**: P0 - CRITICAL FOUNDATION DOCUMENT

## Executive Summary

This document defines the complete VM lifecycle management system for Cocoon, including:
1. State machine with all possible transitions
2. VM identifier rules (`vm_id` vs `name`) and resolution
3. Allowed operations per state
4. Metadata schema and persistence
5. VM configuration schema (`config.json` immutable vs `metadata.json` mutable)
6. Idempotency rules and error handling
7. Reconciliation and cleanup procedures

The lifecycle system ensures consistent, predictable VM behavior across all operations (create, start, stop, delete, inspect).

## Table of Contents

1. [State Machine](#1-state-machine)
2. [State Transitions](#2-state-transitions)
3. [Operations and Permissions](#3-operations-and-permissions)
4. [Metadata Schema](#4-metadata-schema)
5. [VM Configuration Schema (config.json vs metadata.json)](#5-vm-configuration-schema)
6. [Metadata Persistence](#6-metadata-persistence)
7. [Idempotency Rules](#7-idempotency-rules)
8. [Error Handling](#8-error-handling)
9. [Reconciliation](#9-reconciliation)
10. [Implementation Guide](#10-implementation-guide)

---

## 1. State Machine

### 1.1 VM States

```go
type VMState string

const (
    // CREATING: Converting OCI image to qcow2, creating overlay
    VMStateCreating  VMState = "CREATING"

    // CREATED: Overlay exists, Cloud Hypervisor not started
    VMStateCreated   VMState = "CREATED"

    // STARTING: Cloud Hypervisor process starting, VM booting
    VMStateStarting  VMState = "STARTING"

    // RUNNING: VM is fully running, guest OS active
    VMStateRunning   VMState = "RUNNING"

    // STOPPING: Shutdown initiated, waiting for graceful stop
    VMStateStopping  VMState = "STOPPING"

    // STOPPED: VM stopped, Cloud Hypervisor process dead
    VMStateStopped   VMState = "STOPPED"

    // ERROR: Something failed during any operation
    VMStateError     VMState = "ERROR"

    // DELETED: Resources cleaned up, VM removed
    VMStateDeleted   VMState = "DELETED"
)
```

### 1.2 State Machine Diagram

```
          create              start              stop
CREATING -----> CREATED -----> STARTING -----> RUNNING -----> STOPPING -----> STOPPED
   |              |               |               |               |              |
   v              v               v               v               v              v
 ERROR          ERROR           ERROR           ERROR           ERROR        DELETED
   |              |               |               |               |
   +-----> delete +-----> start  +-----> (auto)  +-----> stop    +-----> delete
```

**Key Transitions**:
- `CREATING → CREATED`: OCI image converted, overlay created
- `CREATED → STARTING`: Cloud Hypervisor process launched
- `STARTING → RUNNING`: Guest OS booted, ready to execute tasks
- `RUNNING → STOPPING`: Shutdown command received
- `STOPPING → STOPPED`: Cloud Hypervisor process exited
- `*ERROR → DELETED`: Cleanup after failure
- `STOPPED → DELETED`: Normal cleanup
- `STOPPED → STARTING`: VM can be restarted

**Error Transitions** (any state → ERROR):
- OCI conversion failure
- Boot timeout
- Guest kernel panic
- Cloud Hypervisor crash
- Resource exhaustion
- User-initiated kill (SIGKILL)

### 1.3 State Descriptions

#### CREATING
**Purpose**: Initial VM provisioning phase

**Activities**:
- Convert OCI image to qcow2 base image
- Create copy-on-write overlay disk
- Prepare cloud-init configuration for VM initialization
- Allocate VM ID and create working directory
- Generate VM configuration file

**Duration**: 5-30 seconds (depends on image size)

**Exit Conditions**:
- Success → `CREATED`
- Failure → `ERROR`

#### CREATED
**Purpose**: VM is provisioned but not started

**State**:
- Overlay disk exists at `/var/lib/cocoon/vms/{vm-id}/overlay.qcow2`
- Metadata stored in `/var/lib/cocoon/vms/{vm-id}/metadata.json`
- Metadata server configuration prepared (no separate ISO needed)
- No Cloud Hypervisor process running

**Allowed Operations**:
- `start`: Launch Cloud Hypervisor
- `delete`: Remove all resources
- `inspect`: View configuration

#### STARTING
**Purpose**: Cloud Hypervisor booting, guest OS initializing

**Activities**:
- Cloud Hypervisor process running
- Firmware loading (boot mode dependent):
  - PVH mode: `hypervisor-fw` discovers disk, parses GPT/ESP, loads kernel
  - UEFI mode: `CLOUDHV.fd` provides full UEFI environment, loads GRUB from ESP
- Kernel and initrd loading
- systemd initialization
- cloud-init executing (if enabled)

**Duration**: 5-60 seconds (configurable timeout)

**Exit Conditions**:
- Boot success (cloud-init complete) → `RUNNING`
- Boot timeout → `ERROR`
- Boot failure → `ERROR`

**Monitoring**:
- Parse serial log for boot markers
- Detect kernel panic strings
- Monitor Cloud Hypervisor process health

#### RUNNING
**Purpose**: VM is fully operational and ready for use

**State**:
- Cloud Hypervisor process running (PID stored in metadata)
- Guest OS fully booted
- VM accessible via console/API
- Serial console available for debugging

**Activities**:
- VM runs normally
- Serial output available via `cocoon logs`
- External orchestration can interact via API/console
- Monitor VM health

**Exit Conditions**:
- User calls `stop` → `STOPPING`
- User calls `kill` → `ERROR`
- Cloud Hypervisor crash → `ERROR`

#### STOPPING
**Purpose**: Graceful shutdown in progress

**Activities**:
- ACPI shutdown signal sent to guest
- Guest systemd stopping services
- Filesystems being unmounted
- Cloud Hypervisor waiting for VM to power down

**Duration**: 0-30 seconds (configurable timeout)

**Exit Conditions**:
- Graceful shutdown complete → `STOPPED`
- Timeout → Force kill → `ERROR`

**Monitoring**:
- Monitor Cloud Hypervisor process exit
- Detect shutdown timeout
- Capture final serial output

#### STOPPED
**Purpose**: VM cleanly stopped, resources still exist

**State**:
- Cloud Hypervisor process not running
- Overlay disk exists (preserves VM state)
- Metadata exists
- Serial logs preserved

**Allowed Operations**:
- `start`: Restart the VM (with previous disk state)
- `delete`: Remove all resources
- `inspect`: View configuration and logs

**Use Cases**:
- Pause VM execution temporarily
- Inspect VM state before restarting
- Archive VM state before deletion

#### ERROR
**Purpose**: VM encountered an unrecoverable error

**State**:
- Cloud Hypervisor process may or may not be running
- Resources in inconsistent state
- Error message and stack trace captured in metadata

**Common Errors**:
- Boot timeout
- Guest kernel panic
- Cloud Hypervisor crash
- Resource allocation failure
- Disk I/O error

**Allowed Operations**:
- `delete`: Cleanup resources
- `inspect`: View error details

**Recovery**:
- Cannot transition to any state except `DELETED`
- User must delete and recreate VM

#### DELETED
**Purpose**: VM fully removed, terminal state

**State**:
- All files removed
- Metadata removed from registry
- VM ID is **never reused** (ULID guarantees global uniqueness)

**Properties**:
- Terminal state (no transitions out)
- Idempotent (can delete multiple times)
- Cleanup guaranteed

### 1.4 VM Identifier Rules

#### 1.4.1 vm_id (Internal Primary Key)

- **Format**: `vm-{ulid}` (e.g., `vm-01HXYZ5A3B7C8D9E0F1G2H3J4K`). ULID is time-sortable and globally unique.
- Generated at create time, **never reused** even after deletion.
- Used as directory name: `/var/lib/cocoon/vms/{vm_id}/`
- Used in lock files, log files, socket paths:
  - Lock: `/var/lib/cocoon/vms/{vm_id}/metadata.lock`
  - Serial log: `/var/log/cocoon/{vm_id}-serial.log`
  - API socket: `/run/cocoon/vms/{vm_id}/api.sock`

#### 1.4.2 name (User-Facing Alias)

- Optional on create. If omitted, auto-generated as `cocoon-{random-8-chars}` (e.g., `cocoon-a3f7b2c1`).
- **Globally unique** — `cocoon create` fails with a clear error if the name already exists.
- **Immutable after create** — no rename support in Phase 1.
- Stored in `config.json` and in the global name index.

#### 1.4.3 Name Index

- **File**: `/var/lib/cocoon/name-index.json`
- **Format**:
  ```json
  {
    "myvm": "vm-01HXYZ5A3B7C8D9E0F1G2H3J4K",
    "devbox": "vm-01HABC9D8E7F6G5H4J3K2L1M0N"
  }
  ```
- Protected by `metadata.lock`-level file lock (see [Section 6: Metadata Persistence](#6-metadata-persistence)).
- **Rebuilt from config.json files during reconcile** — the name index is a derived cache, not the source of truth.

#### 1.4.4 CLI Resolution

All CLI commands accept a `<vm-ref>` that resolves as follows (see also `09-cli-design.md`):

1. If `<vm-ref>` starts with `vm-`: treat as exact `vm_id` lookup.
2. Otherwise: look up `<vm-ref>` in the name index.
3. If no match: error `"VM not found: <vm-ref>"`.

No prefix-matching or fuzzy matching is supported.

```go
func ResolveVMRef(ref string) (string, error) {
    if strings.HasPrefix(ref, "vm-") {
        // Direct vm_id lookup
        if _, err := LoadConfig(ref); err != nil {
            return "", fmt.Errorf("VM not found: %s", ref)
        }
        return ref, nil
    }

    // Name index lookup
    index, err := LoadNameIndex()
    if err != nil {
        return "", fmt.Errorf("failed to load name index: %w", err)
    }

    vmID, ok := index[ref]
    if !ok {
        return "", fmt.Errorf("VM not found: %s", ref)
    }
    return vmID, nil
}
```

---

## 2. State Transitions

### 2.1 Valid Transitions

```go
// ValidTransitions defines allowed state transitions
var ValidTransitions = map[VMState][]VMState{
    VMStateCreating: {
        VMStateCreated,  // Success
        VMStateError,    // Failure
    },
    VMStateCreated: {
        VMStateStarting, // start command
        VMStateDeleted,  // delete command
    },
    VMStateStarting: {
        VMStateRunning,  // Boot success
        VMStateError,    // Boot failure
    },
    VMStateRunning: {
        VMStateStopping, // stop command
        VMStateError,    // crash, kill, timeout
    },
    VMStateStopping: {
        VMStateStopped,  // Graceful shutdown
        VMStateError,    // Force kill or timeout
    },
    VMStateStopped: {
        VMStateStarting, // restart command
        VMStateDeleted,  // delete command
    },
    VMStateError: {
        VMStateDeleted,  // cleanup only
    },
    VMStateDeleted: {
        // Terminal state, no transitions
    },
}
```

### 2.2 Transition Validation

```go
// ValidateTransition checks if state transition is allowed
func ValidateTransition(from, to VMState) error {
    allowed, exists := ValidTransitions[from]
    if !exists {
        return fmt.Errorf("unknown state: %s", from)
    }

    for _, valid := range allowed {
        if valid == to {
            return nil
        }
    }

    return fmt.Errorf("invalid transition: %s -> %s", from, to)
}

// TransitionState atomically transitions VM to new state
func TransitionState(vmID string, to VMState) error {
    // 1. Load current metadata
    metadata, err := LoadMetadata(vmID)
    if err != nil {
        return err
    }

    // 2. Validate transition
    if err := ValidateTransition(metadata.State, to); err != nil {
        return err
    }

    // 3. Save old state before updating (CRITICAL: must save before update)
    oldState := metadata.State

    // 4. Update state
    metadata.State = to
    metadata.UpdatedAt = time.Now()

    // 5. Add transition to history
    metadata.StateHistory = append(metadata.StateHistory, StateTransition{
        From:      oldState,  // Use saved old state, not metadata.State
        To:        to,
        Timestamp: time.Now(),
        Reason:    "", // Set by caller
    })

    // 6. Persist atomically
    return SaveMetadata(metadata)
}
```

---

## 3. Operations and Permissions

### 3.1 Operation Permission Matrix

| State     | create | start | stop | delete | inspect |
|-----------|--------|-------|------|--------|---------|
| (none)    | ✅     | ❌    | ❌   | ❌     | ❌      |
| CREATING  | ❌     | ❌    | ❌   | ❌     | ✅      |
| CREATED   | ❌     | ✅    | ❌   | ✅     | ✅      |
| STARTING  | ❌     | ❌    | ❌   | ❌     | ✅      |
| RUNNING   | ❌     | ❌    | ✅   | ✅*    | ✅      |
| STOPPING  | ❌     | ❌    | ❌   | ❌     | ✅      |
| STOPPED   | ❌     | ✅    | ❌   | ✅     | ✅      |
| ERROR     | ❌     | ❌    | ❌   | ✅     | ✅      |
| DELETED   | ❌     | ❌    | ❌   | ❌     | ❌      |

**Notes**:
- `*` = Requires `--force` flag
- `inspect` is always allowed except for non-existent VMs

### 3.2 Operation Definitions

#### create

**Signature**: `cocoon create IMAGE [--name NAME] [--cpus N] [--memory M]`

**Preconditions**:
- VM with same name must not exist
- Sufficient disk space
- Base image available or pullable

**State Changes**:
- `(none) → CREATING → CREATED`

**Postconditions**:
- VM metadata created
- Overlay disk created
- Metadata server configuration prepared (guest datasource config embedded in image)
- VM in CREATED state

**Idempotency**:
- Creating same VM name twice → Error: "VM already exists"
- Solution: Delete existing VM first, or use different name

#### start

**Signature**: `cocoon start VM_ID`

**Preconditions**:
- VM in CREATED or STOPPED state
- Cloud Hypervisor binary available
- Firmware available (PVH: hypervisor-fw; UEFI: CLOUDHV.fd)
- Sufficient system resources

**State Changes**:
- `CREATED → STARTING → RUNNING`
- `STOPPED → STARTING → RUNNING`

**Postconditions**:
- Cloud Hypervisor process running
- PID stored in metadata
- Serial log actively written
- Guest OS booted

**Idempotency**:
- Starting RUNNING VM → No-op, return success
- Starting STARTING VM → Error: "VM already starting"

#### stop

**Signature**: `cocoon stop VM_ID [--timeout SECONDS]`

**Preconditions**:
- VM in RUNNING state
- Cloud Hypervisor process responding

**State Changes**:
- `RUNNING → STOPPING → STOPPED`

**Postconditions**:
- Cloud Hypervisor process not running
- Overlay disk cleanly unmounted
- Serial log closed
- VM in STOPPED state

**Idempotency**:
- Stopping STOPPED VM → No-op, return success
- Stopping STOPPING VM → Wait for completion

#### delete

**Signature**: `cocoon delete VM_ID [--force]`

**Preconditions**:
- VM exists
- If RUNNING: `--force` flag required

**State Changes**:
- `CREATED → DELETED`
- `STOPPED → DELETED`
- `ERROR → DELETED`
- `RUNNING → STOPPED → DELETED` (if --force)

**Postconditions**:
- All files removed
- Metadata removed
- VM ID freed

**Idempotency**:
- Deleting non-existent VM → No-op, return success
- Deleting DELETED VM → No-op, return success

#### inspect

**Signature**: `cocoon inspect VM_ID [--format json|yaml]`

**Preconditions**:
- VM exists (any state except DELETED)

**State Changes**:
- None (read-only operation)

**Output**:
- VM metadata
- Current state
- Configuration
- Resource usage
- Error messages (if ERROR state)

---

## 4. Metadata Schema

### 4.1 Complete Metadata Structure

```go
package metadata

import "time"

// VMMetadata is the complete metadata for a VM
type VMMetadata struct {
    // ===== Identity =====
    VMID      string    `json:"vm_id"`
    Name      string    `json:"name"`
    State     VMState   `json:"state"`

    // ===== Image Information =====
    Image     ImageInfo `json:"image"`

    // ===== Storage =====
    Storage   StorageInfo `json:"storage"`

    // ===== Hypervisor =====
    Hypervisor HypervisorInfo `json:"hypervisor"`

    // ===== Boot Configuration =====
    BootConfig BootConfig `json:"boot_config"`

    // ===== Cloud-Init Configuration =====
    CloudInit CloudInitConfig `json:"cloud_init"`

    // ===== Timestamps =====
    Timestamps Timestamps `json:"timestamps"`

    // ===== Runtime Status =====
    Runtime    RuntimeStatus `json:"runtime"`

    // ===== State History =====
    StateHistory []StateTransition `json:"state_history"`

    // ===== Error Information =====
    Error *ErrorInfo `json:"error,omitempty"`
}

// ImageInfo contains OCI image details
type ImageInfo struct {
    // Original OCI image reference
    Ref string `json:"ref"`

    // Image digest (sha256)
    Digest string `json:"digest"`

    // Base image checksum (for deduplication)
    BaseChecksum string `json:"base_checksum"`

    // Image size in bytes
    Size int64 `json:"size"`

    // Image pulled timestamp
    PulledAt time.Time `json:"pulled_at"`
}

// StorageInfo contains disk information
type StorageInfo struct {
    // Path to overlay disk
    OverlayPath string `json:"overlay_path"`

    // Path to base image (shared, read-only)
    BasePath string `json:"base_path"`

    // Disk size (e.g., "10G")
    Size string `json:"size"`

    // Actual disk usage in bytes
    UsedBytes int64 `json:"used_bytes"`

    // Filesystem type
    Filesystem string `json:"filesystem"` // "ext4", "xfs", etc.
}

// HypervisorInfo contains Cloud Hypervisor details
type HypervisorInfo struct {
    // Cloud Hypervisor API socket path
    CHSocket string `json:"ch_socket"`

    // Cloud Hypervisor process ID (0 if not running)
    CHPID int `json:"ch_pid"`

    // Serial console log path
    SerialLog string `json:"serial_log"`

    // Cloud Hypervisor version
    Version string `json:"version"`

    // API version
    APIVersion string `json:"api_version"`
}

// CloudInitConfig contains cloud-init metadata server configuration
type CloudInitConfig struct {
    // Metadata server address (e.g., "http://169.254.169.254")
    MetadataServerAddr string `json:"metadata_server_addr"`

    // Instance ID for this VM
    InstanceID string `json:"instance_id"`

    // Hostname
    Hostname string `json:"hostname"`

    // Public keys for SSH access
    PublicKeys []string `json:"public_keys,omitempty"`

    // User-data (cloud-config format)
    UserData string `json:"user_data,omitempty"`
}

// BootConfig contains boot configuration
type BootConfig struct {
    // Number of vCPUs
    CPUs int `json:"cpus"`

    // Memory in bytes
    Memory int64 `json:"memory"`

    // Boot mode: "pvh" or "uefi"
    BootMode string `json:"boot_mode"`

    // Firmware path (required for both modes)
    // PVH: /var/lib/cocoon/firmware/hypervisor-fw (passed via --firmware)
    // UEFI: /var/lib/cocoon/firmware/CLOUDHV.fd (passed via --kernel)
    // Note: Cloud Hypervisor does NOT auto-detect firmware; explicit path required
    FirmwarePath string `json:"firmware_path"`

    // Kernel path (if boot_mode == "direct-kernel")
    KernelPath string `json:"kernel_path,omitempty"`

    // Initrd path (if boot_mode == "direct-kernel")
    InitrdPath string `json:"initrd_path,omitempty"`

    // Kernel command line
    KernelCmdline string `json:"kernel_cmdline,omitempty"`
}

// Timestamps tracks VM lifecycle events
type Timestamps struct {
    // VM created timestamp
    CreatedAt time.Time `json:"created_at"`

    // Last updated timestamp
    UpdatedAt time.Time `json:"updated_at"`

    // VM started timestamp (nil if never started)
    StartedAt *time.Time `json:"started_at,omitempty"`

    // VM stopped timestamp (nil if not stopped)
    StoppedAt *time.Time `json:"stopped_at,omitempty"`

    // VM deleted timestamp
    DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

// RuntimeStatus contains runtime execution information
type RuntimeStatus struct {
    // Exit code (nil if not exited)
    ExitCode *int `json:"exit_code,omitempty"`

    // Boot time in milliseconds
    BootTimeMs int64 `json:"boot_time_ms"`

    // Runtime duration in seconds (0 if not stopped)
    RuntimeSeconds float64 `json:"runtime_seconds"`
}

// StateTransition records a state change
type StateTransition struct {
    From      VMState   `json:"from"`
    To        VMState   `json:"to"`
    Timestamp time.Time `json:"timestamp"`
    Reason    string    `json:"reason"`
}

// ErrorInfo contains error details
type ErrorInfo struct {
    // Error message
    Message string `json:"message"`

    // Error type: "boot_timeout", "crash", "panic", etc.
    Type string `json:"type"`

    // Error timestamp
    Timestamp time.Time `json:"timestamp"`

    // Stack trace (if available)
    StackTrace string `json:"stack_trace,omitempty"`

    // Serial log excerpt at error time
    SerialExcerpt string `json:"serial_excerpt,omitempty"`
}
```

### 4.2 Example Metadata (JSON)

```json
{
  "vm_id": "vm-abc123",
  "name": "myvm",
  "state": "RUNNING",

  "image": {
    "ref": "myorg/ubuntu-bootable:22.04",
    "digest": "sha256:abcd1234...",
    "base_checksum": "sha256:ef015678...",
    "size": 419430400,
    "pulled_at": "2026-02-11T20:00:00Z"
  },

  "storage": {
    "overlay_path": "/var/lib/cocoon/vms/vm-abc123/overlay.qcow2",
    "base_path": "/var/lib/cocoon/cache/images/sha256:ef015678.qcow2",
    "size": "10G",
    "used_bytes": 2147483648,
    "filesystem": "ext4"
  },

  "hypervisor": {
    "ch_socket": "/run/cocoon/vms/vm-abc123/api.sock",
    "ch_pid": 12345,
    "serial_log": "/var/log/cocoon/vm-abc123-serial.log",
    "version": "v38.0",
    "api_version": "v1"
  },

  "boot_config": {
    "cpus": 2,
    "memory": 1073741824,
    "boot_mode": "pvh",
    "firmware_path": "/var/lib/cocoon/firmware/hypervisor-fw"
  },

  "timestamps": {
    "created_at": "2026-02-11T20:00:00Z",
    "updated_at": "2026-02-11T20:01:30Z",
    "started_at": "2026-02-11T20:01:00Z",
    "stopped_at": null
  },

  "runtime": {
    "exit_code": null,
    "boot_time_ms": 8500,
    "runtime_seconds": 30.5
  },

  "state_history": [
    {
      "from": "",
      "to": "CREATING",
      "timestamp": "2026-02-11T20:00:00Z",
      "reason": "cocoon create myorg/ubuntu-bootable:22.04"
    },
    {
      "from": "CREATING",
      "to": "CREATED",
      "timestamp": "2026-02-11T20:00:15Z",
      "reason": "Overlay created successfully"
    },
    {
      "from": "CREATED",
      "to": "STARTING",
      "timestamp": "2026-02-11T20:01:00Z",
      "reason": "cocoon start vm-abc123"
    },
    {
      "from": "STARTING",
      "to": "RUNNING",
      "timestamp": "2026-02-11T20:01:08Z",
      "reason": "Guest OS booted successfully"
    }
  ],

  "error": null
}
```

---

## 5. VM Configuration Schema

Cocoon splits per-VM persistent data into two files with distinct mutability semantics:
- **`config.json`** — immutable VM configuration, written once at create time
- **`metadata.json`** — mutable runtime state, updated on every state transition

Both files live in `/var/lib/cocoon/vms/{vm_id}/`.

### 5.1 config.json — Immutable VM Configuration

`config.json` is written once during `cocoon create` and **never modified after creation** (Phase 2 may allow controlled resize of CPU/memory, but the file is treated as append-only migration, not in-place edit).

```go
// config.json — immutable, written once at create, never modified after
type VMConfig struct {
    // Identity
    VMID        string `json:"vm_id"`         // Primary key: vm-{ulid}, never reused
    Name        string `json:"name"`          // User alias, globally unique

    // Image provenance (immutable after create)
    ImageRef      string `json:"image_ref"`       // Original image reference (path/URL/OCI ref)
    BaseChecksum  string `json:"base_checksum"`   // SHA256 of base qcow2 in cache
    BaseImagePath string `json:"base_image_path"` // Path to cached base: /var/lib/cocoon/cache/images/{checksum}_{arch}.qcow2

    // Boot configuration (immutable)
    BootMode     string `json:"boot_mode"`      // "pvh" or "uefi"
    FirmwarePath string `json:"firmware_path"`  // Resolved firmware path at creation

    // Resources (immutable after create; Phase 2 may allow resize)
    CPUs     int    `json:"cpus"`
    MemoryMB int64  `json:"memory_mb"`        // Internal: always bytes-convertible
    DiskSize string `json:"disk_size"`         // Overlay size, e.g. "10G"

    // Storage paths (derived, stored for fast lookup)
    OverlayPath string `json:"overlay_path"`   // /var/lib/cocoon/vms/{vm-id}/overlay.qcow2
    SerialLog   string `json:"serial_log"`     // /var/log/cocoon/{vm-id}-serial.log
    SocketPath  string `json:"socket_path"`    // /run/cocoon/vms/{vm-id}/api.sock

    // Timestamps
    CreatedAt string `json:"created_at"`       // RFC3339

    // Schema version for migration
    SchemaVersion int `json:"schema_version"`  // Currently 1
}
```

**Example config.json**:
```json
{
  "vm_id": "vm-01HXYZ5A3B7C8D9E0F1G2H3J4K",
  "name": "myvm",
  "image_ref": "myorg/ubuntu-bootable:22.04",
  "base_checksum": "sha256:ef015678abcd1234...",
  "base_image_path": "/var/lib/cocoon/cache/images/ef015678abcd1234_amd64.qcow2",
  "boot_mode": "pvh",
  "firmware_path": "/var/lib/cocoon/firmware/hypervisor-fw",
  "cpus": 2,
  "memory_mb": 1024,
  "disk_size": "10G",
  "overlay_path": "/var/lib/cocoon/vms/vm-01HXYZ5A3B7C8D9E0F1G2H3J4K/overlay.qcow2",
  "serial_log": "/var/log/cocoon/vm-01HXYZ5A3B7C8D9E0F1G2H3J4K-serial.log",
  "socket_path": "/run/cocoon/vms/vm-01HXYZ5A3B7C8D9E0F1G2H3J4K/api.sock",
  "created_at": "2026-02-11T20:00:00Z",
  "schema_version": 1
}
```

### 5.2 metadata.json — Mutable Runtime State

`metadata.json` is updated on every state transition, process start/stop, and error event.

```go
// metadata.json — mutable, updated on every state transition
type VMMetadata struct {
    VMID          string `json:"vm_id"`            // Must match config.json
    State         string `json:"state"`            // Current state: CREATING/CREATED/STARTING/RUNNING/STOPPING/STOPPED/ERROR/DELETED
    PreviousState string `json:"previous_state"`   // For transition auditing

    // Runtime (changes with each start/stop cycle)
    ProcessPID int    `json:"process_pid,omitempty"`  // CH process PID (0 if not running)
    BootTime   string `json:"boot_time,omitempty"`    // Duration string, e.g. "2.3s"

    // Error tracking
    LastError  string `json:"last_error,omitempty"`
    ErrorCount int    `json:"error_count"`

    // Timestamps
    UpdatedAt string `json:"updated_at"`          // RFC3339, updated on every state change
    StartedAt string `json:"started_at,omitempty"`
    StoppedAt string `json:"stopped_at,omitempty"`

    // Schema version
    SchemaVersion int `json:"schema_version"`      // Currently 1
}
```

**Example metadata.json** (VM currently running):
```json
{
  "vm_id": "vm-01HXYZ5A3B7C8D9E0F1G2H3J4K",
  "state": "RUNNING",
  "previous_state": "STARTING",
  "process_pid": 12345,
  "boot_time": "2.3s",
  "last_error": "",
  "error_count": 0,
  "updated_at": "2026-02-11T20:01:08Z",
  "started_at": "2026-02-11T20:01:06Z",
  "stopped_at": "",
  "schema_version": 1
}
```

### 5.3 Source of Truth

| Question | Source | File |
|----------|--------|------|
| What should this VM look like? | **config.json** | Immutable configuration: image, resources, paths |
| What is this VM doing right now? | **metadata.json** | Mutable runtime state: current state, PID, errors |

**config.json** is the source of truth for "what this VM should be." Reconcile uses it to reconstruct expected configuration — it knows the VM exists, what image it was created from, how many CPUs it should have, and where its files live.

**metadata.json** is the source of truth for "what this VM is doing now." It tracks runtime state, the Cloud Hypervisor process PID, error history, and timestamps for the current lifecycle.

**On crash recovery**: `config.json` survives intact (it is never modified after creation). `metadata.json` may be stale if the crash occurred during a state transition. Reconciliation reads `config.json` to know the VM exists and its expected configuration, then probes actual system state (is the process alive? is the socket responsive?) to rebuild `metadata.json` accurately.

**On upgrade/migration**: The `SchemaVersion` field in both files enables forward migration. `config.json` rarely changes schema since its fields are stable by design. `metadata.json` schema may evolve more frequently as new runtime tracking is added.

### 5.4 Relationship to Section 4

Section 4 defines the **combined** `VMMetadata` struct used during the initial implementation phase. As the implementation matures, the fields in Section 4's `VMMetadata` will be split into `VMConfig` (Section 5.1) and `VMMetadata` (Section 5.2) as described above. The combined struct in Section 4 remains useful as a reference for `cocoon inspect` output, which merges data from both files.

---

## 6. Metadata Persistence

### 6.1 Storage Path Structure

```
/var/lib/cocoon/
├── name-index.json            # Global name → vm_id mapping (see Section 1.4.3)
└── vms/{vm-id}/
    ├── config.json            # Immutable VM configuration (see Section 5.1)
    ├── metadata.json          # Mutable runtime state (see Section 5.2)
    ├── metadata.lock          # File lock for atomic updates
    └── overlay.qcow2          # VM overlay disk

/run/cocoon/vms/{vm-id}/
└── api.sock                   # Cloud Hypervisor API socket

/var/log/cocoon/
└── {vm-id}-serial.log         # Serial console output
```

### 6.2 Atomic Updates

Metadata updates use atomic write with temporary file + rename:

```go
package metadata

import (
    "encoding/json"
    "os"
    "path/filepath"
    "syscall"
)

// SaveMetadata atomically updates VM metadata
func SaveMetadata(meta *VMMetadata) error {
    // 1. Construct paths
    vmDir := filepath.Join("/var/lib/cocoon/vms", meta.VMID)
    metadataPath := filepath.Join(vmDir, "metadata.json")
    lockPath := filepath.Join(vmDir, "metadata.lock")
    tempPath := metadataPath + ".tmp"

    // 2. Acquire file lock
    lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL, 0644)
    if err != nil {
        return fmt.Errorf("failed to acquire lock: %w", err)
    }
    defer os.Remove(lockPath)
    defer lockFile.Close()

    // 3. Apply exclusive lock
    if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
        return fmt.Errorf("failed to lock: %w", err)
    }

    // 4. Update timestamp
    meta.Timestamps.UpdatedAt = time.Now()

    // 5. Serialize to JSON
    data, err := json.MarshalIndent(meta, "", "  ")
    if err != nil {
        return fmt.Errorf("failed to marshal metadata: %w", err)
    }

    // 6. Write to temporary file
    if err := os.WriteFile(tempPath, data, 0644); err != nil {
        return fmt.Errorf("failed to write temp file: %w", err)
    }

    // 7. Sync to disk
    f, err := os.Open(tempPath)
    if err != nil {
        return err
    }
    f.Sync()
    f.Close()

    // 8. Atomic rename
    if err := os.Rename(tempPath, metadataPath); err != nil {
        os.Remove(tempPath)
        return fmt.Errorf("failed to rename: %w", err)
    }

    return nil
}

// LoadMetadata reads VM metadata from disk
func LoadMetadata(vmID string) (*VMMetadata, error) {
    metadataPath := filepath.Join("/var/lib/cocoon/vms", vmID, "metadata.json")

    data, err := os.ReadFile(metadataPath)
    if err != nil {
        return nil, fmt.Errorf("failed to read metadata: %w", err)
    }

    var meta VMMetadata
    if err := json.Unmarshal(data, &meta); err != nil {
        return nil, fmt.Errorf("failed to parse metadata: %w", err)
    }

    return &meta, nil
}
```

### 6.3 Consistency Guarantees

**Write Atomicity**:
- All metadata updates use temp file + rename
- Guarantees atomic replacement of metadata
- No partial writes visible to readers

**Lock-Free Reads**:
- Readers can read without locks (rename is atomic)
- Always see complete, valid metadata
- May see slightly stale data (eventually consistent)

**Concurrent Writes**:
- File lock prevents concurrent modifications
- First writer wins
- Second writer blocks until lock released

---

## 7. Idempotency Rules

### 7.1 Operation Idempotency

All Cocoon operations are designed to be idempotent where safe:

#### create

```go
func Create(name, image string) error {
    // Check if VM exists
    if exists, _ := VMExists(name); exists {
        return fmt.Errorf("VM '%s' already exists", name)
    }

    // Proceed with creation
    // ...
}
```

**Behavior**:
- Creating same VM name twice → **Error**: "VM already exists"
- **Not idempotent** (by design, to prevent accidental overwrites)

**Workaround**:
```bash
cocoon delete myvm || true
cocoon create ubuntu-22.04-cloudimg --name myvm
```

#### start

```go
func Start(vmID string) error {
    meta, err := LoadMetadata(vmID)
    if err != nil {
        return err
    }

    // Idempotent: already running → no-op
    if meta.State == VMStateRunning {
        log.Info("VM already running")
        return nil
    }

    // Proceed with start
    // ...
}
```

**Behavior**:
- Starting RUNNING VM → **No-op, success**
- Starting STARTING VM → **Error**: "VM is starting"
- **Idempotent for RUNNING state**

#### stop

```go
func Stop(vmID string, timeout time.Duration) error {
    meta, err := LoadMetadata(vmID)
    if err != nil {
        return err
    }

    // Idempotent: already stopped → no-op
    if meta.State == VMStateStopped {
        log.Info("VM already stopped")
        return nil
    }

    // Wait if stopping
    if meta.State == VMStateStopping {
        return waitForStop(vmID, timeout)
    }

    // Proceed with stop
    // ...
}
```

**Behavior**:
- Stopping STOPPED VM → **No-op, success**
- Stopping STOPPING VM → **Wait for completion**
- **Idempotent for STOPPED state**

#### delete

```go
func Delete(vmID string, force bool) error {
    // Idempotent: VM doesn't exist → no-op
    meta, err := LoadMetadata(vmID)
    if os.IsNotExist(err) {
        log.Info("VM already deleted or never existed")
        return nil
    }
    if err != nil {
        return err
    }

    // Stop if running and --force
    if meta.State == VMStateRunning {
        if !force {
            return fmt.Errorf("VM is running, use --force to delete")
        }
        if err := Stop(vmID, 10*time.Second); err != nil {
            // Force kill
            Kill(vmID)
        }
    }

    // Proceed with deletion
    // ...
}
```

**Behavior**:
- Deleting non-existent VM → **No-op, success**
- Deleting DELETED VM → **No-op, success**
- **Fully idempotent**

### 7.2 Idempotency Summary

| Operation | Idempotent? | Behavior on Retry |
|-----------|-------------|-------------------|
| create    | ❌ No       | Error: "VM exists" |
| start     | ✅ Yes      | No-op if RUNNING |
| stop      | ✅ Yes      | No-op if STOPPED |
| delete    | ✅ Yes      | No-op if deleted |
| inspect   | ✅ Yes      | Always read-only |

---

## 8. Error Handling

### 8.1 Error Types

```go
type ErrorType string

const (
    // Creation errors
    ErrorOCIConversion    ErrorType = "oci_conversion_failed"
    ErrorDiskCreation     ErrorType = "disk_creation_failed"
    ErrorInsufficientDisk ErrorType = "insufficient_disk_space"

    // Boot errors
    ErrorBootTimeout      ErrorType = "boot_timeout"
    ErrorKernelPanic      ErrorType = "kernel_panic"
    ErrorMissingBootloader ErrorType = "missing_bootloader"
    ErrorMissingKernel    ErrorType = "missing_kernel"

    // Runtime errors
    ErrorCHCrash          ErrorType = "cloud_hypervisor_crash"
    ErrorTaskTimeout      ErrorType = "task_timeout"
    ErrorGuestCrash       ErrorType = "guest_crash"
    ErrorResourceExhaustion ErrorType = "resource_exhaustion"

    // Shutdown errors
    ErrorStopTimeout      ErrorType = "stop_timeout"
    ErrorForceKillFailed  ErrorType = "force_kill_failed"
)
```

### 8.2 Error State Handling

When a VM enters ERROR state:

```go
func HandleError(vmID string, errType ErrorType, err error) {
    meta, _ := LoadMetadata(vmID)

    // Capture error details
    meta.Error = &ErrorInfo{
        Type:      string(errType),
        Message:   err.Error(),
        Timestamp: time.Now(),
    }

    // Capture serial log excerpt
    if serialLog := readLastLines(meta.Hypervisor.SerialLog, 50); serialLog != "" {
        meta.Error.SerialExcerpt = serialLog
    }

    // Transition to ERROR state
    meta.State = VMStateError
    SaveMetadata(meta)

    // Cleanup running processes
    if meta.Hypervisor.CHPID > 0 {
        syscall.Kill(meta.Hypervisor.CHPID, syscall.SIGKILL)
        meta.Hypervisor.CHPID = 0
    }

    // Log error
    log.Error("VM %s entered ERROR state: %s - %s", vmID, errType, err)
}
```

### 8.3 Error Recovery

**User Actions in ERROR State**:

1. **Inspect Error**:
   ```bash
   cocoon inspect vm-abc123
   # View error message, serial log excerpt
   ```

2. **View Logs**:
   ```bash
   cocoon logs vm-abc123
   # View full serial log
   ```

3. **Cleanup**:
   ```bash
   cocoon delete vm-abc123
   # Remove failed VM
   ```

**No Automatic Recovery**:
- VMs in ERROR state cannot be restarted
- User must delete and recreate
- Prevents silent failures

---

## 9. Reconciliation

### 9.1 Purpose

**Reconciliation** ensures metadata consistency with actual system state:
- Detect orphaned Cloud Hypervisor processes
- Detect VMs stuck in inconsistent states
- Cleanup stale resources
- Fix metadata inconsistencies

### 9.2 Crash Recovery and Reconciliation

#### 9.2.1 Sources of Truth (Priority Order)

When reconciling VM state after crashes (kill -9, power loss, partial state), sources of truth are evaluated in this priority order:

**Priority 0: config.json (VM existence and expected configuration)**
- If `config.json` exists in `/var/lib/cocoon/vms/{vm_id}/`, the VM exists
- Immutable — never corrupted by partial writes during state transitions
- Provides expected paths, resource configuration, and identity (see [Section 5.1](#51-configjson--immutable-vm-configuration))
- Used to rebuild the name index if it becomes stale

**Priority 1: Cloud Hypervisor Process Status**
- Check if PID from metadata.json is still running
- Validate process is actually `cloud-hypervisor` (not PID reuse)
- **Most authoritative for runtime state**: If process is dead, VM cannot be RUNNING

**Priority 2: API Socket Connectivity**
- Check if CH API socket exists at path from `config.json` (`socket_path`)
- Attempt connection to socket
- If process is running but socket missing → inconsistent state

**Priority 3: metadata.json State Field**
- Last known state recorded before crash
- May be stale if crash occurred during state transition
- Used to determine expected state vs actual state

**Priority 4: Overlay Disk Existence**
- Check if overlay exists at path from `config.json` (`overlay_path`)
- If missing, VM cannot be recovered (data loss)
- If present, VM can potentially be restarted

**Priority 5: PID File Validity**
- Check if PID file at `/run/cocoon/vms/{vm-id}/ch.pid` exists
- Cross-reference with metadata.json PID
- Stale PID files indicate crash or unclean shutdown

#### 9.2.2 Crash Scenarios and Recovery

| Scenario | metadata.json | PID Running? | Socket Exists? | Action | New State |
|----------|---------------|--------------|----------------|--------|-----------|
| **Clean state** | RUNNING | Yes | Yes | None | RUNNING |
| **Crashed VM** | RUNNING | No | No | Mark crashed | ERROR |
| **Socket lost** | RUNNING | Yes | No | Inconsistent | ERROR |
| **Zombie process** | STOPPED | Yes | Yes | Kill process | STOPPED |
| **PID reused** | RUNNING | Yes (wrong proc) | No | Detect reuse | ERROR |
| **Power loss** | RUNNING | No | No | Mark crashed | ERROR |
| **Partial start** | STARTING | No | No | Timeout/stuck | ERROR |
| **Stuck stopping** | STOPPING | Yes | Yes | Force kill | STOPPED |
| **Orphaned socket** | STOPPED | No | Yes | Clean socket | STOPPED |

#### 9.2.3 Reconciliation Algorithm

**On Startup (cocoon daemon start or cocoon reconcile)**:

```go
func ReconcileAll() error {
    // 1. SCAN: Discover all VMs
    vmDirs, err := os.ReadDir("/var/lib/cocoon/vms/")
    if err != nil {
        return fmt.Errorf("failed to scan VM directory: %w", err)
    }

    var inconsistencies []Inconsistency

    // 2. ANALYZE: Check each VM
    for _, vmDir := range vmDirs {
        vmID := vmDir.Name()

        // 2a. Load metadata
        meta, err := LoadMetadata(vmID)
        if err != nil {
            inconsistencies = append(inconsistencies, Inconsistency{
                VMID:     vmID,
                Type:     "metadata_corrupted",
                Severity: "critical",
                Details:  err.Error(),
            })
            continue
        }

        // 2b. Check PID and process
        actualState := DetermineActualState(meta)

        // 2c. Compare metadata state vs actual state
        if meta.State != actualState {
            inconsistencies = append(inconsistencies, Inconsistency{
                VMID:           vmID,
                Type:           "state_mismatch",
                Severity:       getSeverity(meta.State, actualState),
                ExpectedState:  meta.State,
                ActualState:    actualState,
                Details:        fmt.Sprintf("metadata=%s, actual=%s", meta.State, actualState),
            })
        }

        // 2d. Check for zombie resources
        zombies := DetectZombieResources(vmID, meta)
        inconsistencies = append(inconsistencies, zombies...)
    }

    // 3. REPORT: Log all inconsistencies
    for _, inc := range inconsistencies {
        log.Warn("VM %s: %s [%s] - %s", inc.VMID, inc.Type, inc.Severity, inc.Details)
    }

    // 4. FIX: Apply fixes if requested
    if reconcileFixFlag {
        for _, inc := range inconsistencies {
            if err := ApplyFix(inc); err != nil {
                log.Error("Failed to fix %s for VM %s: %v", inc.Type, inc.VMID, err)
            } else {
                log.Info("Fixed %s for VM %s", inc.Type, inc.VMID)
            }
        }
    }

    return nil
}

func DetermineActualState(meta *VMMetadata) VMState {
    pid := meta.Hypervisor.CHPID
    socket := meta.Hypervisor.CHSocket

    // Check process
    processRunning := isProcessRunning(pid)
    processValid := false
    if processRunning {
        processValid = validateProcess(pid, "cloud-hypervisor")
    }

    // Check socket
    socketExists := fileExists(socket)
    socketConnectable := false
    if socketExists {
        socketConnectable = canConnectToSocket(socket)
    }

    // Determine actual state based on evidence
    switch meta.State {
    case VMStateRunning:
        if processValid && socketConnectable {
            return VMStateRunning // Genuinely running
        } else if processValid && !socketConnectable {
            return VMStateUnknown // Process alive but socket dead
        } else {
            return VMStateError // Process dead, was RUNNING → crashed
        }

    case VMStateStarting:
        if processValid {
            // Check how long in STARTING state
            elapsed := time.Since(meta.Timestamps.UpdatedAt)
            if elapsed > 5*time.Minute {
                return VMStateError // Stuck in STARTING
            }
            return VMStateStarting // Still starting (give it time)
        } else {
            return VMStateError // Process died during start
        }

    case VMStateStopping:
        if processValid {
            elapsed := time.Since(meta.Timestamps.UpdatedAt)
            if elapsed > 2*time.Minute {
                return VMStateError // Stuck in STOPPING
            }
            return VMStateStopping // Still stopping
        } else {
            return VMStateStopped // Process exited
        }

    case VMStateStopped:
        if processValid {
            return VMStateInconsistent // Process shouldn't be running!
        } else if socketExists {
            return VMStateInconsistent // Zombie socket
        } else {
            return VMStateStopped // Correctly stopped
        }

    case VMStateCreated:
        if processValid {
            return VMStateInconsistent // Process shouldn't exist yet
        }
        return VMStateCreated

    case VMStateError:
        if processValid {
            return VMStateInconsistent // Should cleanup process in ERROR
        }
        return VMStateError

    default:
        return meta.State
    }
}

func DetectZombieResources(vmID string, meta *VMMetadata) []Inconsistency {
    var zombies []Inconsistency

    // Check for zombie PID file
    pidFilePath := fmt.Sprintf("/run/cocoon/vms/%s/ch.pid", vmID)
    if fileExists(pidFilePath) {
        pidFileContent, _ := os.ReadFile(pidFilePath)
        pidFromFile, _ := strconv.Atoi(string(pidFileContent))

        if pidFromFile != meta.Hypervisor.CHPID {
            zombies = append(zombies, Inconsistency{
                VMID:     vmID,
                Type:     "stale_pid_file",
                Severity: "warning",
                Details:  fmt.Sprintf("PID file has %d, metadata has %d", pidFromFile, meta.Hypervisor.CHPID),
            })
        }
    }

    // Check for zombie socket
    if fileExists(meta.Hypervisor.CHSocket) {
        if !isProcessRunning(meta.Hypervisor.CHPID) {
            zombies = append(zombies, Inconsistency{
                VMID:     vmID,
                Type:     "zombie_socket",
                Severity: "warning",
                Details:  fmt.Sprintf("Socket exists at %s but process %d not running", meta.Hypervisor.CHSocket, meta.Hypervisor.CHPID),
            })
        }
    }

    return zombies
}

func ApplyFix(inc Inconsistency) error {
    meta, err := LoadMetadata(inc.VMID)
    if err != nil {
        return err
    }

    switch inc.Type {
    case "state_mismatch":
        // Update metadata to match actual state
        oldState := meta.State
        newState := inc.ActualState

        if newState == VMStateError || newState == VMStateInconsistent {
            // Crashed or inconsistent → ERROR state
            meta.State = VMStateError
            meta.Error = &ErrorInfo{
                Type:      "reconciliation_detected_crash",
                Message:   fmt.Sprintf("VM was %s but process not running (likely crashed)", oldState),
                Timestamp: time.Now(),
            }

            // Cleanup zombie process if any
            if meta.Hypervisor.CHPID > 0 && isProcessRunning(meta.Hypervisor.CHPID) {
                syscall.Kill(meta.Hypervisor.CHPID, syscall.SIGKILL)
            }
            meta.Hypervisor.CHPID = 0

        } else if newState == VMStateStopped {
            // Was STOPPING or RUNNING but process exited
            meta.State = VMStateStopped
            meta.Hypervisor.CHPID = 0
            now := time.Now()
            meta.Timestamps.StoppedAt = &now
        }

        meta.StateHistory = append(meta.StateHistory, StateTransition{
            From:      oldState,
            To:        meta.State,
            Timestamp: time.Now(),
            Reason:    fmt.Sprintf("Reconciliation: %s", inc.Details),
        })

        return SaveMetadata(meta)

    case "zombie_socket":
        // Remove stale socket
        return os.Remove(meta.Hypervisor.CHSocket)

    case "stale_pid_file":
        // Remove stale PID file
        pidFilePath := fmt.Sprintf("/run/cocoon/vms/%s/ch.pid", inc.VMID)
        return os.Remove(pidFilePath)

    case "zombie_process":
        // Kill orphaned process
        if meta.Hypervisor.CHPID > 0 {
            syscall.Kill(meta.Hypervisor.CHPID, syscall.SIGKILL)
            meta.Hypervisor.CHPID = 0
            return SaveMetadata(meta)
        }

    default:
        return fmt.Errorf("unknown inconsistency type: %s", inc.Type)
    }

    return nil
}

// Helper: Check if process is running and matches expected name
func validateProcess(pid int, expectedName string) bool {
    if pid <= 0 {
        return false
    }

    // Check if process exists
    process, err := os.FindProcess(pid)
    if err != nil {
        return false
    }

    // Send signal 0 to check if process is alive (doesn't actually kill it)
    err = process.Signal(syscall.Signal(0))
    if err != nil {
        return false // Process doesn't exist
    }

    // Read /proc/{pid}/comm to verify process name
    commPath := fmt.Sprintf("/proc/%d/comm", pid)
    commBytes, err := os.ReadFile(commPath)
    if err != nil {
        return false
    }

    actualName := strings.TrimSpace(string(commBytes))
    return strings.Contains(actualName, expectedName)
}

func isProcessRunning(pid int) bool {
    if pid <= 0 {
        return false
    }
    process, err := os.FindProcess(pid)
    if err != nil {
        return false
    }
    err = process.Signal(syscall.Signal(0))
    return err == nil
}

func canConnectToSocket(socketPath string) bool {
    conn, err := net.DialTimeout("unix", socketPath, 1*time.Second)
    if err != nil {
        return false
    }
    conn.Close()
    return true
}
```

#### 9.2.4 Crash Scenarios in Detail

**Scenario 1: kill -9 on Cloud Hypervisor**
```
Before: metadata.json → RUNNING, PID=1234
Crash:  kill -9 1234
After:  metadata.json → RUNNING, PID=1234 (dead)

Reconciliation:
1. Read metadata.json → state=RUNNING, PID=1234
2. Check process 1234 → not running
3. Check socket → missing
4. Action: Update metadata.json → state=ERROR, error="process killed"
5. Cleanup: Remove zombie socket/PID file if any
```

**Scenario 2: Power Loss During Boot**
```
Before: metadata.json → STARTING, PID=1234
Crash:  Power loss
After:  metadata.json → STARTING, PID=1234 (dead)

Reconciliation:
1. Read metadata.json → state=STARTING, PID=1234
2. Check process 1234 → not running
3. Check elapsed time → 5+ minutes (stuck)
4. Action: Update metadata.json → state=ERROR, error="boot timeout/power loss"
5. Cleanup: None needed (already gone)
```

**Scenario 3: Partial State (Stopped but Process Running)**
```
Before: metadata.json → STOPPED
Crash:  Cloud Hypervisor never cleanly exited
After:  metadata.json → STOPPED, PID=1234 (still running!)

Reconciliation:
1. Read metadata.json → state=STOPPED, PID=1234
2. Check process 1234 → RUNNING
3. Inconsistency detected: zombie process
4. Action: Kill process 1234, confirm metadata.json → STOPPED
5. Cleanup: Remove socket
```

**Scenario 4: PID Reuse (Process Exists but Wrong)**
```
Before: metadata.json → RUNNING, PID=1234
Crash:  Cloud Hypervisor dies, kernel reuses PID 1234 for bash
After:  metadata.json → RUNNING, PID=1234 (but it's bash!)

Reconciliation:
1. Read metadata.json → state=RUNNING, PID=1234
2. Check process 1234 → running
3. Validate process name → "bash", not "cloud-hypervisor"
4. Action: Update metadata.json → state=ERROR, error="process PID reused"
5. Cleanup: Do NOT kill bash (wrong process)
```

### 9.3 Reconciliation Command

```bash
# Dry-run: Report inconsistencies only
cocoon doctor --reconcile

# Fix inconsistencies automatically
cocoon doctor --reconcile --fix

# Force cleanup of stuck VMs and zombie processes
cocoon doctor --reconcile --fix --force
```

**Aliases**:
- `cocoon reconcile` → `cocoon doctor --reconcile`
- `cocoon doctor` → runs reconciliation by default

**Flags**:
- `--fix`: Automatically fix inconsistencies (default: dry-run)
- `--force`: Force cleanup of stuck VMs and kill zombie processes

**Output**:
```bash
$ cocoon doctor --reconcile
Scanning VMs in /var/lib/cocoon/vms/...

[CRITICAL] vm-abc123: state_mismatch
  Expected: RUNNING
  Actual:   ERROR (process not running)
  Details:  Process PID 1234 not found (likely crashed)

[WARNING] vm-def456: zombie_socket
  Details:  Socket /run/cocoon/vms/vm-def456/api.sock exists but process 5678 not running

[INFO] vm-ghi789: clean
  State: RUNNING (PID 9012, socket responsive)

Summary:
  Total VMs: 3
  Clean: 1
  Issues: 2 (1 critical, 1 warning)

Run 'cocoon doctor --reconcile --fix' to repair inconsistencies.
```

### 9.5 Legacy Reconciliation Logic (Simple Version)

**Note**: This is a simplified version. See Section 8.2 for the full crash recovery algorithm.

```go
func Reconcile(fix, force bool) error {
    // 1. Scan VM directory
    vms, err := listAllVMs()
    if err != nil {
        return err
    }

    for _, vmID := range vms {
        // 2. Load metadata
        meta, err := LoadMetadata(vmID)
        if err != nil {
            log.Warn("Failed to load metadata for %s: %v", vmID, err)
            continue
        }

        // 3. Check process state
        processRunning := isProcessRunning(meta.Hypervisor.CHPID)

        // 4. Detect inconsistencies
        inconsistency := detectInconsistency(meta, processRunning)

        if inconsistency != "" {
            log.Warn("VM %s: %s", vmID, inconsistency)

            if fix {
                fixInconsistency(meta, inconsistency, force)
            }
        }
    }

    // 5. Detect orphaned processes
    orphans := detectOrphanedCHProcesses(vms)
    for _, pid := range orphans {
        log.Warn("Orphaned Cloud Hypervisor process: %d", pid)
        if fix && force {
            syscall.Kill(pid, syscall.SIGKILL)
            log.Info("Killed orphaned process %d", pid)
        }
    }

    return nil
}

func detectInconsistency(meta *VMMetadata, processRunning bool) string {
    switch meta.State {
    case VMStateRunning:
        if !processRunning {
            return "State is RUNNING but Cloud Hypervisor process not found"
        }

    case VMStateStopped:
        if processRunning {
            return "State is STOPPED but Cloud Hypervisor process still running"
        }

    case VMStateStarting:
        // Check if stuck in STARTING for too long
        elapsed := time.Since(meta.Timestamps.UpdatedAt)
        if elapsed > 5*time.Minute {
            return fmt.Sprintf("Stuck in STARTING for %v", elapsed)
        }

    case VMStateStopping:
        // Check if stuck in STOPPING for too long
        elapsed := time.Since(meta.Timestamps.UpdatedAt)
        if elapsed > 2*time.Minute {
            return fmt.Sprintf("Stuck in STOPPING for %v", elapsed)
        }
    }

    return ""
}

func fixInconsistency(meta *VMMetadata, inconsistency string, force bool) {
    switch meta.State {
    case VMStateRunning:
        // Process not running → mark as ERROR
        meta.State = VMStateError
        meta.Error = &ErrorInfo{
            Type:      "cloud_hypervisor_disappeared",
            Message:   inconsistency,
            Timestamp: time.Now(),
        }
        SaveMetadata(meta)
        log.Info("Fixed: Marked VM %s as ERROR", meta.VMID)

    case VMStateStopped:
        // Process still running → kill it
        if force {
            syscall.Kill(meta.Hypervisor.CHPID, syscall.SIGKILL)
            meta.Hypervisor.CHPID = 0
            SaveMetadata(meta)
            log.Info("Fixed: Killed orphaned process for VM %s", meta.VMID)
        }

    case VMStateStarting, VMStateStopping:
        // Stuck in transient state → force to ERROR
        if force {
            if meta.Hypervisor.CHPID > 0 {
                syscall.Kill(meta.Hypervisor.CHPID, syscall.SIGKILL)
            }
            meta.State = VMStateError
            meta.Error = &ErrorInfo{
                Type:      "stuck_in_transient_state",
                Message:   inconsistency,
                Timestamp: time.Now(),
            }
            SaveMetadata(meta)
            log.Info("Fixed: Moved VM %s to ERROR state", meta.VMID)
        }
    }
}

func detectOrphanedCHProcesses(knownVMs []string) []int {
    // Get all Cloud Hypervisor processes
    allCHProcesses := findAllCHProcesses()

    // Load PIDs from known VMs
    knownPIDs := make(map[int]bool)
    for _, vmID := range knownVMs {
        meta, err := LoadMetadata(vmID)
        if err == nil && meta.Hypervisor.CHPID > 0 {
            knownPIDs[meta.Hypervisor.CHPID] = true
        }
    }

    // Find orphans
    var orphans []int
    for _, pid := range allCHProcesses {
        if !knownPIDs[pid] {
            orphans = append(orphans, pid)
        }
    }

    return orphans
}
```

### 9.6 Reconciliation Schedule

**When to Run**:
1. **On daemon startup** (if running as daemon)
2. **Periodically** (every 5 minutes in daemon mode)
3. **Manually** (user runs `cocoon reconcile`)
4. **After crashes** (detect on next CLI invocation)

**Dry-run by Default**:
- Without `--fix`, only reports inconsistencies
- User reviews and decides whether to fix
- Prevents accidental destructive actions

---

## 10. Implementation Guide

### 10.1 Phase 1: Core State Machine (P0)

**Tasks**:
- [ ] Implement `VMState` enum and validation
- [ ] Implement `TransitionState()` with validation
- [ ] Implement `VMMetadata` struct
- [ ] Implement atomic metadata persistence
- [ ] Add state checks to all operations

**Files**:
- `internal/vm/state.go`: State machine
- `internal/vm/metadata.go`: Metadata CRUD
- `internal/vm/operations.go`: Operations with state checks

### 10.2 Phase 2: Operations (P0)

**Tasks**:
- [ ] Implement `create` with CREATING → CREATED transition
- [ ] Implement `start` with STARTING → RUNNING transition
- [ ] Implement `stop` with STOPPING → STOPPED transition
- [ ] Implement `delete` with → DELETED transition
- [ ] Implement `inspect` (read-only)

**Error Handling**:
- [ ] Implement error state transitions
- [ ] Capture error details in metadata
- [ ] Save serial log excerpts on error

### 10.3 Phase 3: Idempotency (P1)

**Tasks**:
- [ ] Add idempotency checks to `start` (RUNNING → no-op)
- [ ] Add idempotency checks to `stop` (STOPPED → no-op)
- [ ] Add idempotency checks to `delete` (non-existent → no-op)
- [ ] Add tests for all idempotency scenarios

### 10.4 Phase 4: Reconciliation (P1)

**Tasks**:
- [ ] Implement `listAllVMs()` scanner
- [ ] Implement `detectInconsistency()` logic
- [ ] Implement `fixInconsistency()` with --force flag
- [ ] Implement orphaned process detection
- [ ] Add dry-run mode (default)
- [ ] Add reconciliation on daemon startup

### 10.5 Testing Checklist

**State Machine Tests**:
- [ ] Valid transitions succeed
- [ ] Invalid transitions error
- [ ] Error transitions from any state
- [ ] DELETED is terminal

**Idempotency Tests**:
- [ ] start RUNNING VM → no-op
- [ ] stop STOPPED VM → no-op
- [ ] delete deleted VM → no-op
- [ ] create existing VM → error

**Concurrency Tests**:
- [ ] Concurrent metadata updates (lock contention)
- [ ] Concurrent state transitions
- [ ] Atomic rename guarantees

**Reconciliation Tests**:
- [ ] Detect RUNNING VM with dead process
- [ ] Detect STOPPED VM with running process
- [ ] Detect stuck STARTING VM
- [ ] Detect orphaned Cloud Hypervisor processes
- [ ] Fix inconsistencies with --fix

**Error Handling Tests**:
- [ ] Boot timeout → ERROR state
- [ ] Kernel panic → ERROR state
- [ ] Cloud Hypervisor crash → ERROR state
- [ ] Cannot restart from ERROR state
- [ ] Can delete from ERROR state

---

## 11. References

### 11.1 Related Documents

- `00-overview.md`: Project overview
- `01-boot-contract.md`: Boot contract and UEFI boot (P0 CRITICAL)
- `05-storage-management.md`: Storage and COW
- `06-concurrency.md`: Concurrency and locking

### 11.2 External References

- **Cloud Hypervisor API**: https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/vmm/src/api/openapi/cloud-hypervisor.yaml
- **Linux Process States**: https://man7.org/linux/man-pages/man1/ps.1.html
- **File Locking (flock)**: https://man7.org/linux/man-pages/man2/flock.2.html

---

## Appendix A: Quick Reference

### A.1 State Transition Commands

```bash
# CREATING → CREATED
cocoon create ubuntu-22.04-cloudimg --name myvm

# CREATED → STARTING → RUNNING
cocoon start vm-abc123

# RUNNING → STOPPING → STOPPED
cocoon stop vm-abc123

# STOPPED → STARTING → RUNNING
cocoon start vm-abc123

# STOPPED → DELETED
cocoon delete vm-abc123

# RUNNING → DELETED (force)
cocoon delete vm-abc123 --force

# ERROR → DELETED
cocoon delete vm-abc123
```

### A.2 State Inspection

```bash
# View current state
cocoon inspect vm-abc123

# View state history
cocoon inspect vm-abc123 --history

# View error details
cocoon inspect vm-abc123 --error

# List all VMs with states
cocoon ps -a
```

### A.3 Reconciliation

```bash
# Dry-run (report only)
cocoon reconcile

# Fix inconsistencies
cocoon reconcile --fix

# Force cleanup stuck VMs
cocoon reconcile --fix --force
```

---

**End of VM Lifecycle Management v1.0**

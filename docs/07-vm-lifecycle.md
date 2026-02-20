# VM Lifecycle Management

**Version**: 1.0
**Status**: Implemented
**Phase**: Phase 1
**Last Updated**: 2026-02-14

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

    // STARTING: Cloud Hypervisor process starting, VM booting.
    // boot_strategy determines boot method:
    //   "uefi":   UEFI boot with CLOUDHV.fd (Phase 1, default)
    //   "direct": Direct kernel boot with kernel + initramfs (implemented for OCI VM images)
    // Actual mode used is recorded in metadata.last_boot_mode.
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

> **Phase 2 extension**: A PAUSED state will be added — see [13-pause-resume.md](./13-pause-resume.md) for design.

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

**Recovery Transition**:
- `ERROR → STOPPED`: Via `cocoon kill` (force-terminates zombie process and resets state)
- `ERROR → DELETED`: Via `cocoon delete` (cleanup after failure)

### 1.3 State Descriptions

#### CREATING
**Purpose**: Initial VM provisioning phase

**Activities**:
- Convert OCI image to qcow2 base image
- Create copy-on-write overlay disk
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
- No Cloud Hypervisor process running

**Allowed Operations**:
- `start`: Launch Cloud Hypervisor
- `delete`: Remove all resources
- `inspect`: View configuration

#### STARTING
**Purpose**: Cloud Hypervisor booting, guest OS initializing

**Note**: The transition to STARTING occurs when the start flow is initiated, before the Cloud Hypervisor process is fully launched. If the launch itself fails, the VM transitions directly from STARTING to ERROR.

**Activities**:
- Cloud Hypervisor process running
- Firmware/kernel loading (boot mode dependent):
  - UEFI mode: `CLOUDHV.fd` provides full UEFI environment, loads GRUB from ESP
  - Direct mode: Kernel and initramfs passed directly to Cloud Hypervisor (no firmware)
- Kernel and initrd loading
- systemd initialization

**Duration**: 5-60 seconds (configurable timeout)

**Exit Conditions**:
- Boot success (systemd targets reached / login prompt) → `RUNNING`
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
- User calls `kill` → `STOPPING` → `STOPPED` (two-step: force kill triggers graceful path)
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
- Timeout → force kill attempted: if force kill succeeds → `STOPPED`; if force kill fails → `ERROR` (the `hypervisor.Shutdown()` method handles force kill internally on timeout; `Stop` transitions to STOPPED on successful force kill, or ERROR only if force kill itself fails)

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
- Error message captured in metadata (`last_error` field)

**Common Errors**:
- Boot timeout
- Guest kernel panic
- Cloud Hypervisor crash
- Resource allocation failure
- Disk I/O error

**Allowed Operations**:
- `kill`: Force-terminate zombie process and transition to `STOPPED` (allows restart)
- `delete`: Cleanup resources
- `inspect`: View error details

**Recovery**:
- `ERROR → STOPPED`: Via `cocoon kill` (force-terminates zombie process and resets state, allowing restart)
- `ERROR → DELETED`: Via `cocoon delete` (cleanup after failure)
- The `kill` path enables recovery without full delete+recreate

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

- Optional on create. If omitted, auto-generated as `cocoon-{random-16-hex-chars}` (e.g., `cocoon-a3f7b2c1d9e0f1a2`).
- **Globally unique** — `cocoon create` fails with a clear error if the name already exists.
- **Immutable after create** — no rename support in Phase 1.
- Stored in `config.json` and in the global name index.

#### 1.4.3 Name Index

- **File**: `/var/lib/cocoon/db/name-index.json`
- **Format**:
  ```json
  {
    "myvm": "vm-01HXYZ5A3B7C8D9E0F1G2H3J4K",
    "devbox": "vm-01HABC9D8E7F6G5H4J3K2L1M0N"
  }
  ```
- Protected by `/var/lib/cocoon/db/name-index.lock` (flock, Level 2 — see [06-concurrency.md § Lock Hierarchy](./06-concurrency.md#lock-hierarchy) and [05-storage-management.md § Canonical Layout](./05-storage-management.md#canonical-filesystem-layout-normative)).
- **Rebuilt from config.json files during reconcile** — the name index is a derived cache, not the source of truth.

#### 1.4.4 CLI Resolution

All CLI commands accept a `<vm-ref>` that resolves as follows (see also `09-cli-design.md`):

1. If `<vm-ref>` starts with `vm-`: treat as exact `vm_id` lookup.
2. Otherwise: look up `<vm-ref>` in the name index.
3. If no match: error `"VM not found: <vm-ref>"`.

No prefix-matching or fuzzy matching is supported.

```go
// ResolveVMRef resolves a user-provided reference to a vm_id.
// If ref starts with "vm-", it is treated as a direct vm_id and validated
// by checking for the existence of config.json (with path-traversal guard).
// Otherwise, the name index is consulted.
func (m *manager) ResolveVMRef(ref string) (string, error) {
    if strings.HasPrefix(ref, "vm-") {
        // Validate vm_id format (reject path traversal)
        if err := validateVMID(ref); err != nil {
            return "", fmt.Errorf("%w: %s", types.ErrVMNotFound, ref)
        }
        // Direct vm_id lookup: verify config.json exists
        configPath := m.cfg.VMConfigPath(ref)
        if _, err := os.Stat(configPath); os.IsNotExist(err) {
            return "", fmt.Errorf("%w: %s", types.ErrVMNotFound, ref)
        }
        return ref, nil
    }

    // Name index lookup
    index, err := LoadNameIndex(m.cfg)
    if err != nil {
        return "", fmt.Errorf("load name index: %w", err)
    }

    vmID, ok := index[ref]
    if !ok {
        return "", fmt.Errorf("%w: %s", types.ErrVMNotFound, ref)
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
        VMStateStopped,  // Graceful shutdown or successful force kill after timeout
        VMStateError,    // Force kill failed after timeout
    },
    VMStateStopped: {
        VMStateStarting, // restart command
        VMStateDeleted,  // delete command
    },
    VMStateError: {
        VMStateStopped,  // via cocoon kill (force-terminates zombie, resets state)
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
    if slices.Contains(allowed, to) {
        return nil
    }
    return fmt.Errorf("invalid transition: %s -> %s", from, to)
}

// TransitionState validates a state transition and persists it atomically.
// The previous state is recorded in metadata for auditing.
// When transitioning to ERROR, LastError, LastErrorAt, and ErrorCount are
// automatically updated from the reason string.
//
// Internally delegates to transitionStateWithUpdate which acquires the
// VM metadata lock (Level 4), loads metadata, validates, applies changes,
// and writes via utils.AtomicWriteJSON.
func (m *manager) TransitionState(vmID string, to types.VMState, reason string) error {
    return m.transitionStateWithUpdate(vmID, to, reason, nil)
}

// transitionStateWithUpdate validates and persists a state transition,
// optionally applying an additional mutation function to metadata.
// Acquires the VM metadata lock (Level 4) internally — callers MUST NOT
// already hold it (flock is not reentrant).
func (m *manager) transitionStateWithUpdate(vmID string, to types.VMState, reason string, mutate func(*types.VMMetadataFile)) error {
    // 1. Acquire flock on metadata.lock
    // 2. Load current metadata via utils.ReadJSON

    // 3. Validate transition
    if err := types.ValidateTransition(from, to); err != nil {
        return err
    }

    // 4. Update state fields
    meta.PreviousState = meta.State
    meta.State = string(to)
    meta.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

    // 5. Auto-track errors: when entering ERROR, record reason and increment count
    if to == types.VMStateError {
        meta.LastError = reason
        meta.LastErrorAt = time.Now().UTC().Format(time.RFC3339)
        meta.ErrorCount++
    }

    // 6. Apply optional mutation (e.g., set PID, BootTime, StoppedAt)
    if mutate != nil {
        mutate(&meta)
    }

    // 7. Persist atomically via utils.AtomicWriteJSON
    return utils.AtomicWriteJSON(metaPath, &meta)
}
```

---

## 3. Operations and Permissions

### 3.1 Operation Permission Matrix

| State     | create | start | stop | kill | delete | inspect | list |
|-----------|--------|-------|------|------|--------|---------|------|
| (none)    | ✅     | ❌    | ❌   | ❌   | ❌     | ❌      | ✅   |
| CREATING  | ❌     | ❌    | ❌   | ❌   | ❌     | ✅      | ✅   |
| CREATED   | ❌     | ✅    | ❌   | ❌   | ✅     | ✅      | ✅   |
| STARTING  | ❌     | ❌    | ❌   | ✅   | ❌     | ✅      | ✅   |
| RUNNING   | ❌     | ❌    | ✅   | ✅   | ✅*    | ✅      | ✅   |
| STOPPING  | ❌     | ❌    | ❌   | ✅   | ❌     | ✅      | ✅   |
| STOPPED   | ❌     | ✅    | ❌   | ❌** | ✅     | ✅      | ✅   |
| ERROR     | ❌     | ❌    | ❌   | ✅   | ✅     | ✅      | ✅   |
| DELETED   | ❌     | ❌    | ❌   | ❌   | ❌     | ❌      | ❌   |

**Notes**:
- `*` = Requires `--force` flag
- `**` = No-op (idempotent: VM already stopped)
- `inspect` is always allowed except for non-existent VMs
- `list` is a global operation that returns all non-deleted VMs

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
- VM in CREATED state

**Idempotency**:
- Creating same VM name twice → Error: "VM already exists"
- Solution: Delete existing VM first, or use different name

#### start

**Signature**: `cocoon start VM_ID`

**Preconditions**:
- VM in CREATED or STOPPED state
- Cloud Hypervisor binary available
- Firmware available (UEFI: CLOUDHV.fd) or kernel/initramfs available (Direct)
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

**Signature**: `cocoon stop VM_ID [--timeout DURATION]`  *(DURATION is a Go duration string, e.g. `30s`, `2m`; default: `30s`)*

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

#### kill

**Signature**: `cocoon kill VM_ID`

**Preconditions**:
- VM in RUNNING, STOPPING, STARTING, or ERROR state

**State Changes**:
- `RUNNING → STOPPING → STOPPED` (two-step: force kill triggers graceful path)
- `STOPPING → STOPPED`
- `STARTING → ERROR` (cannot go through STOPPING)
- `ERROR → STOPPED` (cleans up zombie process, enables restart)

**Postconditions**:
- Cloud Hypervisor process force-killed (SIGKILL)
- PID cleared in metadata
- VM in STOPPED or ERROR state

**Idempotency**:
- Killing STOPPED VM → No-op, return success

#### inspect

**Signature**: `cocoon inspect VM_ID [--format json]`

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

#### list

**Signature**: `cocoon ps [-a]`

**Preconditions**:
- None

**State Changes**:
- None (read-only operation)

**Output**:
- List of all VMs with their current state, name, and summary info
- Returns `VMInspect` structs (merged view of config.json and metadata.json)
- Skips VMs with missing or corrupt data

### 3.3 Manager Interface

All operations are defined as methods on the `vm.Manager` interface (`vm/vm.go`):

```go
type Manager interface {
    // CRUD operations.
    Create(ctx context.Context, opts *CreateOptions) (*types.VMConfig, error)
    Start(ctx context.Context, vmID string) error
    Stop(ctx context.Context, vmID string, timeout time.Duration) error
    Kill(ctx context.Context, vmID string) error
    Delete(ctx context.Context, vmID string, force bool) error
    Inspect(ctx context.Context, vmID string) (*types.VMInspect, error)
    List(ctx context.Context) ([]*types.VMInspect, error)

    // Name resolution.
    ResolveVMRef(ref string) (string, error)

    // State management.
    TransitionState(vmID string, to types.VMState, reason string) error
    LoadConfig(vmID string) (*types.VMConfig, error)
    LoadMetadata(vmID string) (*types.VMMetadataFile, error)
    SaveMetadata(meta *types.VMMetadataFile) error
    UpdateMetadata(vmID string, mutate func(*types.VMMetadataFile)) error

    // Reconciliation.
    Reconcile(ctx context.Context, fix bool, force bool) ([]Inconsistency, error)
}
```

The concrete implementation is `*manager` in `vm/engine/manager.go`.

---

## 4. Inspect Output Schema (Merged View)

This section defines the **merged view** struct returned by `cocoon inspect`. It combines data from the immutable `config.json` and mutable `metadata.json` files (see Section 5 for the on-disk split). This struct is never persisted as a single file — it exists only in memory and in API/CLI output.

> **Phase 1 note:** The actual implementation uses a lean `VMInspect` struct
> (`types/inspect.go`) that groups fields into nested sub-structs: `image`,
> `storage`, `hypervisor`, `boot_config`, `timestamps`, `runtime`, and `error`.
> Fields like `state_history` and extended storage/hypervisor
> metadata (`used_bytes`, `filesystem`, `version`, `api_version`) are not
> included in Phase 1.

### 4.1 Complete Inspect Structure

```go
package types

// VMInspect is the merged view returned by "cocoon inspect".
// Combines data from config.json (immutable) and metadata.json (mutable).
// This struct is never persisted as a single file.
type VMInspect struct {
    VMID  string  `json:"vm_id"`
    Name  string  `json:"name"`
    State VMState `json:"state"`

    Image      InspectImageInfo      `json:"image"`
    Storage    InspectStorageInfo    `json:"storage"`
    Hypervisor InspectHypervisorInfo `json:"hypervisor"`
    BootConfig InspectBootConfig     `json:"boot_config"`
    Timestamps InspectTimestamps     `json:"timestamps"`
    Runtime    InspectRuntimeStatus  `json:"runtime"`
    Error      *InspectErrorInfo     `json:"error,omitempty"`
}

// InspectImageInfo contains OCI image details.
type InspectImageInfo struct {
    Ref     string      `json:"ref"`
    Digest  string      `json:"digest"`
    BaseKey string      `json:"base_key"`
    Type    VMImageType `json:"type,omitempty"`
}

// InspectStorageInfo contains disk information.
type InspectStorageInfo struct {
    OverlayPath string `json:"overlay_path"`
    BasePath    string `json:"base_path"`
    Size        string `json:"size"`
}

// InspectHypervisorInfo contains Cloud Hypervisor details.
type InspectHypervisorInfo struct {
    CHSocket         string   `json:"ch_socket"`
    CHPID            int      `json:"ch_pid"`
    SerialLog        string   `json:"serial_log"`
    ConsolePTY       string   `json:"console_pty,omitempty"` // PTY path when console mode is Pty
    VirtiofsdPID     int      `json:"virtiofsd_pid,omitempty"`
    VirtiofsdSocket  string   `json:"virtiofsd_socket,omitempty"`
    SerialLogExcerpt []string `json:"serial_log_excerpt,omitempty"`
}

// InspectBootConfig contains boot configuration.
type InspectBootConfig struct {
    CPUs         int          `json:"cpus"`
    MemoryMB     int64        `json:"memory_mb"`
    BootStrategy BootStrategy `json:"boot_strategy"`
    FirmwarePath string       `json:"firmware_path"`
}

// InspectTimestamps tracks VM lifecycle events.
type InspectTimestamps struct {
    CreatedAt string `json:"created_at"`
    UpdatedAt string `json:"updated_at"`
    StartedAt string `json:"started_at,omitempty"`
    StoppedAt string `json:"stopped_at,omitempty"`
}

// InspectRuntimeStatus contains runtime execution information.
type InspectRuntimeStatus struct {
    BootTime          string `json:"boot_time,omitempty"`
    LastBootMode      string `json:"last_boot_mode,omitempty"`
    ErrorCount        int    `json:"error_count"`
    OCIOverlayMounted bool   `json:"oci_overlay_mounted,omitempty"`
}

// InspectErrorInfo contains error details.
type InspectErrorInfo struct {
    Message   string `json:"message"`
    Type      string `json:"type"`
    Timestamp string `json:"timestamp"`
}
```

### 4.2 Example Inspect Output (JSON)

```json
{
  "vm_id": "vm-abc123",
  "name": "myvm",
  "state": "RUNNING",

  "image": {
    "ref": "myorg/ubuntu-bootable:22.04",
    "digest": "sha256:abcd1234...",
    "base_key": "ef015678abcd1234_amd64"
  },

  "storage": {
    "overlay_path": "/var/lib/cocoon/vms/vm-abc123/overlay.qcow2",
    "base_path": "/var/lib/cocoon/cache/images/ef015678abcd1234_amd64.qcow2",
    "size": "10G"
  },

  "hypervisor": {
    "ch_socket": "/run/cocoon/vms/vm-abc123/api.sock",
    "ch_pid": 12345,
    "serial_log": "/var/log/cocoon/vm-abc123-serial.log",
    "serial_log_excerpt": [
      "[    0.000000] Linux version 6.8.0...",
      "Ubuntu 22.04.5 LTS ready"
    ]
  },

  "boot_config": {
    "cpus": 2,
    "memory_mb": 2048,
    "boot_strategy": "uefi",
    "firmware_path": "/var/lib/cocoon/firmware/CLOUDHV.fd"
  },

  "timestamps": {
    "created_at": "2026-02-11T20:00:00Z",
    "updated_at": "2026-02-11T20:01:30Z",
    "started_at": "2026-02-11T20:01:00Z"
  },

  "runtime": {
    "boot_time": "2.3s",
    "last_boot_mode": "uefi",
    "error_count": 0
  }
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
    ImageRef        string `json:"image_ref"`          // Original image reference (path/URL/OCI ref)
    BaseKey         string `json:"base_key"`           // Content-addressed key: {checksum_16}_{arch} (e.g., "a1b2c3d4e5f6a7b8_amd64")
    BaseDigestFull  string `json:"base_digest_full"`   // Full SHA-256 hex (64 chars) for collision audit
    Arch            string `json:"arch"`               // Architecture: "amd64", "arm64", etc.

    // Boot configuration (immutable)
    BootStrategy  BootStrategy `json:"boot_strategy"`            // "uefi" (default), "direct" (OCI)
    FirmwarePath  string       `json:"firmware_path"`             // Primary firmware path resolved at creation
    KernelPath    string       `json:"kernel_path,omitempty"`     // Direct boot kernel (OCI VM images)
    InitramfsPath string       `json:"initramfs_path,omitempty"`  // Direct boot initramfs (OCI VM images)
    Cmdline       string       `json:"cmdline,omitempty"`         // Direct boot kernel cmdline (OCI VM images)
    TPMSocketPath string       `json:"tpm_socket_path,omitempty"` // swtpm socket path (if TPM enabled)

    // Resources (immutable after create; Phase 2 may allow resize)
    CPUs     int    `json:"cpus"`
    MemoryMB int64  `json:"memory_mb"`        // Internal: always bytes-convertible
    DiskSize string `json:"disk_size"`         // Overlay size, e.g. "10G"

    // Storage paths (derived from base_key, stored for fast lookup)
    BaseImagePath string `json:"base_image_path"` // Derived: /var/lib/cocoon/cache/images/{base_key}.qcow2
    OverlayPath   string `json:"overlay_path"`    // /var/lib/cocoon/vms/{vm_id}/overlay.qcow2
    SerialLog     string `json:"serial_log"`      // /var/log/cocoon/{vm_id}-serial.log
    SocketPath    string `json:"socket_path"`     // /run/cocoon/vms/{vm_id}/api.sock

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
  "base_key": "ef015678abcd1234_amd64",
  "base_digest_full": "ef015678abcd1234567890abcdef1234567890abcdef1234567890abcdef1234",
  "arch": "amd64",
  "boot_strategy": "uefi",
  "firmware_path": "/var/lib/cocoon/firmware/CLOUDHV.fd",
  "cpus": 2,
  "memory_mb": 2048,
  "disk_size": "10G",
  "base_image_path": "/var/lib/cocoon/cache/images/ef015678abcd1234_amd64.qcow2",
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
type VMMetadataFile struct {
    VMID          string `json:"vm_id"`            // Must match config.json
    State         string `json:"state"`            // Current state: CREATING/CREATED/STARTING/RUNNING/STOPPING/STOPPED/ERROR/DELETED
    PreviousState string `json:"previous_state"`   // For transition auditing

    // Runtime (changes with each start/stop cycle)
    ProcessPID        int    `json:"process_pid,omitempty"`        // CH process PID (0 if not running)
    HypervisorBinary  string `json:"hypervisor_binary,omitempty"`  // Basename of hypervisor process used for liveness checks
    VirtiofsdPID      int    `json:"virtiofsd_pid,omitempty"`      // Rootfs virtiofsd PID (OCI runtime VMs only; 0 when not running)
    VirtiofsdSocket   string `json:"virtiofsd_socket,omitempty"`   // Rootfs-serving virtiofsd socket path (OCI runtime VMs only)
    VirtiofsdBinary   string `json:"virtiofsd_binary,omitempty"`   // Basename of virtiofsd process used for liveness checks
    OCIOverlayMounted bool   `json:"oci_overlay_mounted,omitempty"` // Whether the per-VM OCI OverlayFS mount is currently active
    BootTime          string `json:"boot_time,omitempty"`          // Duration string, e.g. "2.3s"
    LastBootMode      string `json:"last_boot_mode,omitempty"`     // Actual mode used: "uefi" or "direct"
    LastFirmwarePath  string `json:"last_firmware_path,omitempty"` // Actual firmware path used this boot

    // Error tracking
    LastError     string `json:"last_error,omitempty"`
    LastErrorType string `json:"last_error_type,omitempty"`
    LastErrorAt   string `json:"last_error_at,omitempty"`
    ErrorCount    int    `json:"error_count"`

    // Lifecycle flags
    AutoRemove bool `json:"auto_remove,omitempty"` // Auto-delete on stop (set by --rm)

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
  "last_boot_mode": "uefi",
  "last_firmware_path": "/var/lib/cocoon/firmware/CLOUDHV.fd",
  "last_error": "",
  "last_error_type": "",
  "last_error_at": "",
  "error_count": 0,
  "auto_remove": false,
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

**config.json** is the source of truth for "what this VM should be." Reconcile uses it to reconstruct expected configuration — it knows the VM exists, what image it was created from (`base_key` links to `references.json` and cache), how many CPUs it should have, and where its files live. The `base_key` field is the same key used in `references.json` and cache filenames (see [05-storage-management.md § Canonical Filesystem Layout](./05-storage-management.md#canonical-filesystem-layout-normative)).

**metadata.json** is the source of truth for "what this VM is doing now." It tracks runtime state, the Cloud Hypervisor process PID, error history, and timestamps for the current lifecycle.

**On crash recovery**: `config.json` survives intact (it is never modified after creation). `metadata.json` may be stale if the crash occurred during a state transition. Reconciliation reads `config.json` to know the VM exists and its expected configuration, then probes actual system state (is the process alive? is the socket responsive?) to rebuild `metadata.json` accurately.

**On upgrade/migration**: The `SchemaVersion` field in both files enables forward migration. `config.json` rarely changes schema since its fields are stable by design. `metadata.json` schema may evolve more frequently as new runtime tracking is added.

### 5.4 Relationship to Section 4

Section 4 defines the `VMInspect` struct as a **merged view** — it represents the combined data returned by `cocoon inspect`, which reads from both `config.json` and `metadata.json` and assembles a unified JSON response. **The on-disk format is always split** into `config.json` (immutable) and `metadata.json` (mutable) from Phase 1 onwards. There is no "combined file" phase; the Section 4 struct is purely an API/display model.

---

## 6. Metadata Persistence

### 6.1 Storage Path Structure

```
/var/lib/cocoon/
├── db/
│   └── name-index.json        # Global name → vm_id mapping (see Section 1.4.3)
└── vms/{vm-id}/
    ├── config.json            # Immutable VM configuration (see Section 5.1)
    ├── metadata.json          # Mutable runtime state (see Section 5.2)
    ├── metadata.lock          # File lock for atomic updates
    ├── overlay.qcow2          # VM overlay disk
    └── tpm/                   # TPM 2.0 state directory (if TPM enabled)

/run/cocoon/vms/{vm-id}/
├── api.sock                   # Cloud Hypervisor API socket
├── ch.pid                     # Cloud Hypervisor process PID file
├── swtpm.sock                 # swtpm TPM socket (if TPM enabled)
└── swtpm.pid                  # swtpm process PID file (if TPM enabled)

/var/log/cocoon/
├── {vm-id}-serial.log         # Serial console output
├── {vm-id}-ch.log             # Cloud Hypervisor log
└── {vm-id}-swtpm.log          # swtpm log (if TPM enabled)
```

### 6.2 Atomic Updates

Metadata updates use atomic write with temporary file + rename:

```go
package types

import (
    "encoding/json"
    "os"
    "path/filepath"
    "syscall"
    "time"
)

// SaveMetadata persists a VM's metadata.json atomically under flock.
// Uses metadata.lock (Level 4) to serialize concurrent writers.
// Writes via utils.AtomicWriteJSON (temp file + fsync + rename).
func (m *manager) SaveMetadata(meta *types.VMMetadataFile) error {
    lockPath := m.cfg.VMMetadataLock(meta.VMID)
    fl := flock.New(lockPath)
    if err := fl.Lock(); err != nil {
        return fmt.Errorf("acquire metadata lock for %s: %w", meta.VMID, err)
    }
    defer fl.Unlock()

    meta.UpdatedAt = time.Now().UTC().Format(time.RFC3339)

    metaPath := m.cfg.VMMetadataPath(meta.VMID)
    return utils.AtomicWriteJSON(metaPath, meta)
}

// LoadMetadata reads a VM's mutable metadata.json.
// Reads are lock-free because metadata.json is always atomically replaced.
// Uses utils.ReadJSON internally.
func (m *manager) LoadMetadata(vmID string) (*types.VMMetadataFile, error) {
    metaPath := m.cfg.VMMetadataPath(vmID)
    var meta types.VMMetadataFile
    if err := utils.ReadJSON(metaPath, &meta); err != nil {
        if os.IsNotExist(err) {
            return nil, fmt.Errorf("%w: %s", types.ErrVMNotFound, vmID)
        }
        return nil, fmt.Errorf("read metadata for %s: %w", vmID, err)
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
func (m *manager) Create(ctx context.Context, opts *vm.CreateOptions) (*types.VMConfig, error) {
    // Name uniqueness checked atomically via AddName under name-index.lock
    // If name already in use, returns types.ErrVMAlreadyExists
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
func (m *manager) Start(ctx context.Context, vmID string) error {
    meta, err := m.LoadMetadata(vmID)
    if err != nil {
        return err
    }

    state := types.VMState(meta.State)

    // Idempotent: already running → no-op
    if state == types.VMStateRunning {
        return nil
    }

    // Reject if already starting
    if state == types.VMStateStarting {
        return fmt.Errorf("VM %s is already starting", vmID)
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
func (m *manager) Stop(ctx context.Context, vmID string, timeout time.Duration) error {
    meta, err := m.LoadMetadata(vmID)
    if err != nil {
        return err
    }

    state := types.VMState(meta.State)

    // Idempotent: already stopped → no-op (best-effort OCI runtime cleanup)
    if state == types.VMStateStopped {
        return nil
    }

    // If already stopping, wait for process exit with polling
    if state == types.VMStateStopping {
        // Poll m.hyper.IsAlive(vmID) until timeout
        // If process is observed exited, transition to STOPPED and run
        // best-effort OCI runtime cleanup before returning.
        // ...
    }

    // Proceed with stop: RUNNING -> STOPPING via TransitionState,
    // then m.hyper.Shutdown() for graceful ACPI shutdown.
    // On timeout, Shutdown() internally calls ForceKill():
    //   - ForceKill succeeds → transition to STOPPED
    //   - ForceKill fails → transition to ERROR
    // After STOPPED transition, run best-effort OCI runtime cleanup.
    // ...
}
```

**Behavior**:
- Stopping STOPPED VM → **No-op, success**
- Stopping STOPPING VM → **Wait for completion + cleanup OCI runtime once STOPPED is observed**
- **Idempotent for STOPPED state**

#### delete

```go
func (m *manager) Delete(ctx context.Context, vmID string, force bool) error {
    // Idempotent: VM doesn't exist → no-op
    meta, err := m.LoadMetadata(vmID)
    if isNotFound(err) {
        return nil // already gone
    }

    // If RUNNING, require --force; attempt graceful stop then force kill
    if types.VMState(meta.State) == types.VMStateRunning {
        if !force {
            return types.ErrVMRunning
        }
        if stopErr := m.Stop(ctx, vmID, 10*time.Second); stopErr != nil {
            _ = m.hyper.ForceKill(vmID)
        }
    }

    // Transition to DELETED, then cleanup: unpin reference, remove overlay,
    // remove name from index, remove VM directories and log files.
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
    ErrorImageNotBootable ErrorType = "image_not_bootable"

    // Boot errors
    ErrorBootTimeout       ErrorType = "boot_timeout"
    ErrorKernelPanic       ErrorType = "kernel_panic"
    ErrorBootFailure       ErrorType = "boot_failure"
    ErrorMissingBootloader ErrorType = "missing_bootloader"
    ErrorMissingKernel     ErrorType = "missing_kernel"

    // Runtime errors
    ErrorCHCrash            ErrorType = "cloud_hypervisor_crash"
    ErrorGuestCrash         ErrorType = "guest_crash"
    ErrorResourceExhaustion ErrorType = "resource_exhaustion"

    // Shutdown errors
    ErrorStopTimeout     ErrorType = "stop_timeout"
    ErrorForceKillFailed ErrorType = "force_kill_failed"

    // Reference errors
    ErrorChecksumCollision ErrorType = "checksum_collision"
)
```

### 8.2 Error State Handling

There is no standalone `HandleError` function. Error tracking is handled **inline** by `transitionStateWithUpdate` in `vm/engine/manager.go`. When any caller transitions a VM to ERROR state via `TransitionState(vmID, types.VMStateError, reason)`, the method automatically:

1. Sets `LastError` to the reason string
2. Sets `LastErrorAt` to the current timestamp (RFC 3339)
3. Increments `ErrorCount`

```go
// Inside transitionStateWithUpdate (vm/engine/manager.go):
if to == types.VMStateError {
    meta.LastError = reason
    meta.LastErrorAt = time.Now().UTC().Format(time.RFC3339)
    meta.ErrorCount++
}
```

**Callers pass descriptive reasons** when transitioning to ERROR:
```go
// Boot failure:
m.TransitionState(vmID, types.VMStateError, fmt.Sprintf("boot failed: %v", bootErr))

// Graceful stop failure:
m.TransitionState(vmID, types.VMStateError, fmt.Sprintf("graceful stop failed: %v", err))

// Force kill during start:
m.TransitionState(vmID, types.VMStateError, "force killed during start")
```

Process cleanup (force kill) is handled by the individual operation methods (Stop, Kill, Delete) rather than by a centralized error handler.

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
- VMs in ERROR state cannot be started directly
- Recovery path: `cocoon kill` (ERROR -> STOPPED), then `cocoon start` (STOPPED -> STARTING -> RUNNING)
- Alternatively: `cocoon delete` to remove the failed VM entirely
- There is no automatic retry; the user decides whether to recover or delete

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

#### 9.2.3 Inconsistency Types

The following types are defined in `vm/types.go`:

```go
// InconsistencyType classifies the kind of reconciliation inconsistency.
type InconsistencyType string

const (
    InconsistencyStateMismatch     InconsistencyType = "state_mismatch"
    InconsistencyMetadataCorrupt   InconsistencyType = "metadata_corrupted"
    InconsistencyStalePIDFile      InconsistencyType = "stale_pid_file"
    InconsistencyZombieSocket      InconsistencyType = "zombie_socket"
    InconsistencyZombieProcess     InconsistencyType = "zombie_process"
    InconsistencyMissingOverlay    InconsistencyType = "missing_overlay"
    InconsistencyOrphanedOverlay   InconsistencyType = "orphaned_overlay"
    InconsistencyMissingReference  InconsistencyType = "missing_reference"
    InconsistencyDanglingReference InconsistencyType = "dangling_reference"
    InconsistencyNameIndexStale    InconsistencyType = "name_index_stale"
    InconsistencyDuplicateVMName   InconsistencyType = "duplicate_vm_name"
    InconsistencyDeletedVMDir      InconsistencyType = "deleted_vm_directory"

    // OCI runtime inconsistencies
    InconsistencyOCIRuntimeCache       InconsistencyType = "oci_runtime_cache_missing"
    InconsistencyOCIRuntimeOverlay     InconsistencyType = "oci_runtime_overlay_mismatch"
    InconsistencyOCIRuntimeVirtio      InconsistencyType = "oci_runtime_virtiofsd_mismatch"
    InconsistencyMissingOCIRuntimePin  InconsistencyType = "missing_oci_runtime_pin"
    InconsistencyDanglingOCIRuntimePin InconsistencyType = "dangling_oci_runtime_pin"
    InconsistencyOrphanedOCIRuntime    InconsistencyType = "orphaned_oci_runtime_cache"
)

// InconsistencySeverity indicates how serious an inconsistency is.
type InconsistencySeverity string

const (
    SeverityCritical InconsistencySeverity = "critical"
    SeverityWarning  InconsistencySeverity = "warning"
    SeverityInfo     InconsistencySeverity = "info"
)

// Inconsistency represents a detected discrepancy between expected and actual VM state.
type Inconsistency struct {
    VMID          string                `json:"vm_id"`
    Type          InconsistencyType     `json:"type"`
    Severity      InconsistencySeverity `json:"severity"`
    Details       string                `json:"details"`
    BaseKey       string                `json:"base_key,omitempty"`
    DigestFull    string                `json:"digest_full,omitempty"`
    ImageRef      string                `json:"image_ref,omitempty"`
    ExpectedState string                `json:"expected_state,omitempty"`
    ActualState   string                `json:"actual_state,omitempty"`
}
```

Note that `Type` is `InconsistencyType` (a typed string), `Severity` is `InconsistencySeverity` (a typed string), and `ExpectedState`/`ActualState` are plain `string` (not `VMState`). The `BaseKey`, `DigestFull`, and `ImageRef` fields are used for reference reconciliation issues.

#### 9.2.4 Reconciliation Algorithm

**On Startup (future daemon mode — Phase 2) or `cocoon doctor --fix`**:

```go
// Reconcile scans all VMs and detects inconsistencies between metadata and
// actual system state. When fix is true, it attempts to repair them. When
// force is true, it will also kill zombie processes and force-move stuck VMs.
//
// In addition to VM state/process/socket/overlay checks, Reconcile also:
//   - Detects dangling references (references.json entries with no matching VM)
//   - Detects missing references (VM exists but references.json lacks its entry)
//   - Detects name index staleness and rebuilds it if needed
//   - Detects orphaned swtpm processes (not just cloud-hypervisor)
//   - Detects deleted VM directories that still have resources
//   - Detects missing config.json (orphaned VM directories)
//   - Detects duplicate VM names across config.json files
func (m *manager) Reconcile(ctx context.Context, fix bool, force bool) ([]vm.Inconsistency, error) {
    // 1. SCAN: Discover all VMs
    entries, err := os.ReadDir(m.cfg.VMDir())
    if err != nil {
        if os.IsNotExist(err) {
            return nil, nil
        }
        return nil, fmt.Errorf("scan VM directory: %w", err)
    }

    var inconsistencies []vm.Inconsistency
    knownPIDs := make(map[int]string)  // pid -> vmID
    vmConfigs := make(map[string]*types.VMConfig)

    // 2. ANALYZE: Check each VM
    for _, entry := range entries {
        if !entry.IsDir() {
            continue
        }
        vmID := entry.Name()

        // 2a. Check config.json existence (source of truth for VM existence)
        // If missing, detect orphaned overlay or orphaned directory.
        configPath := m.cfg.VMConfigPath(vmID)
        if _, statErr := os.Stat(configPath); os.IsNotExist(statErr) {
            // ... detect orphaned overlay or missing config ...
            continue
        }

        // 2b. Load config and check references.json contains this VM
        vmCfg, cfgErr := m.LoadConfig(vmID)
        if cfgErr != nil {
            inconsistencies = append(inconsistencies, vm.Inconsistency{
                VMID: vmID, Type: vm.InconsistencyMetadataCorrupt,
                Severity: vm.SeverityCritical, Details: cfgErr.Error(),
            })
            continue
        }
        vmConfigs[vmID] = vmCfg

        // Check references.json contains vmID under its base_key
        // (detects crash between config write and reference pin)

        // 2c. Load metadata
        meta, metaErr := m.LoadMetadata(vmID)
        if metaErr != nil {
            inconsistencies = append(inconsistencies, vm.Inconsistency{
                VMID: vmID, Type: vm.InconsistencyMetadataCorrupt,
                Severity: vm.SeverityCritical, Details: metaErr.Error(),
            })
            continue
        }

        // 2d. Detect DELETED state with existing directory
        if types.VMState(meta.State) == types.VMStateDeleted {
            inconsistencies = append(inconsistencies, vm.Inconsistency{
                VMID: vmID, Type: vm.InconsistencyDeletedVMDir,
                Severity: vm.SeverityInfo,
                Details:  "metadata state is DELETED but VM directory/resources still exist",
            })
            continue
        }

        // Track known PIDs for orphan detection
        if meta.ProcessPID > 0 {
            knownPIDs[meta.ProcessPID] = vmID
        }

        // 2e. Determine actual state by probing the system
        actualState := m.determineActualState(meta, vmCfg)

        if actualState != types.VMState(meta.State) {
            inconsistencies = append(inconsistencies, vm.Inconsistency{
                VMID:          vmID,
                Type:          vm.InconsistencyStateMismatch,
                Severity:      reconcileSeverity(types.VMState(meta.State), actualState),
                Details:       fmt.Sprintf("metadata=%s, actual=%s", meta.State, actualState),
                ExpectedState: meta.State,
                ActualState:   string(actualState),
            })
        }

        // 2f. Check for zombie resources (stale PID files, zombie sockets)
        zombies := m.detectZombieResources(vmID, meta, vmCfg)
        inconsistencies = append(inconsistencies, zombies...)

        // 2g. Check overlay existence
        // ...
    }

    // 3. Cross-VM checks
    // 3a. Detect dangling references (references.json entries with no matching VM config)
    refIssues, _ := m.detectDanglingReferenceIssues(vmConfigs)
    inconsistencies = append(inconsistencies, refIssues...)

    // 3b. Detect name index staleness (rebuild index from config.json files)
    nameIssues, _ := m.detectNameIndexIssues(vmConfigs)
    inconsistencies = append(inconsistencies, nameIssues...)

    // 4. FIX: Apply fixes if requested
    if fix {
        for i := range inconsistencies {
            if err := m.applyFix(&inconsistencies[i], force); err != nil {
                inconsistencies[i].Details += fmt.Sprintf(" (fix failed: %v)", err)
            }
        }
    }

    // 5. Detect orphaned cloud-hypervisor AND swtpm processes not tracked by any VM
    orphans := detectOrphanedProcesses(knownPIDs)
    inconsistencies = append(inconsistencies, orphans...)

    return inconsistencies, nil
}

// determineActualState probes the system to find out what a VM is really doing.
// Uses the priority order: process status > socket > metadata > overlay > PID file.
func (m *manager) determineActualState(meta *types.VMMetadataFile, vmCfg *types.VMConfig) types.VMState {
    pid := meta.ProcessPID
    socketPath := vmCfg.SocketPath

    // Check process
    processRunning := utils.IsProcessAlive(pid)
    processValid := false
    if processRunning {
        processValid = utils.ValidateProcess(pid, "cloud-hypervisor")
    }

    // Check socket
    socketConnectable := false
    if socketPath != "" {
        if _, err := os.Stat(socketPath); err == nil {
            socketConnectable = canConnectToSocket(socketPath)
        }
    }

    // Determine actual state based on evidence
    switch types.VMState(meta.State) {
    case types.VMStateRunning:
        if processValid && socketConnectable {
            return types.VMStateRunning // Genuinely running
        }
        return types.VMStateError // Process dead or PID reused -> crashed

    case types.VMStateStarting:
        if processValid {
            if isStuckInState(meta.UpdatedAt, 5*time.Minute) {
                return types.VMStateError // Stuck in STARTING
            }
            return types.VMStateStarting // Still starting (give it time)
        }
        return types.VMStateError // Process died during start

    case types.VMStateStopping:
        if processValid {
            if isStuckInState(meta.UpdatedAt, 2*time.Minute) {
                return types.VMStateError // Stuck in STOPPING
            }
            return types.VMStateStopping // Still stopping
        }
        return types.VMStateStopped // Process exited

    case types.VMStateStopped:
        if processRunning {
            return types.VMStateError // Process shouldn't be running
        }
        return types.VMStateStopped // Correctly stopped

    case types.VMStateCreated:
        if processRunning {
            return types.VMStateError // Unexpected process
        }
        return types.VMStateCreated

    case types.VMStateDeleted:
        return types.VMStateError // Directory should not exist

    case types.VMStateError:
        return types.VMStateError

    case types.VMStateCreating:
        return types.VMStateError // Creation did not complete

    default:
        return types.VMState(meta.State)
    }
}

// detectZombieResources finds stale PID files and zombie sockets.
func (m *manager) detectZombieResources(vmID string, meta *types.VMMetadataFile, vmCfg *types.VMConfig) []vm.Inconsistency {
    var zombies []vm.Inconsistency

    // Check PID file
    pidFilePath := m.cfg.VMPIDPath(vmID)
    if pidData, err := os.ReadFile(pidFilePath); err == nil {
        pidFromFile, _ := strconv.Atoi(strings.TrimSpace(string(pidData)))
        if pidFromFile > 0 && pidFromFile != meta.ProcessPID {
            zombies = append(zombies, vm.Inconsistency{
                VMID:     vmID,
                Type:     vm.InconsistencyStalePIDFile,
                Severity: vm.SeverityWarning,
                Details:  fmt.Sprintf("PID file has %d, metadata has %d", pidFromFile, meta.ProcessPID),
            })
        }
    }

    // Check for zombie socket (socket path comes from config.json)
    if vmCfg.SocketPath != "" {
        if _, err := os.Stat(vmCfg.SocketPath); err == nil {
            if !utils.ValidateProcess(meta.ProcessPID, "cloud-hypervisor") {
                zombies = append(zombies, vm.Inconsistency{
                    VMID:     vmID,
                    Type:     vm.InconsistencyZombieSocket,
                    Severity: vm.SeverityWarning,
                    Details:  fmt.Sprintf("socket exists at %s but process %d not running", vmCfg.SocketPath, meta.ProcessPID),
                })
            }
        }
    }

    return zombies
}

// applyFix attempts to repair an inconsistency.
// Handles: state_mismatch, zombie_socket, stale_pid_file, zombie_process,
// orphaned_overlay, missing_reference, dangling_reference, name_index_stale,
// deleted_vm_directory, missing_overlay, and metadata_corrupted.
func (m *manager) applyFix(inc *vm.Inconsistency, force bool) error {
    switch inc.Type {
    case vm.InconsistencyStateMismatch:
        return m.fixStateMismatch(inc, force)

    case vm.InconsistencyZombieSocket:
        vmCfg, err := m.LoadConfig(inc.VMID)
        if err != nil {
            return err
        }
        return os.Remove(vmCfg.SocketPath)

    case vm.InconsistencyStalePIDFile:
        return os.Remove(m.cfg.VMPIDPath(inc.VMID))

    case vm.InconsistencyZombieProcess:
        if !force {
            return fmt.Errorf("--force required to kill zombie processes")
        }
        // Only kill if actually cloud-hypervisor (guard against PID reuse)
        // ...

    case vm.InconsistencyMissingReference:
        // Re-pin reference using base_key from inc or config.json
        return m.refCounter.AddReference(inc.BaseKey, inc.VMID, inc.DigestFull, inc.ImageRef)

    case vm.InconsistencyDanglingReference:
        return m.refCounter.RemoveReference(inc.BaseKey, inc.VMID)

    case vm.InconsistencyNameIndexStale:
        _, err := RebuildNameIndex(m.cfg)
        return err

    case vm.InconsistencyDeletedVMDir:
        return m.cleanupDeletedVMArtifacts(inc.VMID)

    // ... additional cases for orphaned_overlay, missing_overlay, etc.
    default:
        return fmt.Errorf("unknown inconsistency type: %s", inc.Type)
    }

    return nil
}

// Process/socket probing helpers live in the utils package:
//   - utils.IsProcessAlive(pid) bool
//   - utils.ValidateProcess(pid, expectedName) bool
//   - canConnectToSocket(socketPath) bool (local to engine package)
```

#### 9.2.5 Crash Scenarios in Detail

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
# Dry-run: Report inconsistencies only (default behavior)
cocoon doctor

# Fix inconsistencies automatically
cocoon doctor --fix

# Force cleanup of stuck VMs and kill zombie processes
cocoon doctor --fix --force
```

**Note**: There is no separate `cocoon reconcile` command. `cocoon doctor` runs
reconciliation by default (dry-run mode). Use `--fix` to apply repairs.

**Flags**:
- `--fix`: Automatically fix inconsistencies (default: dry-run)
- `--force`: Force cleanup of stuck VMs and kill zombie processes
- `--format`: Output format (`table`, `json`)

**OCI runtime note**: For OCI VMs, fixing `state_mismatch` to `STOPPED` or `ERROR` also triggers best-effort OCI runtime teardown (`virtiofsd` stop, overlay unmount, metadata cleanup) to avoid leaked runtime artifacts.

**Output**:
```bash
$ cocoon doctor
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

Run 'cocoon doctor --fix' to repair inconsistencies.
```

### 9.5 Reconciliation Checks Summary

The `Reconcile` method on `*manager` performs the following checks:

| Check | Inconsistency Type | Description |
|-------|-------------------|-------------|
| State vs process/socket | `state_mismatch` | Metadata state does not match actual system state |
| Config.json missing | `metadata_corrupted` | VM directory exists but config.json is absent or unreadable |
| Metadata.json missing | `metadata_corrupted` | Config exists but metadata.json is absent or unreadable |
| Stale PID file | `stale_pid_file` | PID in ch.pid differs from metadata PID |
| Zombie socket | `zombie_socket` | API socket exists but process is dead |
| Zombie process | `zombie_process` | Orphaned cloud-hypervisor or swtpm process not tracked by any VM |
| Missing overlay | `missing_overlay` | Overlay disk missing for a non-deleted VM |
| Orphaned overlay | `orphaned_overlay` | Overlay exists but config.json is missing |
| Missing reference | `missing_reference` | VM config exists but references.json lacks its entry |
| Dangling reference | `dangling_reference` | references.json entry points to non-existent VM |
| Name index stale | `name_index_stale` | name-index.json is out of sync with VM configs |
| Duplicate VM name | `duplicate_vm_name` | Two config.json files claim the same name |
| Deleted VM dir | `deleted_vm_directory` | Metadata state is DELETED but directory still exists |
| OCI runtime cache missing | `oci_runtime_cache_missing` | OCI VM config references a runtime cache entry that does not exist on disk |
| OCI runtime overlay mismatch | `oci_runtime_overlay_mismatch` | OCI OverlayFS mount state differs between metadata and actual mount status |
| OCI runtime virtiofsd mismatch | `oci_runtime_virtiofsd_mismatch` | virtiofsd process state differs between metadata and actual process status |
| Missing OCI runtime pin | `missing_oci_runtime_pin` | OCI VM config exists but oci-runtime-refs.json lacks its pin entry |
| Dangling OCI runtime pin | `dangling_oci_runtime_pin` | oci-runtime-refs.json entry points to non-existent VM |
| Orphaned OCI runtime cache | `orphaned_oci_runtime_cache` | OCI runtime cache directory exists with no VM pins |

### 9.6 Reconciliation Schedule

**When to Run**:
1. **On CLI invocation** (`cocoon doctor --fix` or future daemon mode — Phase 2)
2. **Periodically** (Phase 2 daemon mode — not yet implemented; currently reconciliation runs on-demand via `cocoon doctor`)
3. **Manually** (user runs `cocoon doctor --fix`)
4. **After crashes** (detect on next CLI invocation)

**Dry-run by Default**:
- Without `--fix`, only reports inconsistencies
- User reviews and decides whether to fix
- Prevents accidental destructive actions

---

## 10. Implementation Guide

### 10.1 Phase 1: Core State Machine (P0)

**Tasks**:
- [x] Implement `VMState` enum and validation (`types/state.go`)
- [x] Implement `TransitionState()` with validation (`vm/engine/manager.go`)
- [x] Implement `VMMetadataFile` struct (`types/metadata.go`)
- [x] Implement atomic metadata persistence (`utils/atomic.go`, `vm/engine/manager.go`)
- [x] Add state checks to all operations (`vm/engine/manager.go`)

**Files**:
- `types/state.go`: State machine (VMState, ValidTransitions, ValidateTransition)
- `types/metadata.go`: VMMetadataFile struct
- `types/config.go`: VMConfig struct, default resource values
- `types/errors.go`: ErrorType, sentinel errors, ClassifiedError
- `vm/vm.go`: Manager interface definition
- `vm/types.go`: CreateOptions, Inconsistency types, NameIndex
- `vm/engine/manager.go`: Manager implementation (CRUD, state transitions, metadata persistence)
- `vm/engine/reconcile.go`: Reconciliation logic (Reconcile, determineActualState, detectZombieResources, applyFix)
- `config/config.go`: CocoonConfig, derived path helpers

### 10.2 Phase 2: Operations (P0)

**Tasks**:
- [x] Implement `create` with CREATING → CREATED transition (`vm/engine/manager.go`)
- [x] Implement `start` with STARTING → RUNNING transition (`vm/engine/manager.go`)
- [x] Implement `stop` with STOPPING → STOPPED transition (`vm/engine/manager.go`)
- [x] Implement `delete` with → DELETED transition (`vm/engine/manager.go`)
- [x] Implement `inspect` (read-only) (`cmd/cocoon/inspect.go`)

**Error Handling**:
- [x] Implement error state transitions (`vm/engine/manager.go`)
- [x] Capture error details in metadata (`types/metadata.go` LastError field)
- [x] Save serial log excerpts on error (`vm/engine/manager.go`)

### 10.3 Phase 3: Idempotency (P1)

**Tasks**:
- [x] Add idempotency checks to `start` (RUNNING → no-op) (`vm/engine/manager.go`)
- [x] Add idempotency checks to `stop` (STOPPED → no-op) (`vm/engine/manager.go`)
- [x] Add idempotency checks to `delete` (non-existent → no-op) (`cmd/cocoon/rm.go`)

### 10.4 Phase 4: Reconciliation (P1)

**Tasks**:
- [x] Implement `(m *manager) Reconcile(ctx, fix, force)` scanner (`vm/engine/reconcile.go`)
- [x] Implement `(m *manager) determineActualState()` logic (`vm/engine/reconcile.go`)
- [x] Implement `(m *manager) detectZombieResources()` for stale PIDs/sockets (`vm/engine/reconcile.go`)
- [x] Implement `(m *manager) applyFix()` with --force flag (`vm/engine/reconcile.go`)
- [x] Implement `detectOrphanedProcesses()` for cloud-hypervisor and swtpm (`vm/engine/reconcile.go`)
- [x] Implement `detectDanglingReferenceIssues()` and `detectNameIndexIssues()` (`vm/engine/reconcile.go`)
- [x] Add dry-run mode (default) (`cmd/cocoon/doctor.go`)

### 10.5 Testing Checklist

> **Note**: All state machine tests have passing implementations.
> Items in the "Future Validation" section track additional test coverage beyond the core tests.

**State Machine Tests**:
- [x] Valid transitions succeed (`types/state_test.go`)
- [x] Invalid transitions error (`types/state_test.go`)
- [x] Error transitions from any state (`types/state_test.go`)
- [x] DELETED is terminal (`types/state_test.go`)

### Future Work (Phase 2)

> The items below are planned for future phases and are not yet implemented.

- **Startup reconciliation**: Add reconciliation on startup (daemon mode not yet implemented; currently runs on-demand via `cocoon doctor`).
- **Additional test coverage**: Idempotency tests (start/stop/delete no-ops), concurrency tests (lock contention, atomic rename), reconciliation tests (zombie detection, orphaned processes), error handling tests (boot timeout, kernel panic, CH crash recovery).

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

# ERROR → STOPPED (force kill zombie process, enables restart)
cocoon kill vm-abc123

# STOPPED → STARTING → RUNNING (restart after kill from ERROR)
cocoon start vm-abc123

# ERROR → DELETED
cocoon delete vm-abc123
```

### A.2 State Inspection

```bash
# View current state
cocoon inspect vm-abc123

# List all VMs with states
cocoon ps -a
```

### A.3 Reconciliation

```bash
# Dry-run (report only)
cocoon doctor

# Fix inconsistencies
cocoon doctor --fix

# Force cleanup stuck VMs
cocoon doctor --fix --force
```

---

**End of VM Lifecycle Management v1.0**

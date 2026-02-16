# VM Pause and Resume

**Version**: 1.0
**Status**: Planned
**Phase**: Phase 2
**Last Updated**: 2026-02-14

## Executive Summary

This document specifies the design for VM pause and resume in Cocoon. The `cocoon pause` command freezes all vCPUs of a running VM while keeping the Cloud Hypervisor process alive and the API socket responsive. The `cocoon resume` command unfreezes vCPUs and returns the VM to full operation. Pause/resume is a foundational primitive that enables consistent checkpointing (see [15-warm-start.md](./15-warm-start.md)) and provides direct utility for resource management and debugging.

## Table of Contents

1. [Overview](#1-overview)
2. [State Machine Extension](#2-state-machine-extension)
3. [Design](#3-design)
4. [Implementation](#4-implementation)
5. [CLI](#5-cli)
6. [Error Handling](#6-error-handling)
7. [Reconciliation](#7-reconciliation)
8. [Testing](#8-testing)
9. [Cross-References](#9-cross-references)

---

## 1. Overview

### 1.1 Problem Statement

Cocoon Phase 1 supports only two "active" VM states: RUNNING (vCPUs executing) and STOPPED (CH process dead). There is no intermediate state where the VM is frozen but the hypervisor process remains alive. This gap prevents:

1. **Consistent checkpointing**: Taking a snapshot of VM state requires vCPUs to be frozen so that memory, device state, and disk are consistent at a single point in time. Without pause, a snapshot would capture an inconsistent state.
2. **Resource conservation**: A paused VM consumes no CPU cycles but retains its memory and disk state. This is useful when temporarily parking VMs during resource contention.
3. **Debugging**: Freezing a VM at a specific point allows inspection of guest state (memory layout via CH API, disk contents) without the guest continuing to execute.

### 1.2 Approach

Pause and resume use Cloud Hypervisor's native `vm.pause` and `vm.resume` REST API endpoints. These endpoints freeze and unfreeze all vCPUs at the hypervisor level. The CH process remains alive, the API socket remains responsive, and the VM's memory and disk state are fully preserved.

```
RUNNING ----[cocoon pause]----> PAUSED ----[cocoon resume]----> RUNNING
              PUT /api/v1/vm.pause         PUT /api/v1/vm.resume
```

### 1.3 Minimum Cloud Hypervisor Version

**Minimum Cloud Hypervisor Version**: TBD. The required CH version for pause/resume endpoints (`vm.pause` and `vm.resume`) will be validated by `cocoon doctor` (see [docs/08-dependencies.md](./08-dependencies.md)). These APIs have been available since early CH releases, but the exact minimum version that provides stable pause/resume behavior will be confirmed through integration testing at the start of Phase 2 development.

---

## 2. State Machine Extension

### 2.1 New State: PAUSED

The PAUSED state represents a VM whose vCPUs are frozen but whose CH process is still alive and the API socket is responsive.

```go
const (
    // Existing states (unchanged)
    VMStateCreating  VMState = "CREATING"
    VMStateCreated   VMState = "CREATED"
    VMStateStarting  VMState = "STARTING"
    VMStateRunning   VMState = "RUNNING"
    VMStateStopping  VMState = "STOPPING"
    VMStateStopped   VMState = "STOPPED"
    VMStateError     VMState = "ERROR"
    VMStateDeleted   VMState = "DELETED"

    // New state
    VMStatePaused VMState = "PAUSED"
)
```

### 2.2 PAUSED State Properties

| Property | Value |
|----------|-------|
| CH process alive | Yes |
| API socket responsive | Yes |
| vCPUs executing | No (frozen) |
| Disk I/O possible | No (guest frozen) |
| Serial log active | No (guest frozen, no new output) |
| Memory preserved | Yes |
| Overlay disk preserved | Yes |
| `IsRunnable()` | Yes |

### 2.3 Extended Transition Table

```go
var ValidTransitions = map[VMState][]VMState{
    VMStateCreating:  {VMStateCreated, VMStateError},
    VMStateCreated:   {VMStateStarting, VMStateDeleted},
    VMStateStarting:  {VMStateRunning, VMStateError},
    VMStateRunning:   {VMStateStopping, VMStatePaused, VMStateError},
    VMStatePaused:    {VMStateRunning, VMStateStopping, VMStateError},
    VMStateStopping:  {VMStateStopped, VMStateError},
    VMStateStopped:   {VMStateStarting, VMStateDeleted},
    VMStateError:     {VMStateStopped, VMStateDeleted},
    VMStateDeleted:   {},
}
```

**New transitions**:
- `RUNNING -> PAUSED`: `cocoon pause` freezes vCPUs via `PUT /api/v1/vm.pause`
- `PAUSED -> RUNNING`: `cocoon resume` resumes vCPUs via `PUT /api/v1/vm.resume`
- `PAUSED -> STOPPING`: `cocoon stop` on a paused VM (resume first, then stop gracefully)
- `PAUSED -> ERROR`: CH crash while paused

Note: There is no direct `PAUSED -> DELETED` transition. To delete a paused VM, first stop it (`PAUSED -> STOPPING -> STOPPED`), then delete (`STOPPED -> DELETED`). Kill on a paused VM force-kills the CH process: `PAUSED -> STOPPED` on success, `PAUSED -> ERROR` on failure (no STOPPING intermediate state).

### 2.4 State Machine Diagram

```
                create              start              pause
  CREATING -------> CREATED -------> STARTING -------> RUNNING -------> PAUSED
     |                |                 |                 |                |
     v                v                 v                 v                v
   ERROR            ERROR             ERROR             ERROR           ERROR
     |                |                                   v                v
     v                v                                STOPPING         STOPPING
   DELETED          DELETED                               |                |
                      ^                                   v                |
                      |                                STOPPED             |
                      +--- start -> STARTING              |                |
                      +--- delete                         |                |
                                                                           |
                                                   resume                  |
                                      RUNNING <------------ PAUSED --------+
```

### 2.5 Updated IsRunnable

The `IsRunnable()` helper indicates whether a CH process should be alive for this state. PAUSED is runnable because the CH process is still running:

```go
func (s VMState) IsRunnable() bool {
    return s == VMStateStarting || s == VMStateRunning ||
           s == VMStateStopping || s == VMStatePaused
}
```

### 2.6 Updated Operation Permission Matrix

| State    | create | start | stop | kill | delete | inspect | pause | resume | console | logs |
|----------|--------|-------|------|------|--------|---------|-------|--------|---------|------|
| CREATING | --     | --    | --   | --   | --     | yes     | --    | --     | --      | --   |
| CREATED  | --     | yes   | --   | --   | yes    | yes     | --    | --     | --      | --   |
| STARTING | --     | --    | --   | yes  | --     | yes     | --    | --     | --      | yes  |
| RUNNING  | --     | --    | yes  | yes  | yes*   | yes     | yes   | --     | yes     | yes  |
| PAUSED   | --     | --    | yes  | yes  | yes*   | yes     | --    | yes    | yes**   | yes  |
| STOPPING | --     | --    | --   | yes  | --     | yes     | --    | --     | --      | yes  |
| STOPPED  | --     | yes   | --   | --   | yes    | yes     | --    | --     | --      | yes  |
| ERROR    | --     | --    | --   | --   | yes    | yes     | --    | --     | --      | yes  |

`*` = `--force` for direct kill path; without `--force`, auto-resumes then performs graceful shutdown before deletion
`**` = PTY remains open but no guest output arrives while paused; input is buffered and delivered on resume

**Note**: When Phase 2 is implemented, [docs/07-vm-lifecycle.md](./07-vm-lifecycle.md) §3.1 must be updated to include the PAUSED row shown above.

---

## 3. Design

### 3.1 Pause Flow

```
cocoon pause myvm
    |
    v
[1] ResolveVMRef("myvm") -> "vm-01HX..."
    |
    v
[2] LoadMetadata("vm-01HX...") -> verify state is RUNNING
    |
    v
[3] LoadConfig("vm-01HX...") -> get socket path
    |
    v
[4] PUT /api/v1/vm.pause via CH API socket
    |
    v
[5] TransitionState(vmID, PAUSED, "user: cocoon pause")
    |
    v
[6] Update metadata: PausedAt = now()
    |
    v
[7] Print: "VM myvm paused"
```

### 3.2 Resume Flow

```
cocoon resume myvm
    |
    v
[1] ResolveVMRef("myvm") -> "vm-01HX..."
    |
    v
[2] LoadMetadata("vm-01HX...") -> verify state is PAUSED
    |
    v
[3] LoadConfig("vm-01HX...") -> get socket path
    |
    v
[4] PUT /api/v1/vm.resume via CH API socket
    |
    v
[5] TransitionState(vmID, RUNNING, "user: cocoon resume")
    |
    v
[6] Clear metadata: PausedAt = ""
    |
    v
[7] Print: "VM myvm resumed"
```

### 3.3 Stop on Paused VM (PAUSED -> STOPPING Semantics)

When `cocoon stop` is called on a PAUSED VM, Cocoon **must** resume the VM before sending the ACPI shutdown signal. Cloud Hypervisor cannot deliver ACPI events while vCPUs are frozen -- the guest kernel never receives the power button press, so the shutdown sequence never begins. The full sequence is:

```
cocoon stop myvm  (state: PAUSED)
    |
    v
[1] Detect VM is PAUSED
    |
    v
[2] PUT /api/v1/vm.resume  (unfreeze vCPUs so guest can process ACPI)
    |       |
    |       +-- If resume fails: TransitionState -> ERROR, return error
    |           (CH may have crashed while paused; cannot proceed with graceful shutdown)
    v
[3] TransitionState(vmID, RUNNING, "stop: auto-resume for shutdown")
    |
    v
[4] TransitionState(vmID, STOPPING, "user: cocoon stop")
    |
    v
[5] Send ACPI shutdown (PUT /api/v1/vm.power-button)
    |
    v
[6] Wait for CH process to exit (up to --timeout, default 30s)
    |       |
    |       +-- If timeout: ForceKill() is called; if it succeeds -> STOPPED, if it fails -> ERROR
    v
[7] CH process exited -> TransitionState(vmID, STOPPED, "graceful shutdown complete")
```

**Key semantics**:

- **Resume is mandatory**: Cocoon MUST resume the VM before sending ACPI shutdown. CH cannot process ACPI power button events while vCPUs are frozen. Attempting to send `vm.power-button` to a paused VM has no effect -- the guest never processes the interrupt.
- **Resume failure -> ERROR**: If `PUT /api/v1/vm.resume` fails (e.g., the CH process crashed while the VM was paused, or the API socket is unresponsive), the VM transitions to ERROR. Cocoon does not attempt further shutdown steps because the guest is unreachable. The user can then use `cocoon kill` or `cocoon delete --force` to clean up.
- **Timeout -> ForceKill -> STOPPED or ERROR**: If the guest does not shut down within the timeout, the `Shutdown()` implementation in `client.go` calls `ForceKill()`, sending SIGKILL to the CH process. If `ForceKill()` succeeds (process terminated), `Stop()` transitions the VM to STOPPED. If `ForceKill()` itself fails (e.g., PID reuse, permission error), `Stop()` transitions the VM to ERROR. This matches the behavior documented in [docs/07-vm-lifecycle.md](./07-vm-lifecycle.md).
- **Sequence is atomic from the caller's perspective**: The resume-then-stop sequence is a single `Stop()` call. The intermediate RUNNING state is visible in metadata briefly but is not a user-initiated transition.

### 3.4 Kill on Paused VM

`cocoon kill` on a PAUSED VM sends SIGKILL directly to the CH process **without resuming first**. No resume is needed because SIGKILL operates at the host process level, not the guest vCPU level. The frozen vCPUs are terminated immediately along with the CH process. Transition: `PAUSED -> STOPPED` if the force-kill succeeds, `PAUSED -> ERROR` if the force-kill fails (e.g., PID reuse, permission error).

This is the fastest way to terminate a paused VM but skips any graceful guest shutdown (filesystem sync, service cleanup, etc.).

### 3.5 Source of Truth for Paused State

**Metadata.json is the authoritative source** for whether a VM is in the PAUSED state. The `state` field in `metadata.json` is updated by Cocoon after each successful CH API call and state transition.

- **Primary source**: `metadata.json` `state` field. All Cocoon commands check this field to determine the current VM state before performing operations.
- **Verification source**: CH `GET /api/v1/vm.info` returns a `state` field (e.g., `"Paused"`, `"Running"`). This is used by reconciliation (`cocoon doctor`) to verify that the metadata state matches the actual hypervisor state.
- **Reconciliation rules for paused state**:
  - If `metadata.json` says PAUSED and the CH process is alive and `vm.info` reports `"Paused"`: state is consistent, no action needed.
  - If `metadata.json` says PAUSED but the CH process is gone (PID not running): reconcile to ERROR with reason "process died while paused". The VM cannot be resumed and must be cleaned up.
  - If `metadata.json` says PAUSED but `vm.info` reports `"Running"`: metadata is stale. Reconciliation updates metadata to RUNNING and logs a warning. This can happen if a resume API call succeeded but the metadata write failed.
  - If `metadata.json` says RUNNING but `vm.info` reports `"Paused"`: metadata is stale. Reconciliation updates metadata to PAUSED and logs a warning. This can happen if a pause API call succeeded but the metadata write failed.

### 3.6 Delete on Paused VM

Delete is permitted on a paused VM with or without `--force`:

- **With `--force`**: Sends SIGKILL to the CH process, then proceeds to deletion. Transition: PAUSED → STOPPED → DELETED.
- **Without `--force`**: Auto-resumes the VM (`PUT /api/v1/vm.resume`), then performs graceful ACPI shutdown, then deletes. Transition: PAUSED → RUNNING → STOPPING → STOPPED → DELETED.

Both paths end in deletion. The `--force` path is faster but skips graceful guest shutdown.

### 3.7 Idempotency Rules

| Operation | Current State | Behavior |
|-----------|--------------|----------|
| `pause` | RUNNING | Pause vCPUs, transition to PAUSED |
| `pause` | PAUSED | No-op, return success |
| `pause` | Other | Error: "VM is not running" |
| `resume` | PAUSED | Resume vCPUs, transition to RUNNING |
| `resume` | RUNNING | No-op, return success |
| `resume` | Other | Error: "VM is not paused" |

### 3.8 Lock Integration

Pause and resume follow the lock ordering defined in [06-concurrency.md](./06-concurrency.md). The VM metadata lock (Level 4, per-VM) is held only during metadata reads and writes, never during CH API calls. This prevents long-running HTTP requests from blocking other operations on the same VM.

**Lock Ordering for Pause:**

1. Acquire VM metadata lock (Level 4, per [06-concurrency.md](./06-concurrency.md))
2. Load metadata, verify state is RUNNING
3. Load config (socket path)
4. Release metadata lock
5. Call `PUT /api/v1/vm.pause` (no lock held during CH API call)
6. Re-acquire metadata lock
7. Transition state RUNNING -> PAUSED, set `PausedAt`
8. Release metadata lock

**Lock Ordering for Resume:**

1. Acquire VM metadata lock (Level 4)
2. Load metadata, verify state is PAUSED
3. Load config (socket path)
4. Release metadata lock
5. Call `PUT /api/v1/vm.resume` (no lock held during CH API call)
6. Re-acquire metadata lock
7. Transition state PAUSED -> RUNNING, clear `PausedAt`
8. Release metadata lock

**Concurrent Stop on PAUSED VM:**

When `cocoon stop` targets a PAUSED VM, the stop flow acquires the VM metadata lock, detects the PAUSED state, and releases the lock before calling `PUT /api/v1/vm.resume`. After the resume API call completes, it re-acquires the lock, transitions to RUNNING (with reason "stop: auto-resume for shutdown"), releases the lock, and proceeds with the normal stop flow. Both pause/resume and stop acquire the same per-VM metadata lock sequentially, so concurrent `cocoon pause` and `cocoon stop` on the same VM are serialized correctly.

---

## 4. Implementation

### 4.1 Hypervisor Client Extensions

Add pause and resume methods to the `hypervisor.Client` interface:

```go
// In hypervisor/hypervisor.go (additions to Client interface)

type Client interface {
    // ... existing methods ...

    // PauseVM sends PUT /api/v1/vm.pause to freeze all vCPUs.
    // The CH process remains alive and the API socket remains responsive.
    PauseVM(ctx context.Context, socketPath string) error

    // ResumeVM sends PUT /api/v1/vm.resume to unfreeze all vCPUs.
    ResumeVM(ctx context.Context, socketPath string) error
}
```

Implementation in `hypervisor/cloudhypervisor/client.go`:

```go
func (c *client) PauseVM(ctx context.Context, socketPath string) error {
    url := fmt.Sprintf("http://localhost/api/v1/vm.pause")
    req, err := http.NewRequestWithContext(ctx, "PUT", url, nil)
    if err != nil {
        return fmt.Errorf("create pause request: %w", err)
    }

    httpClient := c.httpClientForSocket(socketPath)
    resp, err := httpClient.Do(req)
    if err != nil {
        return fmt.Errorf("pause VM: %w", err)
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusNoContent {
        body, _ := io.ReadAll(resp.Body)
        return fmt.Errorf("pause VM: CH returned %d: %s", resp.StatusCode, string(body))
    }

    return nil
}

func (c *client) ResumeVM(ctx context.Context, socketPath string) error {
    url := fmt.Sprintf("http://localhost/api/v1/vm.resume")
    req, err := http.NewRequestWithContext(ctx, "PUT", url, nil)
    if err != nil {
        return fmt.Errorf("create resume request: %w", err)
    }

    httpClient := c.httpClientForSocket(socketPath)
    resp, err := httpClient.Do(req)
    if err != nil {
        return fmt.Errorf("resume VM: %w", err)
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusNoContent {
        body, _ := io.ReadAll(resp.Body)
        return fmt.Errorf("resume VM: CH returned %d: %s", resp.StatusCode, string(body))
    }

    return nil
}
```

### 4.2 VM Manager Extensions

Add `Pause()` and `Resume()` methods to the `vm.Manager` interface:

```go
// In vm/vm.go (additions to Manager interface)

type Manager interface {
    // ... existing methods ...

    // Pause freezes all vCPUs of a running VM.
    // Transitions RUNNING -> PAUSED.
    // Idempotent: pausing a PAUSED VM is a no-op.
    Pause(ctx context.Context, vmID string) error

    // Resume unfreezes all vCPUs of a paused VM.
    // Transitions PAUSED -> RUNNING.
    // Idempotent: resuming a RUNNING VM is a no-op.
    Resume(ctx context.Context, vmID string) error
}
```

Implementation in `vm/engine/manager.go`:

```go
func (m *Manager) Pause(ctx context.Context, vmID string) error {
    meta, err := m.LoadMetadata(vmID)
    if err != nil {
        return fmt.Errorf("load metadata: %w", err)
    }

    // Idempotent: already paused -> no-op.
    if types.VMState(meta.State) == types.VMStatePaused {
        return nil
    }

    // Must be RUNNING to pause.
    if types.VMState(meta.State) != types.VMStateRunning {
        return fmt.Errorf("cannot pause VM in state %s (must be RUNNING)", meta.State)
    }

    // Load config for socket path.
    cfg, err := m.LoadConfig(vmID)
    if err != nil {
        return fmt.Errorf("load config: %w", err)
    }

    // Send pause command to CH.
    if err := m.hyper.PauseVM(ctx, cfg.SocketPath); err != nil {
        return fmt.Errorf("pause VM via CH API: %w", err)
    }

    // Transition state.
    if err := m.TransitionState(vmID, types.VMStatePaused, "user: pause"); err != nil {
        // Best-effort resume on state transition failure.
        _ = m.hyper.ResumeVM(ctx, cfg.SocketPath)
        return fmt.Errorf("transition state: %w", err)
    }

    // Update metadata with pause timestamp.
    meta, _ = m.LoadMetadata(vmID) // Reload after transition.
    meta.PausedAt = time.Now().UTC().Format(time.RFC3339)
    return m.SaveMetadata(meta)
}

func (m *Manager) Resume(ctx context.Context, vmID string) error {
    meta, err := m.LoadMetadata(vmID)
    if err != nil {
        return fmt.Errorf("load metadata: %w", err)
    }

    // Idempotent: already running -> no-op.
    if types.VMState(meta.State) == types.VMStateRunning {
        return nil
    }

    // Must be PAUSED to resume.
    if types.VMState(meta.State) != types.VMStatePaused {
        return fmt.Errorf("cannot resume VM in state %s (must be PAUSED)", meta.State)
    }

    // Load config for socket path.
    cfg, err := m.LoadConfig(vmID)
    if err != nil {
        return fmt.Errorf("load config: %w", err)
    }

    // Send resume command to CH.
    if err := m.hyper.ResumeVM(ctx, cfg.SocketPath); err != nil {
        return fmt.Errorf("resume VM via CH API: %w", err)
    }

    // Transition state.
    if err := m.TransitionState(vmID, types.VMStateRunning, "user: resume"); err != nil {
        return fmt.Errorf("transition state: %w", err)
    }

    // Clear pause timestamp.
    meta, _ = m.LoadMetadata(vmID)
    meta.PausedAt = ""
    return m.SaveMetadata(meta)
}
```

### 4.3 Metadata Extension

The `VMMetadataFile` gains a `PausedAt` field:

```go
// In types/metadata.go (addition)

type VMMetadataFile struct {
    // ... existing fields ...

    // PausedAt records when the VM was paused. Empty if not paused.
    PausedAt string `json:"paused_at,omitempty"` // RFC 3339
}
```

### 4.4 State Validation Update

Update `types/state.go` to include the new state and transitions:

```go
// In types/state.go

const VMStatePaused VMState = "PAUSED"

// Update ValidTransitions to include PAUSED transitions.
var ValidTransitions = map[VMState][]VMState{
    VMStateCreating:  {VMStateCreated, VMStateError},
    VMStateCreated:   {VMStateStarting, VMStateDeleted},
    VMStateStarting:  {VMStateRunning, VMStateError},
    VMStateRunning:   {VMStateStopping, VMStatePaused, VMStateError},
    VMStatePaused:    {VMStateRunning, VMStateStopping, VMStateError},
    VMStateStopping:  {VMStateStopped, VMStateError},
    VMStateStopped:   {VMStateStarting, VMStateDeleted},
    VMStateError:     {VMStateStopped, VMStateDeleted},
    VMStateDeleted:   {},
}
```

### 4.5 Stop Command Update

Update the stop implementation to handle PAUSED VMs. See Section 3.3 for the full PAUSED -> STOPPING semantics including resume failure and timeout behavior:

```go
func (m *Manager) Stop(ctx context.Context, vmID string, timeout time.Duration) error {
    meta, err := m.LoadMetadata(vmID)
    if err != nil {
        return err
    }

    // Idempotent: already stopped -> no-op.
    if types.VMState(meta.State) == types.VMStateStopped {
        return nil
    }

    // If PAUSED, resume first so the guest can process ACPI shutdown.
    // CH cannot deliver ACPI events while vCPUs are frozen.
    if types.VMState(meta.State) == types.VMStatePaused {
        cfg, err := m.LoadConfig(vmID)
        if err != nil {
            return err
        }
        if err := m.hyper.ResumeVM(ctx, cfg.SocketPath); err != nil {
            // Resume failed: CH may have crashed while paused.
            // Transition to ERROR -- cannot proceed with graceful shutdown.
            _ = m.TransitionState(vmID, types.VMStateError,
                fmt.Sprintf("resume before stop failed: %v", err))
            return fmt.Errorf("resume before stop: %w (VM transitioned to ERROR)", err)
        }
        if err := m.TransitionState(vmID, types.VMStateRunning, "stop: auto-resume for shutdown"); err != nil {
            return err
        }
    }

    // Proceed with normal stop flow (ACPI shutdown, wait for process exit).
    // If the guest does not shut down within the timeout, Shutdown() calls
    // ForceKill(). If ForceKill succeeds -> STOPPED; if it fails -> ERROR.
    // Same as stopping from RUNNING (see §3.3 timeout behavior).
    // ... existing stop logic ...
}
```

### 4.6 VMInspect Extension

Add pause information to the inspect output:

```go
// In types/inspect.go (addition)

type InspectCheckpointInfo struct {
    RestoredFrom   string `json:"restored_from,omitempty"`
    LastCheckpoint string `json:"last_checkpoint,omitempty"`
    PausedAt       string `json:"paused_at,omitempty"`
}
```

### 4.7 Project Structure Additions

```
cocoon/
├── cmd/cocoon/
│   ├── pause.go               # cocoon pause command
│   └── resume.go              # cocoon resume command
```

---

## 5. CLI

### 5.1 Pause Command

```go
func pauseCommand() *cli.Command {
    return &cli.Command{
        Name:      "pause",
        Usage:     "Freeze all vCPUs of a running VM",
        ArgsUsage: "VM_REF",
        Action:    pauseAction,
    }
}

func pauseAction(c *cli.Context) error {
    if c.NArg() < 1 {
        return fmt.Errorf("VM_REF argument required\n\nUsage: cocoon pause VM_REF")
    }

    app, err := initApp(c)
    if err != nil {
        return err
    }

    ref := c.Args().Get(0)
    vmID, err := app.vmMgr.ResolveVMRef(ref)
    if err != nil {
        return err
    }

    if err := app.vmMgr.Pause(c.Context, vmID); err != nil {
        return fmt.Errorf("pause %s: %w", ref, err)
    }

    fmt.Printf("VM %s paused\n", ref)
    return nil
}
```

**Edge Case Handling:**

The CLI layer translates manager-level errors into user-friendly messages. The manager's idempotency rules (§3.6) handle the no-op cases, and the CLI adds explicit messaging:

| Command | VM State | Behavior |
|---------|----------|----------|
| `cocoon pause` | CREATED / STOPPED | Error: `VM is not running (state: CREATED). Start it first with 'cocoon start'` |
| `cocoon pause` | PAUSED | No-op, print: `VM already paused` |
| `cocoon resume` | RUNNING | No-op, print: `VM is already running` |
| `cocoon resume` | CREATED / STOPPED | Error: `VM is not paused (state: STOPPED)` |
| `cocoon pause` | STARTING | Error: `VM is not in RUNNING state (current: STARTING). Wait for boot to complete before pausing.` |

The CLI detects no-op returns from the manager (pause on PAUSED, resume on RUNNING) and prints the informational message instead of the standard success output. For invalid states, the manager returns an error that the CLI formats with remediation guidance.

**Implementation note**: The manager's `Pause()` and `Resume()` methods handle idempotency internally and return nil for both success and no-op. The CLI detects no-ops by comparing state before and after the call. A future optimization could have the manager return a result type indicating (success, no-op) to avoid the double read.

CLI code:

```go
// In pauseAction, after Pause() returns:
func pauseAction(c *cli.Context) error {
    // ... resolve vmID ...

    err := app.vmMgr.Pause(c.Context, vmID)
    if err != nil {
        return fmt.Errorf("pause %s: %w", ref, err)
    }

    // Check if it was a no-op (already paused).
    meta, _ := app.vmMgr.LoadMetadata(vmID)
    // The manager returns nil for idempotent pause; we detect it
    // by checking if PausedAt was already set before our call.
    fmt.Printf("VM %s paused\n", ref)
    return nil
}
```

### 5.2 Resume Command

```go
func resumeCommand() *cli.Command {
    return &cli.Command{
        Name:      "resume",
        Usage:     "Unfreeze all vCPUs of a paused VM",
        ArgsUsage: "VM_REF",
        Action:    resumeAction,
    }
}

func resumeAction(c *cli.Context) error {
    if c.NArg() < 1 {
        return fmt.Errorf("VM_REF argument required\n\nUsage: cocoon resume VM_REF")
    }

    app, err := initApp(c)
    if err != nil {
        return err
    }

    ref := c.Args().Get(0)
    vmID, err := app.vmMgr.ResolveVMRef(ref)
    if err != nil {
        return err
    }

    if err := app.vmMgr.Resume(c.Context, vmID); err != nil {
        return fmt.Errorf("resume %s: %w", ref, err)
    }

    fmt.Printf("VM %s resumed\n", ref)
    return nil
}
```

### 5.3 Command Registration

In `cmd/cocoon/main.go`:

```go
app.Commands = []*cli.Command{
    // ... existing commands ...
    pauseCommand(),
    resumeCommand(),
}
```

### 5.4 Usage Examples

```bash
# Pause a running VM
$ cocoon pause myvm
VM myvm paused

# Check VM state
$ cocoon inspect myvm | jq .state
"PAUSED"

# Resume the VM
$ cocoon resume myvm
VM myvm resumed

# Stop a paused VM (auto-resumes, then graceful shutdown)
$ cocoon stop myvm
VM myvm resumed (auto-resume for shutdown)
VM myvm stopped

# Force kill a paused VM (no resume needed)
$ cocoon kill myvm

# Idempotent: pause already-paused VM
$ cocoon pause myvm
VM myvm paused   # no-op, already paused

# Idempotent: resume already-running VM
$ cocoon resume myvm
VM myvm resumed  # no-op, already running
```

---

## 6. Error Handling

### 6.1 Error Cases

| Condition | Error Message | Exit Code |
|-----------|--------------|-----------|
| VM not found | `VM not found: <ref>` | 1 |
| Pause non-running VM | `cannot pause VM in state <state> (must be RUNNING)` | 1 |
| Resume non-paused VM | `cannot resume VM in state <state> (must be PAUSED)` | 1 |
| CH API failure on pause | `pause VM via CH API: <err>` | 1 |
| CH API failure on resume | `resume VM via CH API: <err>` | 1 |
| CH process died while paused | Detected by reconciliation -> ERROR state | -- |

### 6.2 Failure Recovery

If the CH `vm.pause` call succeeds but the subsequent state transition fails (e.g., metadata write error), the implementation attempts a best-effort resume to avoid leaving the VM in a paused state that the metadata does not reflect:

```go
if err := m.hyper.PauseVM(ctx, cfg.SocketPath); err != nil {
    return fmt.Errorf("pause VM via CH API: %w", err)
}

if err := m.TransitionState(vmID, types.VMStatePaused, "user: pause"); err != nil {
    // Best-effort resume on state transition failure.
    _ = m.hyper.ResumeVM(ctx, cfg.SocketPath)
    return fmt.Errorf("transition state: %w", err)
}
```

---

## 7. Reconciliation

### 7.1 PAUSED State in Reconciliation

The `cocoon doctor` reconciliation flow is extended to handle the PAUSED state. The key difference from RUNNING is that a paused VM has frozen vCPUs but the CH process is still alive.

```go
// In vm/engine/reconcile.go (extension to DetermineActualState)

case VMStatePaused:
    if processValid && socketConnectable {
        // Verify VM is actually paused via CH API.
        info, err := getVMInfo(cfg.SocketPath)
        if err != nil {
            return VMStateError // Cannot query CH, inconsistent
        }
        if info.State == "Paused" {
            return VMStatePaused // Genuinely paused
        }
        // Metadata says PAUSED but CH says otherwise.
        return VMStateInconsistent
    } else if !processValid {
        return VMStateError // Process dead while paused -> crashed
    } else {
        return VMStateError // Process alive but socket dead
    }
```

### 7.2 Crash While Paused

If the CH process dies while the VM is in PAUSED state, reconciliation detects this through the standard PID liveness check and transitions the VM to ERROR:

```
Before crash: metadata.json -> PAUSED, PID=1234
Crash:        kill -9 1234
After crash:  metadata.json -> PAUSED, PID=1234 (dead)

Reconciliation:
1. Read metadata.json -> state=PAUSED, PID=1234
2. Check process 1234 -> not running
3. Action: Update metadata.json -> state=ERROR, error="process killed while paused"
```

### 7.3 Stuck in PAUSED

If a VM has been in PAUSED state for an unusually long time (configurable threshold, default: no limit), reconciliation reports it as informational but does not automatically resume or transition it. Paused VMs are a legitimate operational state and should not be force-resumed.

### 7.4 Data Source Priority

Reconciliation uses a strict priority order when determining the actual state of a VM. This is especially important for PAUSED VMs, where the CH process is alive but vCPUs are frozen:

1. **PID liveness check** (`kill -0 <pid>`): Checked first. If the process is dead, the VM is in ERROR state regardless of what metadata or the socket reports. A dead process is the strongest signal.

2. **Socket connectivity**: Checked second. If the PID is alive but the API socket is unreachable, preserve the current metadata state and emit a warning. The socket may be temporarily unavailable (e.g., CH is initializing or under load). Do not transition to ERROR based on a transient socket failure when the process is confirmed alive.

3. **CH API state** (`GET /api/v1/vm.info`): Checked third. If the socket is reachable, the CH-reported state is used as ground truth. For PAUSED VMs, the response should report `"state": "Paused"`. If CH reports a different state than metadata expects (e.g., metadata says PAUSED but CH says Running), reconciliation treats this as an inconsistency and logs a warning with corrective action.

```
Priority 1: kill -0 <pid>   -> Dead?  -> ERROR (definitive)
Priority 2: socket connect  -> Fail?  -> Preserve metadata state + warn
Priority 3: vm.info state   -> Mismatch? -> Use CH state as ground truth
```

For PAUSED VMs, reconciliation checks the same signals as RUNNING (PID alive, socket connectable). If the process has crashed while paused, the VM transitions to ERROR.

---

## 8. Testing

### 8.1 State Machine Tests

```go
func TestPausedTransitions(t *testing.T) {
    tests := []struct {
        from    types.VMState
        to      types.VMState
        wantErr bool
    }{
        // Valid transitions
        {types.VMStateRunning, types.VMStatePaused, false},
        {types.VMStatePaused, types.VMStateRunning, false},
        {types.VMStatePaused, types.VMStateStopping, false},
        {types.VMStatePaused, types.VMStateError, false},

        // Invalid transitions
        {types.VMStatePaused, types.VMStateCreated, true},
        {types.VMStatePaused, types.VMStateStarting, true},
        {types.VMStatePaused, types.VMStateDeleted, true},
        {types.VMStateCreated, types.VMStatePaused, true},
        {types.VMStateStopped, types.VMStatePaused, true},
    }

    for _, tt := range tests {
        err := types.ValidateTransition(tt.from, tt.to)
        if (err != nil) != tt.wantErr {
            t.Errorf("ValidateTransition(%s, %s) error = %v, wantErr %v",
                tt.from, tt.to, err, tt.wantErr)
        }
    }
}
```

### 8.2 Idempotency Tests

```go
func TestPauseIdempotency(t *testing.T) {
    mgr := newTestManager(t)
    vmID := createAndStartTestVM(t, mgr)

    // First pause: should succeed.
    err := mgr.Pause(context.Background(), vmID)
    if err != nil {
        t.Fatalf("first pause failed: %v", err)
    }

    // Second pause: should be no-op.
    err = mgr.Pause(context.Background(), vmID)
    if err != nil {
        t.Fatalf("idempotent pause failed: %v", err)
    }

    // Verify state is still PAUSED.
    meta, _ := mgr.LoadMetadata(vmID)
    if meta.State != string(types.VMStatePaused) {
        t.Errorf("expected PAUSED, got %s", meta.State)
    }
}

func TestResumeIdempotency(t *testing.T) {
    mgr := newTestManager(t)
    vmID := createAndStartTestVM(t, mgr)

    // Resume already-running VM: should be no-op.
    err := mgr.Resume(context.Background(), vmID)
    if err != nil {
        t.Fatalf("idempotent resume failed: %v", err)
    }

    // Verify state is still RUNNING.
    meta, _ := mgr.LoadMetadata(vmID)
    if meta.State != string(types.VMStateRunning) {
        t.Errorf("expected RUNNING, got %s", meta.State)
    }
}
```

### 8.3 Integration Tests

```go
func TestPauseResumeLifecycle(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping integration test")
    }

    // 1. Create and start VM
    // 2. Verify RUNNING state
    // 3. Pause VM
    // 4. Verify PAUSED state
    // 5. Verify CH process still alive
    // 6. Verify CH API socket responsive (GET /api/v1/vm.info -> state: "Paused")
    // 7. Resume VM
    // 8. Verify RUNNING state
    // 9. Verify guest continues executing (serial log produces new output)
    // 10. Cleanup
}

func TestStopPausedVM(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping integration test")
    }

    // 1. Create, start, and pause VM
    // 2. Call stop -> should auto-resume then graceful shutdown
    // 3. Verify STOPPED state
    // 4. Verify CH process exited
}

func TestKillPausedVM(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping integration test")
    }

    // 1. Create, start, and pause VM
    // 2. Call kill -> SIGKILL without resume
    // 3. Verify STOPPED state (force-kill succeeded) or ERROR state (force-kill failed)
    // 4. Verify CH process exited
}
```

### 8.4 Reconciliation Tests

```go
func TestReconcilePausedWithDeadProcess(t *testing.T) {
    // 1. Create VM, set metadata state to PAUSED with PID
    // 2. Do NOT start an actual process (simulating crash)
    // 3. Run reconciliation
    // 4. Verify state transitions to ERROR
}

func TestReconcilePausedWithRunningProcess(t *testing.T) {
    // 1. Start VM, pause it
    // 2. Run reconciliation
    // 3. Verify state remains PAUSED (no false positives)
}
```

---

## 9. Cross-References

### 9.1 Related Cocoon Documents

- [07-vm-lifecycle.md](./07-vm-lifecycle.md): Original state machine and transition rules. This document extends the state machine with the PAUSED state.
- [03-hypervisor-integration.md](./03-hypervisor-integration.md): CH process lifecycle, API socket management, and REST API mapping. Pause/resume adds two new API calls.
- [09-cli-design.md](./09-cli-design.md): CLI command structure. Pause and resume are registered alongside existing lifecycle commands.
- [06-concurrency.md](./06-concurrency.md): Lock hierarchy for metadata updates during state transitions.

### 9.2 Interaction with Other Phase 2 Features

- **Console** ([12-console.md](./12-console.md)): Console can be attached to a PAUSED VM. The PTY remains open but no guest output arrives while paused. On resume, output resumes normally.
- **Warm Start** ([15-warm-start.md](./15-warm-start.md)): Pause is a prerequisite for consistent checkpointing. The checkpoint workflow pauses the VM, captures state and disk, then optionally resumes. The `--live` flag on `cocoon checkpoint` automates the pause/checkpoint/resume cycle.
- **Device Passthrough** ([14-device-passthrough.md](./14-device-passthrough.md)): VMs with passthrough devices can be paused and resumed. Device state is preserved because the VFIO binding is maintained. However, passthrough devices prevent checkpointing (a limitation of CH, not of pause/resume).

### 9.3 External References

- Cloud Hypervisor API: https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/vmm/src/api/openapi/cloud-hypervisor.yaml (`vm.pause`, `vm.resume` endpoints)
- Cloud Hypervisor Snapshot Documentation: https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/snapshot_restore.md (pause as prerequisite for snapshot)

---

**End of VM Pause and Resume Design Document v1.0**

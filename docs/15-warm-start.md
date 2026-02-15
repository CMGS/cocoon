# VM Warm Start (Checkpoint and Restore)

**Version**: 1.1
**Status**: Planned
**Phase**: Phase 2
**Last Updated**: 2026-02-15

## Executive Summary

This document specifies the design for VM checkpoint and restore in Cocoon, with a focus on the warm start optimization. Checkpoint captures the complete runtime state of a running VM -- vCPU registers, device state, and memory contents -- to persistent storage. Restore creates a new VM from a checkpoint, resuming execution from the exact point where the checkpoint was taken. The "golden checkpoint" workflow enables sub-second VM creation by skipping the entire boot sequence, providing a 25-150x speedup over cold boot.

Warm-start supports two checkpoint/restore paths depending on the VM's disk backend: **cloud image VMs** use qcow2 overlay snapshots, while **OCI VM image VMs** use overlayfs upperdir preservation. Both paths share the same CH memory/device state snapshot mechanism but differ in how disk state is captured and restored (see §3.7 and §3.8).

This feature builds on the PAUSED state defined in [13-pause-resume.md](./13-pause-resume.md) and uses Cloud Hypervisor's native `vm.snapshot` and `vm.restore` capabilities.

## Table of Contents

1. [Overview](#1-overview)
2. [Motivation](#2-motivation)
3. [Design](#3-design)
4. [Checkpoint Storage](#4-checkpoint-storage)
5. [Configuration Changes](#5-configuration-changes)
6. [Implementation](#6-implementation)
7. [CLI](#7-cli)
8. [Warm Start Workflow](#8-warm-start-workflow)
9. [GC Integration](#9-gc-integration)
10. [Limitations and Constraints](#10-limitations-and-constraints)
11. [Checkpoint Path Comparison](#11-checkpoint-path-comparison)
12. [Error Handling](#12-error-handling)
13. [Security](#13-security)
14. [Testing](#14-testing)
15. [Implementation Plan](#15-implementation-plan)
16. [Unresolved Questions](#16-unresolved-questions)
17. [Cross-References](#17-cross-references)

---

## 1. Overview

### 1.1 Problem Statement

Cold booting a VM involves firmware loading, kernel initialization, systemd startup, and cloud-init execution. This takes 5-30 seconds depending on image complexity. For use cases that create and destroy VMs frequently -- AI agent sandboxes, CI/CD jobs, function-as-a-service -- this boot latency is a significant bottleneck.

### 1.2 Key Distinction

Cloud Hypervisor's `vm.snapshot` saves only the VMM state (vCPU registers, device state, memory contents). It does NOT snapshot the disk. Cocoon must handle disk snapshotting alongside the CH state capture to produce a consistent, restorable checkpoint. The disk snapshot strategy differs by VM type: cloud image VMs copy the qcow2 overlay, while OCI VM image VMs preserve the overlayfs upperdir.

### 1.3 Checkpoint/Restore vs. Pause/Resume

| | Pause/Resume | Checkpoint/Restore |
|---|---|---|
| CH process lifecycle | Same process | New process on restore |
| State persistence | In-memory only | Persisted to disk |
| Survives host reboot | No | Yes |
| Creates new VM | No | Yes (restore creates a new VM) |
| Boot sequence | N/A | Skipped entirely |
| Use case | Temporary freeze | Fast start, cloning, migration |

---

## 2. Motivation

### 2.1 Fast VM Startup (Warm Start)

A checkpoint taken after boot completes can be restored in approximately 200ms, compared to 5-30 seconds for cold boot. This is particularly valuable for AI agent sandboxes where VMs are created and destroyed frequently.

| Operation | Cold Boot | Warm Start (Restore) |
|-----------|-----------|----------------------|
| Firmware load | 0.5-2s | 0ms (skipped) |
| Kernel boot | 2-5s | 0ms (skipped) |
| systemd init | 2-10s | 0ms (skipped) |
| cloud-init | 1-15s | 0ms (skipped) |
| CH process launch | 0.1s | 0.1s |
| State restore | 0ms | 0.05-0.1s |
| Overlay copy | 0ms | 0.01-0.5s (depends on size) |
| **Total** | **5-30s** | **~0.2-0.7s** |

### 2.2 Pre-warmed Environments

For CI/CD and agent sandbox use cases, a checkpoint can be taken after environment setup (package installation, model loading, data staging). Each job then restores from this checkpoint rather than repeating the setup phase, saving minutes per job.

### 2.3 Suspend/Resume Across Host Reboots

Developers and orchestrators can checkpoint a VM's state before a host maintenance window, then restore it afterward -- even after the host reboots.

### 2.4 Debugging and Replay

Checkpoint at an interesting execution point, then restore repeatedly to replay and debug the same scenario.

### 2.5 Future: Live Migration Foundation

Checkpoint on host A, transfer files, restore on host B. While not a goal for Phase 2, the checkpoint/restore primitive is the essential building block.

---

## 3. Design

### 3.1 Checkpoint Flow (End-to-End)

```
cocoon checkpoint my-vm --live --name "after-setup"

  1. ResolveVMRef("my-vm") -> "vm-01HX..."
  2. LoadMetadata("vm-01HX...") -> state=RUNNING
  3. --live flag: auto-pause
     a. PUT /api/v1/vm.pause
     b. TransitionState(vmID, PAUSED, "checkpoint: auto-pause")
  4. Generate checkpoint ID: "ckpt-01HY..."
  5. Create /var/lib/cocoon/checkpoints/ckpt-01HY.../
  6. Copy overlay:
     cp /var/lib/cocoon/vms/vm-01HX.../overlay.qcow2 \
        /var/lib/cocoon/checkpoints/ckpt-01HY.../overlay.qcow2
  7. CH snapshot:
     PUT /api/v1/vm.snapshot
       { "destination_url": "/var/lib/cocoon/checkpoints/ckpt-01HY.../ch-snapshot" }
  8. Write checkpoint.json with provenance and size info
  9. Register checkpoint name in checkpoint-index.json
 10. Pin base image reference for the checkpoint
 11. --live flag: auto-resume
     a. PUT /api/v1/vm.resume
     b. TransitionState(vmID, RUNNING, "checkpoint: auto-resume")
 12. Print: "Checkpoint ckpt-01HY... created (name: after-setup, size: 2.1 GB)"
```

### 3.2 Restore Flow (End-to-End)

```
cocoon restore after-setup --name "job-42"

  1. ResolveCheckpointRef("after-setup") -> "ckpt-01HY..."
  2. Load checkpoint.json from /var/lib/cocoon/checkpoints/ckpt-01HY.../
  3. Validate:
     a. Base image still exists (or can be located by base_key)
     b. Firmware version matches (or warn)
     c. CH version compatible (or warn)
  4. Generate new VM ID: "vm-01HZ..."
  5. Create /var/lib/cocoon/vms/vm-01HZ.../
  6. Copy checkpoint overlay to new VM directory:
     cp /var/lib/cocoon/checkpoints/ckpt-01HY.../overlay.qcow2 \
        /var/lib/cocoon/vms/vm-01HZ.../overlay.qcow2
  7. Write config.json for new VM (using checkpoint metadata for resources)
  8. Write metadata.json in CREATING state
  9. Pin reference: AddReference(base_key, "vm-01HZ...", digest_full, image_ref)
 10. Register name "job-42" in name-index.json
 11. Launch CH with --restore:
     cloud-hypervisor \
       --api-socket /run/cocoon/vms/vm-01HZ.../api.sock \
       --restore source_url=/var/lib/cocoon/checkpoints/ckpt-01HY.../ch-snapshot
 12. Wait for socket
 13. TransitionState("vm-01HZ...", RUNNING, "restored from ckpt-01HY...")
 14. Update metadata with PID and restore provenance
 15. Print: "VM vm-01HZ... (job-42) restored from checkpoint after-setup"
```

**Boot timeout and restore**: The `--boot-timeout` flag applies only to cold boot (`cocoon run` / `cocoon start`). Restore bypasses boot detection entirely — the VM resumes from a snapshot with all services already running. No boot timeout is applied during restore.

### 3.3 Disk Handling Strategy

Because the VM is paused during checkpoint, no writes are in flight, so the disk is consistent with the VM state. The initial implementation uses a direct file copy of the overlay:

```go
// snapshotOverlay copies the VM's overlay disk to the checkpoint directory.
// Must be called while the VM is PAUSED to ensure consistency.
func snapshotOverlay(srcOverlay, dstOverlay string) error {
    return copyFile(srcOverlay, dstOverlay)
}
```

**Why copy instead of internal qcow2 snapshot**: Internal qcow2 snapshots add complexity to the backing chain and complicate GC. A full copy is simpler, more portable, and avoids coupling the checkpoint's disk state to the live VM's overlay file. The cost is additional disk space and copy time, which is acceptable for Phase 2.

The overlay already contains only the delta from the base image, so the copy size equals the overlay's actual disk usage (not the virtual size).

### 3.4 CH Process Lifecycle for Restore

Restoring from a checkpoint requires launching a **new** Cloud Hypervisor process with the `--restore` flag. There is no `vm.create` or `vm.boot` API call -- CH loads all state from the snapshot directory and resumes execution.

```go
// buildRestoreArgs constructs the CH CLI arguments for restoring from a checkpoint.
func buildRestoreArgs(socketPath, snapshotDir string) []string {
    return []string{
        "--api-socket", socketPath,
        "--restore", fmt.Sprintf("source_url=%s", snapshotDir),
    }
}
```

### 3.5 Checkpoint Name Resolution

Checkpoints follow the same resolution pattern as VMs:

```go
// ResolveCheckpointRef resolves a user-provided reference to a checkpoint ID.
// If ref starts with "ckpt-", treat as direct ID; otherwise look up name index.
func (m *checkpointManager) ResolveCheckpointRef(ref string) (string, error) {
    if strings.HasPrefix(ref, "ckpt-") {
        metaPath := filepath.Join(m.cfg.CheckpointDir(), ref, "checkpoint.json")
        if _, err := os.Stat(metaPath); err != nil {
            return "", fmt.Errorf("checkpoint not found: %s", ref)
        }
        return ref, nil
    }

    index, err := LoadCheckpointIndex(m.cfg)
    if err != nil {
        return "", fmt.Errorf("load checkpoint index: %w", err)
    }

    ckptID, ok := index[ref]
    if !ok {
        return "", fmt.Errorf("checkpoint not found: %s", ref)
    }
    return ckptID, nil
}
```

### 3.6 Idempotency Rules

| Operation          | Idempotent? | Behavior on Retry                          |
|--------------------|-------------|--------------------------------------------|
| `checkpoint`       | No          | Creates a new checkpoint each time         |
| `restore`          | No          | Creates a new VM each time                 |
| `checkpoint list`  | Yes         | Read-only                                  |
| `checkpoint delete`| Yes         | No-op if checkpoint does not exist         |

### 3.7 Checkpoint/Restore: Cloud Image VMs (qcow2)

Cloud image VMs use a qcow2 overlay backed by the base cloud image. The checkpoint/restore flow for this path is the default described in §3.1-§3.4 above:

**Checkpoint:**

1. Pause VM (`PUT /api/v1/vm.pause`)
2. Copy the qcow2 overlay file to the checkpoint directory (full file copy while VM is paused)
3. Snapshot CH state (`PUT /api/v1/vm.snapshot`) -- saves memory + device state
4. Record checkpoint metadata (`checkpoint_type: "qcow2"`, CH snapshot path, overlay path)
5. Resume or stop VM

**Restore:**

1. Copy the checkpoint's `overlay.qcow2` to the new VM directory
2. Launch CH with `--restore` (CH restores memory + device state)
3. VM resumes from exact checkpoint state

The qcow2 overlay contains an absolute `backing-file` path to the base cloud image. This means the base image must exist at the same absolute path on the restore host (see §10.9.1 for the same-host constraint).

### 3.8 Checkpoint/Restore: OCI VM Images (overlayfs)

OCI VM image VMs use overlayfs + virtiofsd for the rootfs instead of a qcow2 overlay. The overlayfs `upperdir` already serves as the persistent, copy-on-write disk state for the VM. This path does not involve qcow2 at all.

**Checkpoint:**

1. Pause VM (`PUT /api/v1/vm.pause`)
2. Snapshot CH state (`PUT /api/v1/vm.snapshot`) -- saves memory + device state
3. The `upperdir/` already IS the persistent disk state (no separate disk snapshot or copy needed)
4. Copy or snapshot the `upperdir/` directory to the checkpoint directory
5. Record checkpoint metadata (`checkpoint_type: "overlayfs"`, CH snapshot path, upperdir path)
6. Resume or stop VM

**Restore:**

1. Mount overlayfs: `lowerdir=custom-N:...:rootfs, upperdir=preserved_upper, workdir=new_work`
2. Spawn virtiofsd serving the merged overlayfs mount
3. Launch CH with `--restore` (CH restores memory + device state; virtiofsd reconnects via the restored virtio-fs device)
4. VM resumes from exact checkpoint state

**Key difference from the qcow2 path**: The overlayfs path does NOT have the same-host backing-file constraint. Overlayfs `lowerdir` paths are specified at mount time and can point to any location where the OCI image layers have been extracted. As long as the restore host has the same OCI image layers (verified by manifest digest), the `lowerdir` paths can be remapped freely. This makes the overlayfs path suitable for cross-host restore without `qemu-img rebase` or similar tooling.

**upperdir preservation**: During checkpoint, the `upperdir/` directory is copied (or snapshotted via filesystem-level mechanisms like `cp -a` or btrfs/zfs snapshots) into the checkpoint directory. The copy size depends on how many files the VM has written or modified since boot -- this is file-level COW, so only changed files are present in the upperdir.

---

## 4. Checkpoint Storage

### 4.1 Storage Layout

The checkpoint directory layout depends on the checkpoint type (`checkpoint_type` field in `checkpoint.json`):

**Cloud image VMs (qcow2 path):**

```
/var/lib/cocoon/checkpoints/{ckpt-id}/
├── checkpoint.json                 # Checkpoint metadata (checkpoint_type: "qcow2")
├── ch-snapshot/                    # CH state directory
│   ├── config                      # CH VM config snapshot
│   └── state                       # Memory + device + vCPU state
└── overlay.qcow2                   # Disk state: qcow2 overlay copy
```

**OCI VM image VMs (overlayfs path):**

```
/var/lib/cocoon/checkpoints/{ckpt-id}/
├── checkpoint.json                 # Checkpoint metadata (checkpoint_type: "overlayfs")
├── ch-snapshot/                    # CH state directory (same as qcow2 path)
│   ├── config                      # CH VM config snapshot
│   └── state                       # Memory + device + vCPU state
└── upper/                          # Disk state: preserved overlayfs upperdir copy
```

For the qcow2 path, `overlay.qcow2` is the disk state (a copy of the VM's COW overlay at checkpoint time). For the overlayfs path, `upper/` is the disk state (a copy of the VM's overlayfs upperdir containing all files written or modified since boot). The `ch-snapshot/` directory is identical for both paths -- it contains the CH memory and device state.

**Full directory tree:**

```
/var/lib/cocoon/
├── checkpoints/                            # Checkpoint root
│   ├── checkpoint-index.json               # name -> ckpt-id mapping
│   ├── checkpoint-index.lock               # flock for index updates
│   └── {ckpt-id}/                          # e.g., ckpt-01HXYZ.../
│       ├── checkpoint.json                 # Includes checkpoint_type: "qcow2" | "overlayfs"
│       ├── ch-snapshot/                    # CH memory + device state (both paths)
│       │   ├── config                      # CH VM config snapshot
│       │   └── state                       # Memory + device + vCPU state
│       ├── overlay.qcow2                   # Only for qcow2 path: disk state
│       └── upper/                          # Only for overlayfs path: preserved upperdir
├── vms/
│   └── ...
└── db/
    └── references.json                     # Tracks both VM and checkpoint refs
```

**Note**: Checkpoint storage is separate from the image cache ([docs/05-storage-management.md](./05-storage-management.md)). Checkpoints use their own ID namespace (`ckpt-{ulid}`) and do not use the `base_key` format. Image refcounting tracks checkpoint references via `AddReference(baseKey, checkpointID, ...)` to prevent GC of base images that checkpoints depend on.

### 4.2 Checkpoint Metadata Schema

```go
// CheckpointMetadata is written to checkpoint.json (immutable after creation).
type CheckpointMetadata struct {
    // Identity
    CheckpointID   string `json:"checkpoint_id"`   // ckpt-{ulid}
    Name           string `json:"name"`            // Human-readable name
    Description    string `json:"description"`     // Optional description
    CheckpointType string `json:"checkpoint_type"` // "qcow2" or "overlayfs"

    // Source VM provenance
    SourceVMID   string `json:"source_vm_id"`   // VM this was checkpointed from
    SourceVMName string `json:"source_vm_name"` // Source VM name at checkpoint time

    // Image provenance (from source VM's config.json)
    ImageRef       string `json:"image_ref"`
    BaseKey        string `json:"base_key"`
    BaseDigestFull string `json:"base_digest_full"`
    Arch           string `json:"arch"`

    // VM configuration at checkpoint time
    CPUs         int                `json:"cpus"`
    MemoryMB     int64              `json:"memory_mb"`
    DiskSize     string             `json:"disk_size"`
    BootStrategy types.BootStrategy `json:"boot_strategy"`
    FirmwarePath string             `json:"firmware_path"`

    // Storage paths within the checkpoint directory
    CHSnapshotDir string `json:"ch_snapshot_dir"`          // Relative: "ch-snapshot"
    OverlayFile   string `json:"overlay_file,omitempty"`   // Relative: "overlay.qcow2" (qcow2 path only)
    UpperDir      string `json:"upper_dir,omitempty"`      // Relative: "upper" (overlayfs path only)

    // Size information
    CHSnapshotSizeBytes int64 `json:"ch_snapshot_size_bytes"` // CH state files total
    DiskStateSizeBytes  int64 `json:"disk_state_size_bytes"`  // Overlay or upperdir size
    TotalSizeBytes      int64 `json:"total_size_bytes"`       // Grand total

    // Firmware version tracking (for compatibility validation)
    FirmwareVersion string `json:"firmware_version,omitempty"`
    CHVersion       string `json:"ch_version,omitempty"`

    // Compression
    Compressed      bool   `json:"compressed"`
    CompressionAlgo string `json:"compression_algo,omitempty"` // "zstd" or ""

    // Integrity
    CHSnapshotChecksum string `json:"ch_snapshot_checksum,omitempty"` // SHA-256
    OverlayChecksum    string `json:"overlay_checksum,omitempty"`     // SHA-256

    // Timestamps
    CreatedAt string `json:"created_at"` // RFC 3339

    // Schema version
    SchemaVersion int `json:"schema_version"` // Currently 1
}

const CurrentCheckpointSchemaVersion = 1
```

### 4.3 Checkpoint Size

A checkpoint's size is dominated by two components:

| Component | Typical Size | Notes |
|-----------|-------------|-------|
| CH state (memory) | ~= VM memory size | 256MB VM -> ~256MB state files |
| CH state (devices/vCPU) | < 1MB | Negligible |
| Overlay snapshot | 0 - overlay virtual size | Only written blocks |

**Example**: A VM with 1GB memory and 50MB of disk writes produces a checkpoint of approximately 1.05GB.

### 4.4 Compression

Memory state files are highly compressible (many zero pages, repeated patterns). zstd compression can reduce checkpoint size by 2-5x.

```go
type CheckpointOptions struct {
    // ... other fields ...
    Compress bool `json:"compress,omitempty"` // zstd compression of CH state files
}
```

Phase 2 scope: Compression is optional and off by default. A `--compress` flag on `cocoon checkpoint` enables zstd compression. Restore transparently decompresses if the checkpoint is compressed.

---

## 5. Configuration Changes

### 5.1 Config Path Helpers

```go
// New path helpers on CocoonConfig

func (c *CocoonConfig) CheckpointDir() string {
    return filepath.Join(c.RootDir, "checkpoints")
}

func (c *CocoonConfig) CheckpointPersistDir(ckptID string) string {
    return filepath.Join(c.RootDir, "checkpoints", ckptID)
}

func (c *CocoonConfig) CheckpointMetadataPath(ckptID string) string {
    return filepath.Join(c.RootDir, "checkpoints", ckptID, "checkpoint.json")
}

func (c *CocoonConfig) CheckpointSnapshotDir(ckptID string) string {
    return filepath.Join(c.RootDir, "checkpoints", ckptID, "ch-snapshot")
}

func (c *CocoonConfig) CheckpointOverlayPath(ckptID string) string {
    return filepath.Join(c.RootDir, "checkpoints", ckptID, "overlay.qcow2")
}

func (c *CocoonConfig) CheckpointIndexFile() string {
    return filepath.Join(c.RootDir, "checkpoints", "checkpoint-index.json")
}

func (c *CocoonConfig) CheckpointIndexLock() string {
    return filepath.Join(c.RootDir, "checkpoints", "checkpoint-index.lock")
}
```

### 5.2 Metadata Extensions

The `VMMetadataFile` gains checkpoint provenance fields:

```go
type VMMetadataFile struct {
    // ... existing fields ...

    // Checkpoint provenance (set when VM was created via restore)
    RestoredFromCheckpoint string `json:"restored_from_checkpoint,omitempty"` // ckpt-{ulid}
    LastCheckpointID       string `json:"last_checkpoint_id,omitempty"`       // Most recent checkpoint taken
}
```

### 5.3 VMInspect Extensions

```go
type VMInspect struct {
    // ... existing fields ...

    // Checkpoint info (Phase 2)
    Checkpoint *InspectCheckpointInfo `json:"checkpoint,omitempty"`
}

type InspectCheckpointInfo struct {
    RestoredFrom   string `json:"restored_from,omitempty"`   // Checkpoint ID if restored
    LastCheckpoint string `json:"last_checkpoint,omitempty"` // Most recent checkpoint ID
    PausedAt       string `json:"paused_at,omitempty"`       // When paused (if PAUSED)
}
```

### 5.4 Lock Hierarchy Extension

The checkpoint index lock fits into the existing lock hierarchy at Level 2 (same as name-index.lock). It is never held simultaneously with the name index lock or references lock.

```
Level 1: GC Lock (global)
    |
Level 2: Reference Counter Lock (global)
Level 2: Name Index Lock (global)
Level 2: Checkpoint Index Lock (global) -- never held with name-index or references lock
    |
Level 3: Image Conversion Lock (per-checksum)
    |
Level 4: VM Metadata Lock (per-VM)
```

---

## 6. Implementation

### 6.1 Hypervisor Client Extensions

```go
type Client interface {
    // ... existing methods (including PauseVM, ResumeVM from 13-pause-resume.md) ...

    // SnapshotVM sends PUT /api/v1/vm.snapshot to save VM state.
    // destinationDir is the directory where CH writes its state files.
    SnapshotVM(ctx context.Context, socketPath string, destinationDir string) error

    // LaunchRestore starts a new CH process that restores from a snapshot.
    // Unlike Launch, this uses --restore and does not call vm.create/vm.boot.
    LaunchRestore(ctx context.Context, vmID string, snapshotDir string) (pid int, err error)
}
```

Implementation of `SnapshotVM`:

```go
func (c *client) SnapshotVM(ctx context.Context, socketPath string, destinationDir string) error {
    payload := struct {
        DestinationURL string `json:"destination_url"`
    }{
        DestinationURL: destinationDir,
    }

    body, err := json.Marshal(payload)
    if err != nil {
        return fmt.Errorf("marshal snapshot request: %w", err)
    }

    url := "http://localhost/api/v1/vm.snapshot"
    req, err := http.NewRequestWithContext(ctx, "PUT", url, bytes.NewReader(body))
    if err != nil {
        return fmt.Errorf("create snapshot request: %w", err)
    }
    req.Header.Set("Content-Type", "application/json")

    httpClient := c.httpClientForSocket(socketPath)
    resp, err := httpClient.Do(req)
    if err != nil {
        return fmt.Errorf("snapshot VM: %w", err)
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusNoContent {
        respBody, _ := io.ReadAll(resp.Body)
        return fmt.Errorf("snapshot VM: CH returned %d: %s", resp.StatusCode, string(respBody))
    }

    return nil
}
```

**Note on `destination_url` and `source_url` naming**: Despite the `_url` suffix in the JSON field names, Cloud Hypervisor's snapshot/restore REST API accepts **plain filesystem paths** -- no `file://` prefix is needed. For example, `"destination_url": "/var/lib/cocoon/checkpoints/ckpt-01HY.../ch-snapshot"` is a plain path, not a URL. The same applies to `source_url` in the `--restore` CLI flag (e.g., `source_url=/var/lib/cocoon/checkpoints/ckpt-01HY.../ch-snapshot`). This naming convention comes from Cloud Hypervisor's API schema, which uses "url" loosely to mean "location".

Implementation of `LaunchRestore`:

```go
func (c *client) LaunchRestore(
    ctx context.Context,
    vmID string,
    snapshotDir string,
) (pid int, err error) {
    // Ensure runtime directory exists.
    runtimeDir := c.cfg.VMRuntimeDir(vmID)
    if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
        return 0, fmt.Errorf("create runtime dir: %w", err)
    }

    socketPath := c.cfg.VMSocketPath(vmID)
    _ = os.Remove(socketPath)
    _ = os.Remove(c.cfg.VMPIDPath(vmID))

    args := buildRestoreArgs(socketPath, snapshotDir)

    cmd := exec.CommandContext(ctx, c.cfg.CHBinary, args...)
    configureCHProcess(cmd)

    if err := cmd.Start(); err != nil {
        return 0, fmt.Errorf("start cloud-hypervisor --restore: %w", err)
    }

    pid = cmd.Process.Pid

    pidPath := c.cfg.VMPIDPath(vmID)
    if err := utils.WritePIDFile(pidPath, pid); err != nil {
        _ = cmd.Process.Kill()
        return 0, fmt.Errorf("write PID file: %w", err)
    }

    if err := c.WaitForSocket(ctx, socketPath, 10*time.Second); err != nil {
        _ = cmd.Process.Kill()
        return 0, fmt.Errorf("wait for restore socket: %w", err)
    }

    _ = cmd.Process.Release()
    return pid, nil
}
```

### 6.2 CheckpointManager Interface

```go
// CheckpointManager handles checkpoint lifecycle operations.
// Separate from vm.Manager because checkpoints are independent entities
// with their own storage, naming, and GC lifecycle.
type CheckpointManager interface {
    // Checkpoint creates a checkpoint from a paused VM.
    // The VM must be in PAUSED state (or --live auto-pauses).
    Checkpoint(ctx context.Context, vmID string, opts *CheckpointOptions) (*types.CheckpointMetadata, error)

    // Restore creates a new VM from a checkpoint.
    // Returns the new VM's config.
    Restore(ctx context.Context, checkpointID string, opts *RestoreOptions) (*types.VMConfig, error)

    // List returns all checkpoints, optionally filtered by source VM.
    List(ctx context.Context, sourceVMID string) ([]*types.CheckpointMetadata, error)

    // Inspect returns metadata for a single checkpoint.
    Inspect(ctx context.Context, checkpointID string) (*types.CheckpointMetadata, error)

    // Delete removes a checkpoint and unpins its base image reference.
    Delete(ctx context.Context, checkpointID string) error

    // ResolveCheckpointRef resolves a user-provided reference to a checkpoint ID.
    ResolveCheckpointRef(ref string) (string, error)
}

// CheckpointOptions holds parameters for creating a checkpoint.
type CheckpointOptions struct {
    Name        string `json:"name,omitempty"`
    Description string `json:"description,omitempty"`
    Live        bool   `json:"live,omitempty"`    // Auto-pause/resume
    Compress    bool   `json:"compress,omitempty"` // zstd compression
}

// RestoreOptions holds parameters for restoring from a checkpoint.
type RestoreOptions struct {
    Name string `json:"name,omitempty"` // Name for the new VM
}
```

### 6.3 Checkpoint Implementation

```go
func (m *checkpointManager) Checkpoint(
    ctx context.Context,
    vmID string,
    opts *CheckpointOptions,
) (*types.CheckpointMetadata, error) {
    // 1. Load VM state and config.
    meta, err := m.vmMgr.LoadMetadata(vmID)
    if err != nil {
        return nil, err
    }

    vmState := types.VMState(meta.State)

    // 2. Handle --live flag: auto-pause.
    if opts.Live {
        if vmState == types.VMStateRunning {
            if err := m.vmMgr.Pause(ctx, vmID); err != nil {
                return nil, fmt.Errorf("auto-pause for checkpoint: %w", err)
            }
            defer func() {
                // Auto-resume regardless of checkpoint success/failure.
                if resumeErr := m.vmMgr.Resume(ctx, vmID); resumeErr != nil {
                    log.Errorf("failed to auto-resume VM %s after checkpoint: %v", vmID, resumeErr)
                    // Note: checkpoint succeeded but VM remains paused. User must manually resume.
                }
            }()
        } else if vmState != types.VMStatePaused {
            return nil, fmt.Errorf("cannot checkpoint VM in state %s (must be RUNNING or PAUSED)", meta.State)
        }
    } else {
        if vmState != types.VMStatePaused {
            return nil, fmt.Errorf(
                "VM must be PAUSED to checkpoint (current state: %s). "+
                    "Use 'cocoon pause %s' first, or use --live for auto-pause",
                meta.State, vmID,
            )
        }
    }

    // 3. Check for passthrough devices (incompatible with checkpoint).
    cfg, err := m.vmMgr.LoadConfig(vmID)
    if err != nil {
        return nil, err
    }
    if len(cfg.Devices) > 0 {
        return nil, fmt.Errorf(
            "cannot checkpoint VM with passthrough devices (%d device(s) attached). "+
                "Cloud Hypervisor does not support snapshotting VFIO device state",
            len(cfg.Devices),
        )
    }

    // 4. Generate checkpoint ID and create directory.
    ckptID := "ckpt-" + ulid.New()
    ckptDir := m.cfg.CheckpointPersistDir(ckptID)
    if err := os.MkdirAll(ckptDir, 0o755); err != nil {
        return nil, fmt.Errorf("create checkpoint directory: %w", err)
    }

    // Cleanup on failure.
    success := false
    defer func() {
        if !success {
            _ = os.RemoveAll(ckptDir)
        }
    }()

    // 5. Copy overlay disk.
    srcOverlay := cfg.OverlayPath
    dstOverlay := m.cfg.CheckpointOverlayPath(ckptID)
    if err := copyFile(srcOverlay, dstOverlay); err != nil {
        return nil, fmt.Errorf("copy overlay: %w", err)
    }

    // 6. Capture CH snapshot.
    snapshotDir := m.cfg.CheckpointSnapshotDir(ckptID)
    if err := os.MkdirAll(snapshotDir, 0o755); err != nil {
        return nil, fmt.Errorf("create snapshot directory: %w", err)
    }
    if err := m.hyper.SnapshotVM(ctx, cfg.SocketPath, snapshotDir); err != nil {
        return nil, fmt.Errorf("CH snapshot: %w", err)
    }

    // 7. Compute sizes and checksums.
    overlaySize := fileSize(dstOverlay)
    snapshotSize := dirSize(snapshotDir)

    // 8. Build metadata.
    ckptMeta := &types.CheckpointMetadata{
        CheckpointID:        ckptID,
        Name:                opts.Name,
        Description:         opts.Description,
        SourceVMID:          vmID,
        SourceVMName:        meta.Name,
        ImageRef:            cfg.ImageRef,
        BaseKey:             cfg.BaseKey,
        BaseDigestFull:      cfg.BaseDigestFull,
        Arch:                cfg.Arch,
        CPUs:                cfg.CPUs,
        MemoryMB:            cfg.MemoryMB,
        DiskSize:            cfg.DiskSize,
        BootStrategy:        cfg.BootStrategy,
        FirmwarePath:        cfg.FirmwarePath,
        CHSnapshotDir:       "ch-snapshot",
        OverlayFile:         "overlay.qcow2",
        CHSnapshotSizeBytes: snapshotSize,
        OverlaySizeBytes:    overlaySize,
        TotalSizeBytes:      snapshotSize + overlaySize,
        CreatedAt:           time.Now().UTC().Format(time.RFC3339),
        SchemaVersion:       types.CurrentCheckpointSchemaVersion,
    }

    // 9. Write checkpoint.json.
    metaPath := m.cfg.CheckpointMetadataPath(ckptID)
    if err := writeJSON(metaPath, ckptMeta); err != nil {
        return nil, fmt.Errorf("write checkpoint metadata: %w", err)
    }

    // 10. Register name in checkpoint index.
    if opts.Name != "" {
        if err := m.registerCheckpointName(opts.Name, ckptID); err != nil {
            return nil, fmt.Errorf("register checkpoint name: %w", err)
        }
    }

    // 11. Pin base image reference for the checkpoint.
    // If pinning fails, roll back the checkpoint-index registration
    // so the directory cleanup (deferred above) leaves no dangling index entry.
    if err := m.refCounter.AddReference(
        cfg.BaseKey, ckptID, cfg.BaseDigestFull, cfg.ImageRef,
    ); err != nil {
        if opts.Name != "" {
            _ = m.unregisterCheckpointName(opts.Name)
        }
        return nil, fmt.Errorf("pin base image reference: %w", err)
    }

    // 12. Update source VM metadata.
    meta.LastCheckpointID = ckptID
    _ = m.vmMgr.SaveMetadata(meta)

    success = true
    return ckptMeta, nil
}
```

### 6.4 Restore Implementation

```go
func (m *checkpointManager) Restore(
    ctx context.Context,
    checkpointID string,
    opts *RestoreOptions,
) (*types.VMConfig, error) {
    // 1. Load checkpoint metadata.
    ckptMeta, err := m.loadCheckpointMeta(checkpointID)
    if err != nil {
        return nil, fmt.Errorf("load checkpoint: %w", err)
    }

    // 2. Validate base image exists.
    baseImagePath := m.cfg.BaseImagePath(ckptMeta.BaseKey)
    if _, err := os.Stat(baseImagePath); err != nil {
        return nil, fmt.Errorf(
            "base image for checkpoint not found at %s. "+
                "The base image may have been garbage collected. "+
                "Re-pull the image: cocoon image pull %s",
            baseImagePath, ckptMeta.ImageRef,
        )
    }

    // 3. Generate new VM ID.
    vmID := "vm-" + ulid.New()

    // 4. Create VM directory.
    vmDir := m.cfg.VMPersistDir(vmID)
    if err := os.MkdirAll(vmDir, 0o755); err != nil {
        return nil, fmt.Errorf("create VM directory: %w", err)
    }

    // Cleanup on failure.
    success := false
    defer func() {
        if !success {
            _ = os.RemoveAll(vmDir)
        }
    }()

    // 5. Copy checkpoint overlay to new VM.
    srcOverlay := m.cfg.CheckpointOverlayPath(checkpointID)
    dstOverlay := filepath.Join(vmDir, "overlay.qcow2")
    if err := copyFile(srcOverlay, dstOverlay); err != nil {
        return nil, fmt.Errorf("copy overlay from checkpoint: %w", err)
    }

    // 6. Build config.json for the new VM.
    vmCfg := &types.VMConfig{
        VMID:           vmID,
        Name:           opts.Name,
        ImageRef:       ckptMeta.ImageRef,
        BaseKey:        ckptMeta.BaseKey,
        BaseDigestFull: ckptMeta.BaseDigestFull,
        Arch:           ckptMeta.Arch,
        CPUs:           ckptMeta.CPUs,
        MemoryMB:       ckptMeta.MemoryMB,
        DiskSize:       ckptMeta.DiskSize,
        BootStrategy:   ckptMeta.BootStrategy,
        FirmwarePath:   ckptMeta.FirmwarePath,
        OverlayPath:    dstOverlay,
        SocketPath:     m.cfg.VMSocketPath(vmID),
    }

    cfgPath := filepath.Join(vmDir, "config.json")
    if err := writeJSON(cfgPath, vmCfg); err != nil {
        return nil, fmt.Errorf("write config.json: %w", err)
    }

    // 7. Write initial metadata.
    vmMeta := &types.VMMetadataFile{
        VMID:                   vmID,
        Name:                   opts.Name,
        State:                  string(types.VMStateCreating),
        RestoredFromCheckpoint: checkpointID,
        CreatedAt:              time.Now().UTC().Format(time.RFC3339),
        SchemaVersion:          types.CurrentMetadataSchemaVersion,
    }

    metaPath := filepath.Join(vmDir, "metadata.json")
    if err := writeJSON(metaPath, vmMeta); err != nil {
        return nil, fmt.Errorf("write metadata.json: %w", err)
    }

    // 8. Pin base image reference.
    if err := m.refCounter.AddReference(
        ckptMeta.BaseKey, vmID, ckptMeta.BaseDigestFull, ckptMeta.ImageRef,
    ); err != nil {
        return nil, fmt.Errorf("pin base image reference: %w", err)
    }

    // 9. Register VM name.
    if opts.Name != "" {
        if err := m.vmMgr.RegisterName(opts.Name, vmID); err != nil {
            return nil, fmt.Errorf("register VM name: %w", err)
        }
    }

    // 10. Launch CH with --restore.
    snapshotDir := m.cfg.CheckpointSnapshotDir(checkpointID)
    pid, err := m.hyper.LaunchRestore(ctx, vmID, snapshotDir)
    if err != nil {
        return nil, fmt.Errorf("launch restore: %w", err)
    }

    // 11. Transition to RUNNING.
    if err := m.vmMgr.TransitionState(vmID, types.VMStateRunning,
        fmt.Sprintf("restored from %s", checkpointID)); err != nil {
        return nil, fmt.Errorf("transition state: %w", err)
    }

    // 12. Update metadata with PID.
    vmMeta.PID = pid
    _ = m.vmMgr.SaveMetadata(vmMeta)

    success = true
    return vmCfg, nil
}
```

---

## 7. CLI

### 7.1 Checkpoint Command

```go
func checkpointCommand() *cli.Command {
    return &cli.Command{
        Name:  "checkpoint",
        Usage: "Manage VM checkpoints",
        Subcommands: []*cli.Command{
            {
                Name:      "create",
                Usage:     "Create a checkpoint from a VM",
                ArgsUsage: "VM_REF",
                Flags: []cli.Flag{
                    &cli.StringFlag{
                        Name:  "name",
                        Usage: "human-readable name for the checkpoint",
                    },
                    &cli.StringFlag{
                        Name:  "description",
                        Usage: "optional description",
                    },
                    &cli.BoolFlag{
                        Name:  "live",
                        Usage: "auto-pause before checkpoint, auto-resume after",
                    },
                    &cli.BoolFlag{
                        Name:  "compress",
                        Usage: "compress CH state files with zstd",
                    },
                },
                Action: checkpointCreateAction,
            },
            {
                Name:      "list",
                Aliases:   []string{"ls"},
                Usage:     "List checkpoints",
                Flags: []cli.Flag{
                    &cli.StringFlag{
                        Name:  "vm",
                        Usage: "filter by source VM reference",
                    },
                    &cli.StringFlag{
                        Name:  "format",
                        Value: "table",
                        Usage: "output format (table, json)",
                    },
                },
                Action: checkpointListAction,
            },
            {
                Name:      "inspect",
                Usage:     "Show checkpoint details",
                ArgsUsage: "CHECKPOINT_REF",
                Action:    checkpointInspectAction,
            },
            {
                Name:      "delete",
                Aliases:   []string{"rm"},
                Usage:     "Delete a checkpoint",
                ArgsUsage: "CHECKPOINT_REF",
                Flags: []cli.Flag{
                    &cli.BoolFlag{
                        Name:  "force",
                        Usage: "force deletion without confirmation",
                    },
                },
                Action: checkpointDeleteAction,
            },
        },
    }
}
```

### 7.2 Restore Command

```go
func restoreCommand() *cli.Command {
    return &cli.Command{
        Name:      "restore",
        Usage:     "Create a new VM from a checkpoint",
        ArgsUsage: "CHECKPOINT_REF",
        Flags: []cli.Flag{
            &cli.StringFlag{
                Name:  "name",
                Usage: "name for the restored VM",
            },
        },
        Action: restoreAction,
    }
}
```

### 7.3 Create-from-Checkpoint Shorthand

For a uniform interface, the `cocoon create` command gains a `--from-checkpoint` flag:

```go
// In vmCreateFlags() addition
&cli.StringFlag{
    Name:  "from-checkpoint",
    Usage: "create VM from checkpoint instead of image (equivalent to 'cocoon restore')",
},
```

```bash
# These are equivalent:
cocoon restore ubuntu-agent-golden --name fast-vm
cocoon create --from-checkpoint ubuntu-agent-golden --name fast-vm
```

### 7.4 Usage Examples

```bash
# Create a golden checkpoint
cocoon create myorg/ubuntu-agent:latest --name golden-source
cocoon start golden-source
# ... wait for boot + cloud-init ...
cocoon checkpoint create golden-source --live --name "ubuntu-agent-golden"

# List checkpoints
cocoon checkpoint list
CHECKPOINT ID    NAME                    SOURCE VM       SIZE      CREATED
ckpt-01HY...    ubuntu-agent-golden     golden-source   1.2 GB    2 min ago

# Rapidly create VMs from the golden checkpoint
cocoon restore ubuntu-agent-golden --name agent-001  # ~200ms
cocoon restore ubuntu-agent-golden --name agent-002  # ~200ms
cocoon restore ubuntu-agent-golden --name agent-003  # ~200ms

# Inspect a checkpoint
cocoon checkpoint inspect ubuntu-agent-golden

# Delete a checkpoint
cocoon checkpoint delete ubuntu-agent-golden

# Delete the source VM (checkpoint is independent)
cocoon stop golden-source
cocoon delete golden-source
# Checkpoint ubuntu-agent-golden still exists and can be restored
```

---

## 8. Warm Start Workflow

### 8.1 Golden Checkpoint Concept

A "golden checkpoint" is a checkpoint taken after a VM has fully booted and been configured for a specific workload. It serves as a template for rapid VM creation:

```
Step 1: Cold boot (one-time cost)
  cocoon create myorg/ubuntu-agent:latest --name golden-source
  cocoon start golden-source
  # Wait 5-30 seconds for full boot

Step 2: Capture golden checkpoint
  cocoon checkpoint create golden-source --live --name "ubuntu-agent-golden"

Step 3: Rapid VM creation (repeated, ~200ms each)
  cocoon restore ubuntu-agent-golden --name agent-001
  cocoon restore ubuntu-agent-golden --name agent-002
  cocoon restore ubuntu-agent-golden --name agent-003

Step 4: (Optional) Delete source VM
  cocoon stop golden-source && cocoon delete golden-source
```

### 8.2 Batch Warm Start

Each restore is independent and can run in parallel. The golden checkpoint is read-only after creation.

```
                    +-- restore --> agent-001 (RUNNING in ~200ms)
                    |
golden checkpoint --+-- restore --> agent-002 (RUNNING in ~200ms)
                    |
                    +-- restore --> agent-003 (RUNNING in ~200ms)
                    |
                    +-- ... (N agents)
```

### 8.3 Pre-warmed Environment Use Case

For CI/CD jobs that require a complex environment:

```bash
# One-time setup: boot, install dependencies, checkpoint
cocoon create myorg/ci-base:latest --name ci-setup
cocoon start ci-setup
# SSH in and install: npm, python, docker, test fixtures, etc. (10 minutes)
cocoon checkpoint create ci-setup --live --name "ci-ready"

# Each CI job: restore in ~200ms instead of 10-minute setup
cocoon restore ci-ready --name "job-$BUILD_NUMBER"
# Run tests inside the VM...
cocoon stop "job-$BUILD_NUMBER"
cocoon delete "job-$BUILD_NUMBER"
```

### 8.4 Performance Characteristics

The dominant cost in warm start is the overlay copy. For minimal overlays (few MB of delta after boot), restore completes in under 200ms. For VMs with large overlays (GB of disk writes), the copy time dominates.

**Overlay copy cost during checkpoint**: The overlay copy is also the dominant cost during checkpoint creation, not just restore. While the VM is paused (step 6 in the checkpoint flow, Section 3.1), the overlay file is copied synchronously. The pause duration is proportional to the overlay file size:

| Overlay Size | Approximate Pause Duration | Notes |
|-------------|---------------------------|-------|
| < 100 MB | < 0.5s | Minimal writes after boot |
| 100 MB - 1 GB | 0.5 - 2s | Typical development VM |
| 1 GB - 5 GB | 2 - 10s | Package installation, model loading |
| > 5 GB | 10s+ | Large dataset writes |

For VMs with large overlays, the pause duration may be significant. The `--live` flag (Section 3.1) auto-manages the pause/resume cycle to minimize impact -- the VM is paused only for the duration of the copy and CH snapshot, then immediately resumed. A `--background` option that copies the overlay while the VM remains running (using filesystem-level snapshots or copy-on-write) is deferred to a future phase.

Strategies for minimizing overlay size:
- Take the checkpoint as early as possible after boot (before large writes)
- Use thin-provisioned base images
- Avoid writing large files before checkpointing

---

## 9. GC Integration

### 9.1 Checkpoint References

Checkpoints reference base images just like VMs do. The GC system must account for checkpoints:

```go
// CheckpointReferencePrefix distinguishes checkpoint refs from VM refs.
const CheckpointReferencePrefix = "ckpt-"

// When creating a checkpoint:
refCounter.AddReference(baseKey, checkpointID, digestFull, sourceRef)

// When deleting a checkpoint:
refCounter.RemoveReference(baseKey, checkpointID)
```

A base image cannot be garbage collected while any checkpoint or VM references it.

### 9.2 Orphaned Checkpoint Cleanup

GC detects checkpoint directories with missing or corrupt `checkpoint.json` and cleans them up.

**Orphaned checkpoint reference recovery**: A checkpoint directory may be deleted externally (manual `rm -rf`, disk failure) while `references.json` still holds a reference with the `ckpt-` prefix (e.g., `ckpt-01HYABC123`). This leaves an orphaned reference that pins the base image unnecessarily.

- **Detection**: `cocoon doctor` checks every `ckpt-`-prefixed reference in `references.json` and verifies the corresponding checkpoint directory exists under `/var/lib/cocoon/checkpoints/`. References pointing to non-existent checkpoint directories are reported as orphaned.
- **Repair**: `cocoon doctor --fix` removes orphaned checkpoint references from `references.json`. This unpins the base image, allowing GC to collect it if no other references remain.
- **GC resilience**: When GC iterates references in `references.json`, it must tolerate missing checkpoint directories gracefully. If a `ckpt-`-prefixed reference points to a non-existent directory, GC skips it (logs a warning) rather than crashing. The orphaned reference is left for `cocoon doctor --fix` to clean up.
- **Naming convention**: Checkpoint references use the `ckpt-` prefix to distinguish them from VM references (`vm-`) in `references.json`. This allows both GC and doctor to identify the reference type and validate it against the correct directory (`/var/lib/cocoon/checkpoints/` for checkpoints, `/var/lib/cocoon/vms/` for VMs).

### 9.3 Checkpoint Expiry (Future)

Optional TTL on checkpoints. Expired checkpoints are automatically cleaned up by GC. This is not implemented in Phase 2 but the `CreatedAt` field in checkpoint metadata enables it.

---

## 10. Limitations and Constraints

### 10.0 Cloud Hypervisor Minimum Version for Snapshot/Restore

Cloud Hypervisor minimum version for snapshot/restore: TBD (must be validated before implementation begins). The `cocoon doctor` check will be extended to verify this version requirement for Phase 2 warm-start features.

### 10.1 Firmware Version Compatibility

A checkpoint can only be restored with the **same firmware version** used when the checkpoint was taken. Firmware version mismatches may cause memory layout differences and silent data corruption.

`checkpoint.json` records the firmware path and version. Restore validates these match and refuses to proceed on mismatch (with a `--force` override).

### 10.2 Cloud Hypervisor Version Compatibility

The CH snapshot format may change between releases. `checkpoint.json` records the CH version. Restore warns on minor/patch version mismatch but refuses to proceed on major version change (with a `--force` override).

### 10.3 Network State Not Preserved

TCP connections, UDP sockets, and in-flight network state are lost on restore. Applications must handle reconnection.

### 10.4 Device Passthrough Incompatibility

VMs with PCI passthrough devices (GPU, NIC) cannot be checkpointed because device state cannot be serialized. `cocoon checkpoint create` returns a clear error when passthrough devices are detected.

### 10.5 Volume Passthrough (virtio-fs)

VMs with virtio-fs volumes can be checkpointed. However, host paths must exist at restore time, and virtiofsd processes must be restarted for the restored VM.

### 10.6 Console PTY

On restore, Cloud Hypervisor allocates a new PTY with a different path. `cocoon console` discovers PTY paths dynamically via `GET /api/v1/vm.info`, so no special handling is needed.

### 10.7 Clock Skew

After restore, the guest's clock is set to the time of the checkpoint. The guest must resynchronize via NTP or virtio-clock.

### 10.8 Base Image Requirement

The base image referenced by the checkpoint's overlay must exist at restore time. Checkpoints pin the base image in `references.json`, preventing GC from collecting it while the checkpoint exists.

### 10.9 Same-Architecture Requirement

A checkpoint taken on x86_64 cannot be restored on aarch64 (and vice versa). The `arch` field in `checkpoint.json` is validated at restore time.

### 10.9.1 Same-Host Constraint (qcow2 Path Only)

**Phase 2 Scope**: For the **qcow2 path**, warm-start is supported only on the same host with the same `rootDir` configuration. The qcow2 overlay contains an absolute backing-file path to the base image; if the base image path changes (different host, different rootDir), the overlay becomes invalid. Cross-host migration with `qemu-img rebase` is deferred to Phase 3.

The **overlayfs path** does NOT have this constraint. Overlayfs `lowerdir` paths are specified at mount time, so they can be remapped to wherever the OCI image layers are extracted on the restore host. As long as the restore host has the same OCI image layers (verified by manifest digest), restore can proceed regardless of absolute paths.

### 10.9.2 OCI Path Constraints (overlayfs Path Only)

The overlayfs checkpoint/restore path has its own set of constraints:

- **virtiofsd required on restore host**: The restore host must have `virtiofsd` installed and accessible. The restore flow spawns a new virtiofsd process to serve the merged overlayfs mount to the restored VM.
- **OCI image layers must be extracted on restore host**: The OCI image layers that form the overlayfs `lowerdir` stack must be present on the restore host. The layers are identified by manifest digest -- the same image manifest must be pulled and extracted, but the extraction path can differ from the checkpoint host.
- **upperdir copy size**: The `upper/` directory in the checkpoint contains every file the VM has written or modified since boot (file-level COW). If the VM wrote many or large files, the upperdir copy may be substantial. Unlike qcow2 sparse files (block-level COW), the upperdir is a plain directory tree, so its on-disk size equals the sum of all modified file sizes.
- **Filesystem metadata preservation**: The upperdir copy must preserve ownership, permissions, extended attributes, and overlayfs-specific whiteout entries. Use `cp -a` or equivalent to ensure metadata fidelity.

### 10.10 Snapshot Invalidation Rules

A checkpoint becomes invalid (restore will fail or produce undefined behavior) when any of the following change between checkpoint creation and restore:

| Condition | Severity | Behavior |
|-----------|----------|----------|
| Cloud Hypervisor major version change | **Hard fail** | Snapshot format may be incompatible. Restore refuses with error. |
| Firmware binary differs from checkpoint time | **Hard fail** | Memory layout mismatch. Restore refuses with error (override with `--force`). |
| VM resource configuration change (CPUs, memory, disk size) | **Hard fail** | CH requires identical resource configuration for restore. Cocoon validates and refuses on mismatch. |
| Base image content changed (same key but different content) | **Hard fail** | Overlay references specific base image blocks. Restore validates the base image checksum recorded in `checkpoint.json` matches the on-disk file. |
| Guest kernel or initrd changed (inside base image) | **Silent corruption** | CH restores VM state assuming the original kernel. A different kernel in the base image causes undefined guest behavior. Caught by base image checksum validation above. |
| Host kernel version change | **Usually safe** | KVM API is stable across kernel versions. Rare edge cases with new CPU features. |
| Network configuration change | **Expected** | TCP/UDP connections are lost on restore by design (§10.3). Applications must reconnect. |

Cocoon validates firmware version, CH version, architecture, resource configuration, and base image checksum at restore time. Any mismatch produces a clear error with `--force` override for version warnings.

### 10.11 Network Identity on Restore

A checkpoint captures the complete VM memory, including the guest's network stack state (IP addresses, routing tables, ARP cache). When restoring from a golden checkpoint:

- **Default (`--network none`)**: No issue. The AI Agent sandbox use case has no networking, so there is no identity conflict. This is the recommended mode for golden checkpoints.

- **Networked VMs**: Each restore creates a new VM with a new CNI allocation (different IP from IPAM). However, the guest's memory still contains the old network configuration from the checkpoint. Cloud-init has already run during the original boot and will NOT re-run on restore (restore resumes execution, it does not re-boot). The guest retains the checkpoint's IP, causing conflicts when multiple VMs are restored from the same checkpoint.

**Mitigation for networked golden checkpoints**:

1. **Use DHCP-based networking**: Configure the guest to obtain IP via DHCP rather than static cloud-init assignment. On restore, the DHCP lease will have expired (clock skew, §10.7), and the guest's DHCP client will request a new lease. This requires the CNI network to use the `dhcp` IPAM plugin.
2. **Post-restore reconfiguration (future)**: A Phase 3 post-restore hook mechanism will allow running scripts inside the guest after restore to reconfigure networking, regenerate machine-id, and refresh SSH host keys.
3. **Single restore per checkpoint**: If each checkpoint is restored only once, there is no identity conflict (the restored VM inherits the checkpoint's network identity).

**Recommendation**: For golden checkpoints intended for batch restore (the primary warm-start use case), use `--network none` and configure networking post-restore via an external mechanism (e.g., the Phase 2 `cocoon console` for manual setup, or a Phase 3 agent-based configuration).

### 10.12 Operational Guidance

**Checkpoint sizing**: A checkpoint's disk footprint equals approximately:

```
checkpoint_size ≈ VM_memory_size + overlay_actual_usage
```

A VM with 2GB memory and 100MB of overlay writes produces a ~2.1GB checkpoint. With `--compress` (zstd), memory state typically compresses 2-5x (many zero pages), reducing the example to ~0.5-1.1GB.

**Capacity planning**:

- Monitor total checkpoint disk usage via `cocoon checkpoint list` (shows per-checkpoint size)
- Recommended: total checkpoint storage should not exceed 50% of available disk
- A `cocoon gc` run with `--checkpoints` flag removes orphaned checkpoint directories

**Concurrent restore**: Each restore performs a full overlay file copy and launches a CH process. On I/O-constrained hosts:

- Avoid restoring more than 4-8 VMs simultaneously from the same checkpoint (I/O contention on overlay copy)
- For batch restore, stagger launches or use faster storage (NVMe, tmpfs for overlay copies)
- The overlay copy is the bottleneck; CH state restore is memory-mapped and fast

**Prefault**: Cloud Hypervisor's `--restore` does not prefault memory pages by default. Pages are faulted on demand from the snapshot file. This gives faster initial restore but may cause latency spikes during early guest execution. If predictable latency is required, a future `--prefault` option can be added (not in Phase 2 scope).

---

## 11. Checkpoint Path Comparison

The following table summarizes the differences between the two checkpoint/restore paths:

| Aspect | Cloud Image (qcow2) | OCI VM Image (overlayfs) |
|--------|---------------------|--------------------------|
| Disk state captured | qcow2 overlay file copy | overlayfs upperdir directory copy |
| CH snapshot contents | Memory + device + vCPU state | Memory + device + vCPU state |
| Same-host required | Yes (qcow2 backing-file contains absolute path to base image) | No (overlayfs lowerdir paths are remappable at mount time) |
| Restore dependencies | Base qcow2 image at same absolute path | OCI image layers extracted on restore host (same manifest digest) |
| Disk state size model | Sparse qcow2 file (block-level COW) | Directory tree (file-level COW) |
| Restore disk setup | Copy overlay.qcow2 to new VM directory | Mount overlayfs with preserved upperdir, spawn virtiofsd |
| Cross-host portability | Requires `qemu-img rebase` (Phase 3) | Supported (remap lowerdir paths to local layer extraction) |
| virtiofsd required | No | Yes (must be spawned before CH restore) |

Both paths share the same CH snapshot mechanism, CLI commands, checkpoint metadata schema, GC integration, and name resolution. The `checkpoint_type` field in `checkpoint.json` (`"qcow2"` or `"overlayfs"`) determines which path is used during restore.

---

## 12. Error Handling

### 12.1 Checkpoint Error Cases

| Condition | Error Message | Exit Code |
|-----------|--------------|-----------|
| VM not paused (no --live) | `VM must be PAUSED to checkpoint (current state: RUNNING). Use --live for auto-pause` | 1 |
| VM has passthrough devices | `cannot checkpoint VM with passthrough devices (2 device(s) attached)` | 1 |
| Overlay copy failed | `copy overlay: <err>` | 1 |
| CH snapshot failed | `CH snapshot: CH returned 500: <err>` | 1 |
| Checkpoint name already exists | `checkpoint name "golden" already exists` | 1 |
| Disk space insufficient | `copy overlay: no space left on device` | 1 |

### 12.2 Restore Error Cases

| Condition | Error Message | Exit Code |
|-----------|--------------|-----------|
| Checkpoint not found | `checkpoint not found: <ref>` | 1 |
| Base image garbage collected | `base image for checkpoint not found. Re-pull: cocoon image pull <ref>` | 1 |
| Firmware version mismatch | `firmware version mismatch: checkpoint used X, host has Y. Use --force to override` | 1 |
| CH version mismatch | WARNING: proceeds with warning | 0 |
| Overlay copy failed | `copy overlay from checkpoint: <err>` | 1 |
| CH restore launch failed | `launch restore: <err>` | 1 |
| VM name already exists | `VM name "agent-001" already exists` | 1 |

### 12.3 Cleanup on Failure

Both checkpoint creation and restore use a `defer`-based cleanup pattern. If any step fails, all partially-created artifacts are removed:

- Checkpoint creation: removes the checkpoint directory (via deferred `os.RemoveAll`). If the snapshot succeeded but a later step fails (reference pinning or index registration), the checkpoint-index entry is explicitly rolled back before the deferred directory cleanup runs. The `checkpoint-index.lock` file is released by the normal lock-release path in `registerCheckpointName`/`unregisterCheckpointName`; it is not removed on failure because it is a shared flock file reused across operations.
- Restore: removes the new VM directory, unpins references, unregisters name

---

## 13. Security

### 13.1 Memory Contents in Checkpoints

A checkpoint contains the complete contents of VM memory, which may include encryption keys, tokens, passwords, and sensitive application data. Checkpoint directories inherit root-only filesystem permissions.

### 13.2 Access Control

All checkpoint operations require root privileges. No separate checkpoint-level ACL is introduced.

### 13.3 Checkpoint Integrity

Checkpoints include SHA-256 checksums for the CH state files and overlay:

```go
type CheckpointMetadata struct {
    // ...
    CHSnapshotChecksum string `json:"ch_snapshot_checksum,omitempty"` // SHA-256
    OverlayChecksum    string `json:"overlay_checksum,omitempty"`     // SHA-256
}
```

Restore validates these checksums before launching the CH process.

### 13.4 Encryption at Rest (Future)

A future enhancement could encrypt checkpoint state files using a host-level key. This is explicitly deferred beyond Phase 2.

### 13.5 Pre-Checkpoint Guest Preparation

A checkpoint captures the complete contents of VM memory, including sensitive data that may be present in the guest at checkpoint time. When creating golden checkpoints intended for repeated restore (especially across different workloads or users), the following preparation is recommended before taking the checkpoint:

**Recommended cleanup before `cocoon checkpoint create`**:

1. **Clear temporary files**: `rm -rf /tmp/* /var/tmp/*`
2. **Clear shell history**: `history -c && rm -f ~/.bash_history ~/.zsh_history`
3. **Clear package cache**: `apt-get clean` or `dnf clean all`
4. **Flush DNS cache**: `systemd-resolve --flush-caches` (if applicable)
5. **Remove SSH host keys** (regenerated on next boot): `rm -f /etc/ssh/ssh_host_*`
6. **Truncate machine-id** (regenerated by systemd on next boot): `truncate -s 0 /etc/machine-id`
7. **Revoke any temporary tokens or credentials** obtained during setup
8. **Sync filesystem**: `sync` (ensures overlay captures all writes)

**Security warning**: Do NOT distribute checkpoints across trust boundaries. A checkpoint contains the full memory of the VM, which may include encryption keys, API tokens, passwords, and other secrets even after the cleanup steps above. Checkpoints are intended for single-host or same-trust-domain use.

**Future**: A pre-checkpoint hook mechanism (Phase 3) will allow users to register cleanup scripts that run automatically inside the guest before checkpoint capture.

---

## 14. Testing

### 14.1 Unit Tests

```go
func TestCheckpointMetadataSchema(t *testing.T) {
    meta := &types.CheckpointMetadata{
        CheckpointID:  "ckpt-01HY...",
        Name:          "test",
        SourceVMID:    "vm-01HX...",
        SchemaVersion: 1,
    }

    data, err := json.Marshal(meta)
    if err != nil {
        t.Fatal(err)
    }

    var decoded types.CheckpointMetadata
    if err := json.Unmarshal(data, &decoded); err != nil {
        t.Fatal(err)
    }

    if decoded.CheckpointID != meta.CheckpointID {
        t.Errorf("expected %s, got %s", meta.CheckpointID, decoded.CheckpointID)
    }
}

func TestCheckpointRefResolution(t *testing.T) {
    tests := []struct {
        ref     string
        wantID  string
        wantErr bool
    }{
        {"ckpt-01HY...", "ckpt-01HY...", false},  // Direct ID
        {"my-checkpoint", "ckpt-01HY...", false},   // Name lookup
        {"nonexistent", "", true},                    // Not found
    }

    for _, tt := range tests {
        // ... test with mock index ...
    }
}

func TestCheckpointRejectsPassthroughDevices(t *testing.T) {
    mgr := newTestCheckpointManager(t)
    vmID := createTestVMWithDevice(t)

    _, err := mgr.Checkpoint(context.Background(), vmID, &CheckpointOptions{Live: true})
    if err == nil {
        t.Fatal("expected error for VM with passthrough devices")
    }
    if !strings.Contains(err.Error(), "passthrough devices") {
        t.Errorf("unexpected error: %v", err)
    }
}
```

### 14.2 Integration Tests

```go
func TestCheckpointRestoreCycle(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping integration test")
    }

    // 1. Create and start a VM.
    vmID := createAndStartTestVM(t)

    // 2. Checkpoint with --live.
    ckptMeta, err := ckptMgr.Checkpoint(ctx, vmID, &CheckpointOptions{
        Live: true,
        Name: "test-ckpt",
    })
    if err != nil {
        t.Fatalf("checkpoint failed: %v", err)
    }

    // 3. Verify checkpoint files exist.
    assertFileExists(t, cfg.CheckpointMetadataPath(ckptMeta.CheckpointID))
    assertFileExists(t, cfg.CheckpointOverlayPath(ckptMeta.CheckpointID))
    assertDirExists(t, cfg.CheckpointSnapshotDir(ckptMeta.CheckpointID))

    // 4. Verify source VM is still running.
    meta, _ := vmMgr.LoadMetadata(vmID)
    if meta.State != string(types.VMStateRunning) {
        t.Errorf("expected source VM still RUNNING, got %s", meta.State)
    }

    // 5. Restore from checkpoint.
    newCfg, err := ckptMgr.Restore(ctx, ckptMeta.CheckpointID, &RestoreOptions{
        Name: "restored-vm",
    })
    if err != nil {
        t.Fatalf("restore failed: %v", err)
    }

    // 6. Verify restored VM is running.
    newMeta, _ := vmMgr.LoadMetadata(newCfg.VMID)
    if newMeta.State != string(types.VMStateRunning) {
        t.Errorf("expected restored VM RUNNING, got %s", newMeta.State)
    }

    // 7. Verify restore provenance.
    if newMeta.RestoredFromCheckpoint != ckptMeta.CheckpointID {
        t.Errorf("expected restored_from_checkpoint = %s", ckptMeta.CheckpointID)
    }

    // 8. Cleanup.
    deleteTestVM(t, newCfg.VMID)
    deleteTestVM(t, vmID)
    ckptMgr.Delete(ctx, ckptMeta.CheckpointID)
}

func TestWarmStartPerformance(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping performance test")
    }

    // 1. Create golden checkpoint.
    // 2. Measure restore time over N iterations.
    // 3. Assert restore time is under 1 second.
}
```

### 14.3 GC Integration Tests

```go
func TestCheckpointPinsBaseImage(t *testing.T) {
    // 1. Create VM, checkpoint.
    // 2. Delete source VM.
    // 3. Run GC -> base image must NOT be collected (checkpoint holds reference).
    // 4. Delete checkpoint.
    // 5. Run GC -> base image can now be collected.
}
```

---

## 15. Implementation Plan

### Phase 2.1: Pause and Resume (prerequisite)

See [13-pause-resume.md](./13-pause-resume.md). Must be completed first.

**Estimated effort**: 1-2 weeks

### Phase 2.2: Core Checkpoint and Restore (2-3 weeks)

1. `types/checkpoint.go`: New file with `CheckpointMetadata` type
2. `hypervisor/hypervisor.go`: Add `SnapshotVM()`, `LaunchRestore()` to Client
3. `hypervisor/cloudhypervisor/client.go`: Implement snapshot and restore methods
4. `checkpoint/checkpoint.go`: New package with `CheckpointManager` interface
5. `checkpoint/engine/manager.go`: Checkpoint manager implementation
6. `config/config.go`: Add checkpoint path helpers
7. `types/metadata.go`: Add checkpoint provenance fields
8. `cmd/cocoon/checkpoint.go`: CLI commands (checkpoint create/list/inspect/delete)
9. `cmd/cocoon/restore.go`: CLI restore command
10. Storage: checkpoint directory creation, overlay copy, metadata persistence
11. GC: checkpoint reference tracking in `references.json`
12. Tests for full checkpoint/restore cycle

### Phase 2.3: Warm Start Polish (1 week)

1. `cmd/cocoon/create.go`: Add `--from-checkpoint` flag (delegates to restore)
2. Documentation and examples for golden checkpoint workflow
3. Performance benchmarking: cold boot vs warm start
4. Integration tests for batch restore scenario

### Phase 2.4: Compression and Integrity (1 week)

1. `checkpoint/engine/compress.go`: zstd compression/decompression
2. `checkpoint/engine/integrity.go`: SHA-256 checksum generation and validation
3. `cmd/cocoon/checkpoint.go`: Add `--compress` flag
4. `cocoon checkpoint inspect`: Detailed size and compression information
5. Performance optimization: parallel overlay copy and state snapshot

### Total Estimated Effort: 5-7 weeks (including pause/resume prerequisite)

---

## 16. Unresolved Questions

1. **Overlay copy vs. qcow2 internal snapshot**: Should we use a full file copy or leverage qcow2's internal snapshot feature? Full copy is simpler and more portable but uses more disk space. Decision: Use full copy; revisit if storage costs become a concern.

2. **Checkpoint-to-checkpoint deduplication**: Can we deduplicate across multiple checkpoints of the same VM? Decision: Defer beyond Phase 2.

3. **Maximum checkpoint size**: Should there be a configurable limit? A VM with 64GB memory produces a 64GB+ checkpoint. Decision: Warn above a threshold (configurable) but do not enforce a hard limit.

4. **Restore to different resources**: Can a checkpoint from a 2-vCPU/1GB VM be restored as 4-vCPU/2GB? CH may support this for memory (adding pages) but not for reducing. Decision: Require identical resource configuration; investigate relaxation later.

5. **Network identity on restore**: Should the restored VM get the same or different MAC address? Decision: Restored VMs receive a new VM ID and thus a new deterministic MAC address (MAC = hash(vmID, ifName)), different from the source VM. The MAC is stable across subsequent restarts of the same restored VM instance. This ensures uniqueness on the network while preserving DHCP lease stability for each individual restored VM.

6. **Checkpoint portability**: Can a checkpoint be transferred to a different host? Requires same CH version, firmware, architecture, and base image. Decision: Focus on single-host; cross-host transfer is future scope.

7. **Concurrent checkpoints**: Should multiple checkpoints of the same VM be allowed? Decision: Serialize per-VM (VM must be PAUSED). Concurrent checkpoints of different VMs are naturally supported.

8. **Checkpoint naming conflicts**: Same rule as VM names: reject with error. Delete existing checkpoint first or choose a different name.

---

## 17. Cross-References

### 17.1 Related Cocoon Documents

- [03-hypervisor-integration.md](./03-hypervisor-integration.md): CH process lifecycle. Restore launches a new CH process with `--restore` instead of `vm.create`/`vm.boot`.
- [05-storage-management.md](./05-storage-management.md): Storage layout, COW, and GC. Checkpoints add a new directory under `/var/lib/cocoon/checkpoints/` and pin base image references.
- [06-concurrency.md](./06-concurrency.md): Lock hierarchy. Checkpoint index lock at Level 2.
- [07-vm-lifecycle.md](./07-vm-lifecycle.md): State machine. Checkpoint requires PAUSED state.
- [09-cli-design.md](./09-cli-design.md): CLI command structure. New `cocoon checkpoint` and `cocoon restore` commands.

### 17.2 Interaction with Other Phase 2 Features

- **Pause/Resume** ([13-pause-resume.md](./13-pause-resume.md)): Pause is a prerequisite for checkpoint. The `--live` flag automates the pause/checkpoint/resume cycle.
- **Console** ([12-console.md](./12-console.md)): On restore, a new PTY is allocated. `cocoon console` discovers paths dynamically, so no special handling is needed.
- **Device Passthrough** ([14-device-passthrough.md](./14-device-passthrough.md)): VMs with passthrough devices cannot be checkpointed. CH does not support snapshotting VFIO device state. `cocoon checkpoint create` returns a clear error.

### 17.3 Combined CHVMConfig Target

```go
type CHVMConfig struct {
    CPUs    CHCPUConfig      `json:"cpus"`
    Memory  CHMemoryConfig   `json:"memory"`
    Disks   []CHDiskConfig   `json:"disks,omitempty"`
    Fs      []CHFsConfig     `json:"fs,omitempty"`       // Volume passthrough (virtio-fs)
    Serial  CHSerialConfig   `json:"serial"`
    Console CHConsoleConfig  `json:"console"`             // Console: mode "Pty"
    Devices []CHDeviceConfig `json:"devices,omitempty"`   // Device passthrough (VFIO)
}
```

Note: Checkpoint/restore does not add fields to `CHVMConfig`. It uses the existing config for the source VM and the `--restore` CLI flag for the CH process.

### 17.4 External References

- Cloud Hypervisor REST API: https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/vmm/src/api/openapi/cloud-hypervisor.yaml (`vm.snapshot`, `vm.restore`)
- Cloud Hypervisor Snapshot Documentation: https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/snapshot_restore.md
- Firecracker Snapshots: https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/snapshot-support.md (prior art)
- CRIU: https://criu.org/ (container-level C/R, different scope but similar concepts)

---

**End of VM Warm Start Design Document v1.1**

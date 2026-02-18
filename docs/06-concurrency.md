# Concurrency Design

**Version**: 1.0
**Status**: Implemented
**Phase**: Phase 1
**Last Updated**: 2026-02-18

## Overview

This document describes the concurrency control mechanisms for the Cocoon AI Agent Sandbox. The design ensures safe concurrent operations across VM creation, deletion, garbage collection, and base image updates while maintaining data integrity and preventing deadlocks.

## Core Principles

1. **Correctness over performance**: Always prioritize data integrity
2. **Lock ordering discipline**: Prevent deadlocks through strict hierarchy
3. **Atomic operations**: Ensure reference counting and metadata updates are atomic
4. **Crash consistency**: Design for graceful recovery from crashes
5. **Lock granularity**: Use fine-grained locks where possible to maximize concurrency

## Lock Hierarchy

To prevent deadlocks, all locks MUST be acquired in this order:

```
Level 1: GC Lock (global)
    ↓
Level 2: Reference Counter Lock (global)
    ↓
Level 2: Name Index Lock (global, same level as references — never held together)
    ↓
Level 2: OCI Build Txn Lock (global parent lock for OCI cross-index updates) — Implemented, oci/store.go
    ↓
Level 2: OCI Build Tag Lock (global, acquired under OCI Build Txn Lock for tag index mutations) — Implemented, oci/store.go
    ↓
Level 2: OCI Layer Refs Lock (global, acquired under OCI Build Txn Lock for blob-ref mutations) — Implemented, oci/layerrefs.go
    ↓
Level 2: OCI Reference Lock (global, same level — never held with references or name-index) — Phase 2, docs/04.1-oci-vm-images.md
    ↓
Level 2: Checkpoint Index Lock (global, same level — never held with name-index or references) — Phase 2, docs/15-warm-start.md
    ↓
Level 3: Image Conversion Lock (per-checksum)
    ↓
Level 3: OCI Cache Lock (per-manifest-digest) — Phase 2, docs/04.1-oci-vm-images.md
    ↓
Level 4: VM Metadata Lock (per-VM)
    ↓
Level 5: Checkpoint Lock (per-VM) — Phase 2, docs/15-warm-start.md
    ↓
Level 5: Network Lock (per-VM) — Phase 2, docs/16-networking.md
    ↓
Level 5: Console Lock (per-VM) — Phase 2, docs/12-console.md
    ↓
Level 6: dnsmasq Lock (global) — Phase 2, docs/16-networking.md
```

**Lock File Locations**:
- GC Lock: `/var/lib/cocoon/db/gc.lock`
- Reference Counter Lock: `/var/lib/cocoon/db/references.lock`
- Name Index Lock: `/var/lib/cocoon/db/name-index.lock`
- Image Conversion Lock: `/var/lib/cocoon/cache/locks/{checksum}_{arch}.lock`
- VM Metadata Lock: `/var/lib/cocoon/vms/{vm-id}/metadata.lock`
- Checkpoint Lock (Phase 2): `/var/lib/cocoon/vms/{vm-id}/checkpoint.lock`
- Network Lock (Phase 2): `/run/cocoon/vms/{vm-id}/network.lock`
- Console Lock (Phase 2): `/run/cocoon/vms/{vm-id}/console.lock`
- OCI Build Txn Lock: `/var/lib/cocoon/db/oci-build-txn.lock`
- OCI Build Tag Lock: `/var/lib/cocoon/db/oci-build-tags.lock`
- OCI Layer Refs Lock: `/var/lib/cocoon/db/oci-layer-refs.lock`
- OCI Reference Lock (Phase 2): `/var/lib/cocoon/db/oci-references.lock`
- Checkpoint Index Lock (Phase 2): `/var/lib/cocoon/checkpoints/checkpoint-index.lock`
- OCI Cache Lock (Phase 2): `/var/lib/cocoon/cache/oci/{digest}.lock`
- dnsmasq Lock (Phase 2): `/run/cocoon/dnsmasq/dnsmasq.lock`

**Name Index Lock Notes**:
- `name-index.json` is a derived cache (can be rebuilt from scanning `config.json` files)
- The lock is at Level 2 (same as references.lock); these two locks are NEVER held simultaneously
- Updates use the same flock + atomic-write pattern as references.json
- Operations: create (add name→vm_id), delete (remove mapping), reconcile (rebuild from scratch)

**Rules**:
- Never acquire a higher-level lock while holding a lower-level lock
- Always release locks in reverse order of acquisition
- If you need multiple locks at the same level, acquire them in sorted order by ID
- All locks are file-based (flock) for cross-process safety

## 1. Image Conversion Lock

### Problem

When multiple `cocoon create` commands run concurrently with the same OCI image:

```bash
# Terminal 1
$ cocoon create myorg/ubuntu-bootable:22.04 --name vm-001

# Terminal 2 (at the same time)
$ cocoon create myorg/ubuntu-bootable:22.04 --name vm-002

# Terminal 3 (at the same time)
$ cocoon create myorg/ubuntu-bootable:22.04 --name vm-003
```

Without locking, all three would:
1. Check cache → miss (image not yet cached)
2. Pull from registry → 3 parallel downloads
3. Convert to qcow2 → 3 conversions
4. Save to cache → race condition, corruption

### Solution: File-Based Per-Image Lock

Lock on the image checksum using file locks (flock) to ensure cross-process safety.

The implementation uses the `lock.Locker` interface (`lock/lock.go`) with an
`flock(2)` backend (`lock/flock/flock.go`). Each image's lock path is derived
from the content-addressed base key via `CocoonConfig.ConversionLockPath(baseKey)`:

```go
// lock/lock.go — interface for cross-process mutual exclusion.
package lock

type Locker interface {
    Lock() error             // Acquire exclusive lock (blocks).
    TryLock() (bool, error)  // Non-blocking attempt; false if held.
    Unlock() error           // Release lock. File is NOT deleted.
    Path() string            // Lock file path.
}
```

```go
// lock/flock/flock.go — flock(2) implementation of lock.Locker.
package flock

func New(path string) lock.Locker  // Create a Locker for the given path.
```

```go
// config/config.go — per-image lock path helper.
func (c *CocoonConfig) ConversionLockPath(baseKey string) string {
    return filepath.Join(c.RootDir, "cache", "locks", baseKey+".lock")
}
```

Usage (from `image/pipeline/manager.go`):

```go
lockPath := m.cfg.ConversionLockPath(baseKey)
fl := flock.New(lockPath)
if err := fl.Lock(); err != nil { return err }
defer fl.Unlock()
// ... convert image inside lock ...
```

### Usage Pattern

The `Prepare()` function has **two distinct concurrency paths** depending on image type:

#### OCI Images: Pull + Convert Inside Lock

```go
// Simplified view of image/pipeline/manager.go prepareOCI().
func (m *manager) prepareOCI(ctx context.Context, ref string) (*ImageIdentity, string, error) {
    // 1. Identify (skopeo inspect) — cheap, no lock needed.
    identity, err := identifyOCIPlatform(ctx, ref)
    baseKey := identity.BaseKey()
    basePath := m.cfg.BaseImagePath(baseKey)

    // 2. Fast path: check if already cached (no lock for read).
    if _, err := os.Stat(basePath); err == nil {
        return identity, basePath, nil
    }

    // 3. Acquire per-image conversion lock (Level 3).
    lockPath := m.cfg.ConversionLockPath(baseKey)
    fl := flock.New(lockPath)
    fl.Lock()
    defer fl.Unlock()

    // 4. Double-check cache after acquiring lock.
    if _, err := os.Stat(basePath); err == nil {
        return identity, basePath, nil
    }

    // 5. Pull (buildah) + mount + convert — ALL inside lock.
    //    Only one process pulls and converts per unique image.
    pullOCIImage(ctx, identity)
    convertOCIImage(ctx, identity, basePath, baseKey) // rename + chmod 0444

    return identity, basePath, nil
}
```

#### URL/Local Images: Pull Outside Lock, Convert Inside Lock

```go
// Simplified view of image/pipeline/manager.go Prepare() for URL/local files.
func (m *manager) Prepare(ctx context.Context, ref string) (*ImageIdentity, string, error) {
    // 1. Pull determines identity (checksum from download/file) — NO LOCK.
    //    For URL: downloads to temp file, computes SHA-256.
    //    For local file: reads file, computes SHA-256.
    //    Concurrent callers MAY redundantly download the same URL.
    identity, err := m.Pull(ctx, ref)
    baseKey := identity.BaseKey()
    basePath := m.cfg.BaseImagePath(baseKey)

    // 2. Fast path: check cache before converting.
    if _, err := os.Stat(basePath); err == nil {
        return identity, basePath, nil
    }

    // 3. Convert acquires its own per-image lock (Level 3).
    //    Lock path: ConversionLockPath(baseKey) = {ConversionLockDir}/{checksum}_{arch}.lock
    //    Inside lock: double-check cache, format-detect, qemu-img convert, rename + chmod 0444.
    basePath, err = m.Convert(ctx, identity)

    return identity, basePath, nil
}
```

**Why Pull is outside the lock for URL/local**: The baseKey (content-addressed checksum) is unknown until the file is downloaded and hashed. The conversion lock is keyed by baseKey, so it cannot be acquired before Pull completes. Redundant downloads are harmless — the lock ensures only one conversion writes the final base image.

### Behavior

- **OCI**: Pull + convert both inside per-image lock. Only one process pulls per unique image.
- **URL/local**: Pull may be redundant across concurrent callers. Convert (inside lock) deduplicates: first writer wins, subsequent waiters find cache hit at step 4/2.
- **Both paths**: Base image is chmod 0444 after atomic rename (immutable for COW overlays).
- **Cross-process safety**: Works across multiple CLI invocations (not just threads)

### Crash Recovery

File locks (flock) are automatically released when:
- The process exits normally
- The process crashes (kernel releases lock)
- The file descriptor is closed

This means if a process crashes during image conversion:
1. The lock is automatically released by the kernel
2. The next process acquires the lock
3. Sees incomplete cache file (or no file)
4. Retries the conversion

No manual cleanup of stale locks is needed.

## 2. Reference Counter Atomicity

### Problem

`references.json` tracks which VMs use which base images, keyed by
content-addressed identity (`{checksum}_{arch}`), not by absolute path:

```json
{
  "abc123def456a7b8_amd64": {
    "path": "/var/lib/cocoon/cache/images/abc123def456a7b8_amd64.qcow2",
    "digest_full": "abc123def456a7b8901234567890abcdef1234567890abcdef1234567890abcd",
    "source_ref": "myorg/ubuntu-bootable:22.04",
    "refs": ["vm-001", "vm-002"],
    "created_at": "2026-02-12T10:00:00Z"
  },
  "f7e8d9c0b1a2e3f4_amd64": {
    "path": "/var/lib/cocoon/cache/images/f7e8d9c0b1a2e3f4_amd64.qcow2",
    "digest_full": "f7e8d9c0b1a2e3f4567890abcdef1234567890abcdef1234567890abcdef1234",
    "source_ref": "myorg/fedora-bootable:39",
    "refs": ["vm-003"],
    "created_at": "2026-02-12T11:00:00Z"
  }
}
```

Concurrent operations can corrupt this file:
- Process A reads, adds vm-004, writes
- Process B reads (old data), removes vm-003, writes
- Result: Lost update (vm-004 addition is lost)

### Solution: File Locking + Atomic Write

Use POSIX file locking (`flock`) for mutual exclusion and atomic write pattern for durability:

```go
package storage

import (
    "encoding/json"
    "os"
    "path/filepath"
    "syscall"
    "time"
)

// ReferenceCounter tracks base image usage with atomic operations.
// Keys are content-addressed: {checksum}_{arch} (see 05-storage-management.md).
type ReferenceCounter struct {
    storageDir string
    lockFile   string
    dataFile   string
}

func NewReferenceCounter(storageDir string) *ReferenceCounter {
    return &ReferenceCounter{
        storageDir: storageDir,
        lockFile:   filepath.Join(storageDir, "db", "references.lock"),
        dataFile:   filepath.Join(storageDir, "db", "references.json"),
    }
}

// RefEntry represents a single image reference entry
type RefEntry struct {
    Path       string   `json:"path"`
    DigestFull string   `json:"digest_full"`
    SourceRef  string   `json:"source_ref"`
    Refs       []string `json:"refs"`
    CreatedAt  string   `json:"created_at"`
}

// RefData represents the reference data structure.
// Keys are content-addressed: {checksum}_{arch} (e.g., "a1b2c3d4e5f6a7b8_amd64").
type RefData map[string]*RefEntry

// updateReferences performs an atomic update operation.
// The imageKey parameter uses the content-addressed format: {checksum}_{arch}.
func (rc *ReferenceCounter) updateReferences(op func(RefData) error) error {
    // 1. Acquire exclusive file lock
    lockFile, err := os.OpenFile(rc.lockFile, os.O_RDWR|os.O_CREATE, 0644)
    if err != nil {
        return err
    }
    defer lockFile.Close()

    // Use flock for advisory locking (portable across NFS)
    err = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX)
    if err != nil {
        return err
    }
    defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)

    // 2. Read current references (keyed by {checksum}_{arch})
    refs := make(RefData)

    data, err := os.ReadFile(rc.dataFile)
    if err != nil && !os.IsNotExist(err) {
        return err
    }
    if len(data) > 0 {
        if err := json.Unmarshal(data, &refs); err != nil {
            return err
        }
    }

    // 3. Apply operation
    if err := op(refs); err != nil {
        return err
    }

    // 4. Atomic write: write to temp file, fsync, rename
    tempFile := rc.dataFile + ".tmp"

    jsonData, err := json.MarshalIndent(refs, "", "  ")
    if err != nil {
        return err
    }

    // Write to temp file
    f, err := os.OpenFile(tempFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
    if err != nil {
        return err
    }

    _, err = f.Write(jsonData)
    if err != nil {
        f.Close()
        return err
    }

    // Fsync to ensure data is on disk
    if err := f.Sync(); err != nil {
        f.Close()
        return err
    }
    f.Close()

    // Atomic rename (POSIX guarantees atomicity)
    if err := os.Rename(tempFile, rc.dataFile); err != nil {
        return err
    }

    return nil
}

// ParseBaseKey splits base_key into (checksum, arch).
// E.g., "a1b2c3d4e5f6a7b8_amd64" → ("a1b2c3d4e5f6a7b8", "amd64").
func ParseBaseKey(baseKey string) (checksum, arch string, err error) {
    parts := strings.SplitN(baseKey, "_", 2)
    if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
        return "", "", fmt.Errorf("invalid base_key format: %q", baseKey)
    }
    return parts[0], parts[1], nil
}

// AddReference adds a VM reference to a base image.
// baseKey is the content-addressed key: {checksum_16}_{arch}.
// digestFull is the full 64-char SHA-256 hex for collision detection.
// sourceRef is the original image reference (OCI ref / URL / path).
func (rc *ReferenceCounter) AddReference(baseKey, vmID, digestFull, sourceRef string) error {
    return rc.updateReferences(func(refs RefData) error {
        entry := refs[baseKey]
        if entry != nil {
            // Collision check: same key but different full digest
            if digestFull != "" && entry.DigestFull != "" && entry.DigestFull != digestFull {
                return fmt.Errorf("checksum collision: base_key %s already maps to a different image "+
                    "(stored: %s…, incoming: %s…)", baseKey, entry.DigestFull[:16], digestFull[:16])
            }
        } else {
            entry = &RefEntry{
                Path:       filepath.Join(rc.storageDir, "cache", "images", baseKey+".qcow2"),
                DigestFull: digestFull,
                SourceRef:  sourceRef,
                CreatedAt:  time.Now().Format(time.RFC3339),
            }
            refs[baseKey] = entry
        }
        // Check if already exists
        for _, vm := range entry.Refs {
            if vm == vmID {
                return nil // Already referenced
            }
        }
        entry.Refs = append(entry.Refs, vmID)
        return nil
    })
}

// RemoveReference removes a VM reference from a base image.
// baseKey is the content-addressed key: {checksum_16}_{arch}.
func (rc *ReferenceCounter) RemoveReference(baseKey, vmID string) error {
    return rc.updateReferences(func(refs RefData) error {
        entry := refs[baseKey]
        if entry == nil {
            return nil
        }
        newRefs := []string{}
        for _, vm := range entry.Refs {
            if vm != vmID {
                newRefs = append(newRefs, vm)
            }
        }
        if len(newRefs) > 0 {
            entry.Refs = newRefs
        } else {
            delete(refs, baseKey)
        }
        return nil
    })
}

// GetReferences returns all VMs referencing a base image.
// baseKey is the content-addressed key: {checksum_16}_{arch}.
func (rc *ReferenceCounter) GetReferences(baseKey string) ([]string, error) {
    var result []string

    err := rc.updateReferences(func(refs RefData) error {
        entry := refs[baseKey]
        if entry != nil {
            result = make([]string, len(entry.Refs))
            copy(result, entry.Refs)
        }
        return nil
    })

    return result, err
}

// IsReferenced checks if a base image has any references.
// baseKey is the content-addressed key: {checksum_16}_{arch}.
func (rc *ReferenceCounter) IsReferenced(baseKey string) (bool, error) {
    var referenced bool

    err := rc.updateReferences(func(refs RefData) error {
        entry := refs[baseKey]
        referenced = entry != nil && len(entry.Refs) > 0
        return nil
    })

    return referenced, err
}
```

### Why This Works

1. **File lock**: Only one process can hold the lock at a time
2. **Read-modify-write**: All operations see consistent state
3. **Atomic rename**: POSIX guarantees `rename()` is atomic
4. **fsync**: Ensures durability before rename
5. **Temp file**: If process crashes, original file is intact

### Lock Timing

The file lock is held for the entire read-modify-write cycle:
- Acquire lock → Read → Modify → Write temp → Fsync → Rename → Release lock
- Typical duration: 1-5ms (fast enough for concurrent operations)

## 3. Concurrent Scenarios

### Scenario A: 50 Concurrent Creates with Same Image

**Command**:
```bash
# Run 50 times in parallel
parallel -j 50 cocoon create myorg/ubuntu-bootable:22.04 --name vm-{} ::: {001..050}
```

**Expected Behavior**:

1. **Image Conversion** (Level 2 Lock):
   - Process 1: Acquires image lock, pulls and converts myorg/ubuntu-bootable:22.04
   - Processes 2-50: Wait on image lock
   - Process 1: Completes conversion, releases lock
   - Processes 2-50: Acquire lock in sequence, see cached image, immediately return

2. **Overlay Creation** (No Lock Needed):
   - All 50 processes create overlays in parallel
   - Each writes to different file: `vms/vm-001/overlay.qcow2`, `vms/vm-002/overlay.qcow2`, etc.
   - No contention

3. **Reference Updates** (Level 4 Lock):
   - Processes serialize on `references.lock`
   - Each adds its VM ID to the base image's reference list
   - Order doesn't matter (set semantics)
   - Each update takes ~1ms → total 50ms for all updates

**Result**:
- 1 image download and conversion
- 50 overlay images created
- All 50 references recorded correctly
- Total time: ~30 seconds (dominated by image pull and conversion)

**Verification**:
```bash
# Check references
$ cat /var/lib/cocoon/db/references.json
{
  "abc123def456a7b8_amd64": {
    "path": "/var/lib/cocoon/cache/images/abc123def456a7b8_amd64.qcow2",
    "refs": ["vm-001", "vm-002", ..., "vm-050"],
    "created_at": "2026-02-12T10:00:00Z"
  }
}

# Check disk usage (1 base + 50 overlays, not 50 full images)
$ du -sh /var/lib/cocoon/cache/images/abc123def456a7b8_amd64.qcow2
5.2G    abc123def456a7b8_amd64.qcow2

$ du -sh /var/lib/cocoon/vms/vm-*/overlay.qcow2
196K    vm-001/overlay.qcow2
196K    vm-002/overlay.qcow2
...
196K    vm-050/overlay.qcow2
```

### Scenario B: Delete VM While GC is Running

**Command**:
```bash
# Terminal 1: Run GC
$ cocoon gc

# Terminal 2: Delete VM (during GC)
$ cocoon delete vm-042
```

**Expected Behavior**:

1. **GC Process**:
   - Acquires reference lock
   - Reads references: `{"abc123def456a7b8_amd64": {"refs": ["vm-042"], ...}}`
   - Sees vm-042 references abc123def456a7b8_amd64
   - Skips deletion (image is referenced)
   - Releases lock
   - Moves to next image

2. **Delete Process** (runs after GC releases lock):
   - Acquires reference lock
   - Removes vm-042 from references
   - Removes empty entry for abc123def456a7b8_amd64
   - Releases lock
   - Deletes overlay file

3. **Next GC Run**:
   - Acquires reference lock
   - Reads references: no entry for abc123def456a7b8_amd64
   - Sees no references
   - Permanently deletes the unreferenced image immediately

**Race Condition Prevention**:

The critical section is protected by the reference lock:

```go
func (gc *GarbageCollector) CollectImage(imageKey string) error {
    // WRONG: Check references outside lock (race condition)
    refs, _ := gc.refs.GetReferences(imageKey)  // not safe alone
    if len(refs) > 0 {
        return nil // Image in use
    }
    // VM could be deleted here!
    imagePath := filepath.Join(gc.storageDir, "cache", "images", imageKey+".qcow2")
    os.Remove(imagePath) // DANGEROUS

    // CORRECT: Check and delete in same critical section
    return gc.refs.updateReferences(func(refData RefData) error {
        entry := refData[imageKey]
        if entry != nil && len(entry.Refs) > 0 {
            return nil // Image in use, skip
        }

        // No references, safe to delete permanently
        imagePath := filepath.Join(gc.storageDir, "cache", "images", imageKey+".qcow2")
        return os.Remove(imagePath)
    })
}
```

**Result**: No data loss or corruption. Either GC sees the reference and skips deletion, or delete completes first and GC cleans up on next run.

### Scenario C: Create VM While Base Image is Updating

**Command**:
```bash
# Terminal 1: Create VM with bootable OCI image
$ cocoon create myorg/ubuntu-bootable:22.04 --name vm-new

# Terminal 2: Re-pull updated myorg/ubuntu-bootable:22.04 (new version released upstream)
$ cocoon image pull myorg/ubuntu-bootable:22.04
```

**Expected Behavior**:

1. **Existing VMs Using Old Base**:
   ```
   /cache/images/abc123.qcow2  (old myorg/ubuntu-bootable:22.04)
     ├── vm-001/overlay.qcow2
     └── vm-002/overlay.qcow2
   ```

2. **Re-pull** (Terminal 2):
   - Pulls new version of myorg/ubuntu-bootable:22.04
   - Calculates new checksum: `def456`
   - Converts and stores: `/cache/images/def456.qcow2`
   - Updates manifest cache to point myorg/ubuntu-bootable:22.04 → def456
   - Old base `abc123.qcow2` still exists (vm-001, vm-002 reference it)

3. **Create New VM** (Terminal 1):
   - Checks cache for myorg/ubuntu-bootable:22.04
   - Manifest now points to `def456.qcow2`
   - Uses new base image
   - Creates overlay: `vm-new/overlay.qcow2` → `def456.qcow2`

4. **Final State**:
   ```
   /cache/images/abc123.qcow2  (old, still referenced)
     ├── vm-001/overlay.qcow2
     └── vm-002/overlay.qcow2

   /cache/images/def456.qcow2  (new)
     └── vm-new/overlay.qcow2
   ```

5. **Cleanup**:
   - When vm-001 and vm-002 are deleted, references to abc123.qcow2 drop to zero
   - GC can clean up old base image after grace period

**Key Insight**: Multiple versions of the "same" image can coexist because we use content-addressable storage (checksum-based filenames). Old VMs continue using old base, new VMs use new base.

## 4. Crash Consistency

### Problem: Crashes During VM Creation

**Crash Point 1: After Overlay Created, Before Metadata Written**
```
State:
- /vms/vm-042/overlay.qcow2 EXISTS
- /vms/vm-042/config.json MISSING
- references.json: no entry for vm-042
```

**Crash Point 2: After Metadata Written, Before Reference Updated**
```
State:
- /vms/vm-042/overlay.qcow2 EXISTS
- /vms/vm-042/config.json EXISTS
- references.json: no entry for vm-042 (INCONSISTENT!)
```

### Detection: Orphan Scan

Scan for inconsistencies between filesystem and reference counter:

```go
package reconcile

import (
    "encoding/json"
    "os"
    "path/filepath"
)

type Reconciler struct {
    vmDir string
    refs  *storage.ReferenceCounter
}

type Orphan struct {
    Type    string // "overlay", "config", "reference"
    VMID    string
    Path    string
    BaseKey string // Content-addressed key: {checksum_16}_{arch}
}

// ScanOrphans detects inconsistent state
func (r *Reconciler) ScanOrphans() ([]Orphan, error) {
    orphans := []Orphan{}

    // 1. Find VMs with overlay but no config (crash point 1)
    vmDirs, _ := os.ReadDir(r.vmDir)
    for _, vmDir := range vmDirs {
        if !vmDir.IsDir() {
            continue
        }

        vmID := vmDir.Name()
        overlayPath := filepath.Join(r.vmDir, vmID, "overlay.qcow2")
        configPath := filepath.Join(r.vmDir, vmID, "config.json")

        overlayExists := fileExists(overlayPath)
        configExists := fileExists(configPath)

        if overlayExists && !configExists {
            orphans = append(orphans, Orphan{
                Type: "overlay",
                VMID: vmID,
                Path: overlayPath,
            })
        }
    }

    // 2. Find VMs with config but missing from references (crash point 2)
    for _, vmDir := range vmDirs {
        if !vmDir.IsDir() {
            continue
        }

        vmID := vmDir.Name()
        configPath := filepath.Join(r.vmDir, vmID, "config.json")

        if !fileExists(configPath) {
            continue
        }

        // Read config to get base_key
        configData, err := os.ReadFile(configPath)
        if err != nil {
            continue
        }

        var config struct {
            BaseKey string `json:"base_key"` // e.g., "a1b2c3d4e5f6a7b8_amd64"
        }
        json.Unmarshal(configData, &config)

        // Check if VM is in references (keyed by base_key)
        refs, _ := r.refs.GetReferences(config.BaseKey)
        found := false
        for _, ref := range refs {
            if ref == vmID {
                found = true
                break
            }
        }

        if !found {
            orphans = append(orphans, Orphan{
                Type:    "reference",
                VMID:    vmID,
                BaseKey: config.BaseKey,
            })
        }
    }

    return orphans, nil
}

func fileExists(path string) bool {
    _, err := os.Stat(path)
    return err == nil
}
```

### Recovery: Reconcile Command

```go
// Reconcile repairs inconsistent state
func (r *Reconciler) Reconcile(orphans []Orphan) error {
    for _, orphan := range orphans {
        switch orphan.Type {
        case "overlay":
            // Overlay exists but no config -> incomplete creation
            // Safe to permanently delete (VM was never fully created)
            os.RemoveAll(filepath.Dir(orphan.Path)) // Remove VM dir

        case "reference":
            // Config exists but no reference -> add missing reference
            // This completes the incomplete operation
            err := r.refs.AddReference(orphan.BaseKey, orphan.VMID, "", "")
            if err != nil {
                return err
            }
        }
    }
    return nil
}
```

### Usage

```bash
# Detect orphans
$ cocoon doctor
Found 2 orphans:
- vm-042: overlay exists but no config (incomplete creation)
- vm-043: config exists but no reference (interrupted operation)

# Fix automatically
$ cocoon doctor --fix
Cleaned up orphaned overlay: vm-042
Restored missing reference: vm-043

# Verify
$ cocoon doctor
No orphans found. System is consistent.
```

### Crash Recovery Guarantees

1. **Atomicity of reference updates**: Using file lock + atomic write ensures references are never corrupted
2. **Idempotent operations**: Running reconcile multiple times produces same result
3. **Safe defaults**: Only collect resources proven unreferenced by lock-protected checks
4. **Manual override**: Operators can dry-run (`cocoon gc --dry-run`) before deletion

## 5. VM Metadata Locking

### Problem

VM metadata updates (start/stop state, IP addresses, resource usage) can race with delete operations:

```go
// WRONG: Race condition
func (vm *VM) UpdateState(state string) error {
    // Read metadata
    metadata := readMetadata(vm.ID)

    // GAP: VM could be deleted here by another process!

    // Write metadata
    metadata.State = state
    writeMetadata(vm.ID, metadata)  // Could fail if VM deleted
    return nil
}
```

### Solution: Per-VM Metadata Lock

Each VM has its own metadata lock file:

```go
package vm

import (
    "encoding/json"
    "fmt"
    "os"
    "path/filepath"
    "syscall"
)

type MetadataManager struct {
    vmDir string
}

func NewMetadataManager(vmDir string) *MetadataManager {
    return &MetadataManager{vmDir: vmDir}
}

// UpdateMetadata performs atomic metadata update
func (m *MetadataManager) UpdateMetadata(vmID string, updateFn func(*Metadata) error) error {
    // 1. Acquire VM-specific lock
    lockPath := filepath.Join(m.vmDir, vmID, "metadata.lock")
    lockFile, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0644)
    if err != nil {
        return fmt.Errorf("failed to open lock file: %w", err)
    }
    defer lockFile.Close()

    err = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX)
    if err != nil {
        return fmt.Errorf("failed to acquire lock: %w", err)
    }
    defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)

    // 2. Read current metadata
    metadataPath := filepath.Join(m.vmDir, vmID, "metadata.json")
    metadata := &Metadata{}

    data, err := os.ReadFile(metadataPath)
    if err != nil && !os.IsNotExist(err) {
        return err
    }
    if len(data) > 0 {
        if err := json.Unmarshal(data, metadata); err != nil {
            return err
        }
    }

    // 3. Apply update
    if err := updateFn(metadata); err != nil {
        return err
    }

    // 4. Atomic write
    tempPath := metadataPath + ".tmp"
    jsonData, err := json.MarshalIndent(metadata, "", "  ")
    if err != nil {
        return err
    }

    f, err := os.OpenFile(tempPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
    if err != nil {
        return err
    }

    _, err = f.Write(jsonData)
    if err != nil {
        f.Close()
        return err
    }

    if err := f.Sync(); err != nil {
        f.Close()
        return err
    }
    f.Close()

    return os.Rename(tempPath, metadataPath)
}

type Metadata struct {
    VMID      string `json:"vm_id"`
    State     string `json:"state"`
    IPAddress string `json:"ip_address,omitempty"`
    StartedAt string `json:"started_at,omitempty"`
}
```

### Usage Pattern

```go
// Update VM state
err := metadataMgr.UpdateMetadata(vmID, func(m *Metadata) error {
    m.State = "running"
    m.StartedAt = time.Now().Format(time.RFC3339)
    return nil
})

// Update IP address
err := metadataMgr.UpdateMetadata(vmID, func(m *Metadata) error {
    m.IPAddress = "10.0.2.15"
    return nil
})
```

### Crash Recovery

VM metadata locks are automatically released on process crash, just like image conversion locks. If a process crashes during metadata update:

1. Lock is released by kernel
2. Temp file (`.tmp`) remains but original is intact
3. Next operation succeeds normally
4. Temp files can be cleaned up by GC

## Phase 2 Planned Locks

The following locks are planned for Phase 2 features. They are not yet implemented
but are documented here to reserve their positions in the lock hierarchy and ensure
consistency across design documents.

### Checkpoint Lock (Per-VM) — Phase 2

**Purpose**: Protects VM checkpoint/restore operations for warm-start functionality.

- **Lock file**: `/var/lib/cocoon/vms/{vm-id}/checkpoint.lock`
- **Level**: 5 (after VM Metadata Lock)
- **Scope**: Per-VM
- **Held during**: `vm.snapshot` and `vm.restore` API calls
- **Design doc**: [15-warm-start.md](./15-warm-start.md)

The checkpoint lock must be acquired AFTER the VM state lock (Level 4) to prevent
deadlocks. A checkpoint operation first transitions the VM to a CHECKPOINTING state
(under the metadata lock), then acquires the checkpoint lock for the actual
snapshot/restore I/O.

### Network Lock (Per-VM) — Phase 2

**Purpose**: Protects CNI ADD/DEL operations and TAP device lifecycle for a single VM.

- **Lock file**: `/run/cocoon/vms/{vm-id}/network.lock`
- **Level**: 5 (same level as Checkpoint Lock; never held together for the same VM)
- **Scope**: Per-VM
- **Held during**: TAP device creation/deletion and CNI plugin invocation
- **Design doc**: [16-networking.md](./16-networking.md)

The network lock must be acquired AFTER the VM state lock (Level 4). Network setup
occurs after the VM transitions to STARTING state. The lock lives under `/run/cocoon/`
(ephemeral) because network state is reconstructed on reboot.

### dnsmasq Lock (Global) — Phase 2

**Purpose**: Protects dnsmasq configuration updates (DHCP lease file writes and SIGHUP).

- **Lock file**: `/run/cocoon/dnsmasq/dnsmasq.lock`
- **Level**: 6 (after Network Lock)
- **Scope**: Global
- **Held during**: Adding/removing DHCP leases from the lease file and sending SIGHUP to dnsmasq
- **Design doc**: [16-networking.md](./16-networking.md)

The dnsmasq lock must be acquired AFTER the network lock (Level 5). A typical
network setup sequence is: acquire network lock → create TAP device → acquire
dnsmasq lock → add DHCP lease → SIGHUP dnsmasq → release dnsmasq lock → release
network lock. The lock lives under `/run/cocoon/` (ephemeral) because dnsmasq
state is reconstructed on reboot.

### OCI Reference Lock (Global) — Phase 2

**Purpose**: Protects `oci-references.json` (manifest-digest to VM-ID reference tracking).

- **Lock file**: `/var/lib/cocoon/db/oci-references.lock`
- **Level**: 2 (same level as Reference Counter Lock and Name Index Lock — never held together)
- **Scope**: Global
- **Held during**: Adding/removing VM references in `oci-references.json`
- **Design doc**: [04.1-oci-vm-images.md](./04.1-oci-vm-images.md)

The OCI Reference Lock is at the same level as the existing Reference Counter Lock
and Name Index Lock. They protect different files and are never held simultaneously.
This lock follows the same read-modify-write pattern as the Phase 1 references lock.

### OCI Cache Lock (Per-Manifest-Digest) — Phase 2

**Purpose**: Protects `cache/oci/{digest}/` during OCI layer extraction.

- **Lock file**: `/var/lib/cocoon/cache/oci/{digest}.lock`
- **Level**: 3 (same level as Image Conversion Lock)
- **Scope**: Per-manifest-digest
- **Held during**: OCI layer extraction into the cache directory
- **Design doc**: [04.1-oci-vm-images.md](./04.1-oci-vm-images.md)

The per-digest OCI Cache Lock prevents concurrent layer extractions for the same
manifest digest. This follows the same pattern as the per-checksum Image Conversion
Lock for qcow2 images. If GC encounters a held cache lock, it skips that entry.

### Checkpoint Index Lock (Global) — Phase 2

**Purpose**: Protects `checkpoint-index.json` (name to checkpoint-ID mapping).

- **Lock file**: `/var/lib/cocoon/checkpoints/checkpoint-index.lock`
- **Level**: 2 (same level as Name Index Lock and Reference Counter Lock — never held together)
- **Scope**: Global
- **Held during**: Updates to `checkpoint-index.json` (adding/removing checkpoint name mappings)
- **Design doc**: [15-warm-start.md](./15-warm-start.md)

The checkpoint index lock is never held simultaneously with the name index lock or
references lock. It follows the same Level 2 mutual-exclusion pattern used by the
other index/reference locks.

## 6. GC Locking Strategy

### Problem: TOCTOU Race

Classic Time-Of-Check-Time-Of-Use bug:

```go
// WRONG: Race condition
func (gc *GarbageCollector) DeleteUnreferencedImage(image string) error {
    // Check 1: Is image unreferenced?
    refs, _ := gc.refs.GetReferences(image)
    if len(refs) > 0 {
        return nil // In use
    }

    // GAP: Another process could create VM with this image here!

    // Check 2: Delete image
    os.Remove(image) // Could delete image that's now in use!
    return nil
}
```

### Solution: Lock Ordering and Atomic Operations

GC must follow strict lock ordering to prevent deadlocks and races:

```go
package gc

import (
    "fmt"
    "os"
    "path/filepath"
    "strings"
    "syscall"
    "time"
)

type GarbageCollector struct {
    storageDir string
    gcLockFile string
    refs       *ReferenceCounter
}

func NewGarbageCollector(storageDir string, refs *ReferenceCounter) *GarbageCollector {
    return &GarbageCollector{
        storageDir: storageDir,
        gcLockFile: filepath.Join(storageDir, "db", "gc.lock"),
        refs:       refs,
    }
}

// Run performs garbage collection with proper locking
func (gc *GarbageCollector) Run() error {
    // 1. Acquire global GC lock (Level 1)
    gcLock, err := os.OpenFile(gc.gcLockFile, os.O_RDWR|os.O_CREATE, 0644)
    if err != nil {
        return fmt.Errorf("failed to open GC lock: %w", err)
    }
    defer gcLock.Close()

    err = syscall.Flock(int(gcLock.Fd()), syscall.LOCK_EX)
    if err != nil {
        return fmt.Errorf("failed to acquire GC lock: %w", err)
    }
    defer syscall.Flock(int(gcLock.Fd()), syscall.LOCK_UN)

    // 2. Now safe to perform GC operations.
    //    Scan cache/images/ for content-addressed files ({checksum}_{arch}.qcow2).
    images, err := filepath.Glob(filepath.Join(gc.storageDir, "cache", "images", "*.qcow2"))
    if err != nil {
        return err
    }

    for _, imagePath := range images {
        // Derive the content-addressed key from the filename (strip .qcow2)
        imageKey := strings.TrimSuffix(filepath.Base(imagePath), ".qcow2")
        if err := gc.DeleteUnreferencedImage(imageKey); err != nil {
            // Log but continue with other images
            fmt.Printf("Failed to GC %s: %v\n", imageKey, err)
        }
    }

    return nil
}

// DeleteUnreferencedImage uses reference lock for atomic check-and-delete.
// imageKey is the content-addressed identity: {checksum}_{arch}.
func (gc *GarbageCollector) DeleteUnreferencedImage(imageKey string) error {
    // GC lock (Level 1) is already held
    // Now acquire reference lock (Level 2) - follows hierarchy
    return gc.refs.updateReferences(func(refData RefData) error {
        // Check references inside lock
        entry := refData[imageKey]
        if entry != nil && len(entry.Refs) > 0 {
            return nil // Image in use, skip
        }

        // No references, delete immediately
        imagePath := filepath.Join(gc.storageDir, "cache", "images", imageKey+".qcow2")
        return os.Remove(imagePath)
    })
}
```

### Lock Ordering for GC

GC operations follow the lock hierarchy:

1. **Acquire GC lock (Level 1)**: Ensures only one GC runs at a time
2. **For each image, acquire reference lock (Level 2)**: Atomic check-and-delete
3. **Never acquire VM metadata locks (Level 4)**: GC doesn't modify VM metadata

This ordering prevents deadlocks with concurrent VM create/delete operations:

- **VM Create**: Acquires Reference (L2) → Image Conversion (L3) → VM Metadata (L4)
- **VM Delete**: Acquires Reference (L2) → VM Metadata (L4)
- **GC**: Acquires GC (L1) → Reference (L2) for each image

No circular dependencies exist in this hierarchy.

### Preventing GC During VM Operations

VM create and delete operations must prevent GC from running concurrently:

```go
// VM operations should check if GC is running
func (vm *VMManager) CreateVM(image string, vmID string) error {
    // Acquire GC lock (blocking — waits if GC is running)
    gcLockPath := filepath.Join(vm.storageDir, "db", "gc.lock")
    gcLock, err := os.OpenFile(gcLockPath, os.O_RDWR|os.O_CREATE, 0644)
    if err != nil {
        return err
    }
    defer gcLock.Close()

    // Blocking exclusive lock — VM creation waits until GC finishes
    err = syscall.Flock(int(gcLock.Fd()), syscall.LOCK_EX)
    if err != nil {
        return fmt.Errorf("cannot acquire GC lock: %w", err)
    }
    defer syscall.Flock(int(gcLock.Fd()), syscall.LOCK_UN)

    // Now safe to proceed with VM creation
    // ...
}
```

**Alternative approach**: VM operations do NOT acquire GC lock, but GC is careful to never delete resources with active references. This allows VM operations to proceed during GC, but requires reference counter to be the source of truth.

## 7. Lock Performance Characteristics

### Expected Lock Contention

| Operation | Lock Level | Lock Type | Duration | Contention |
|-----------|-----------|-----------|----------|------------|
| GC run | GC lock (L1) | Exclusive | Minutes | Low (scheduled) |
| Image conversion | Image lock (L3) | Exclusive | 20-60s | High (first time only) |
| Reference update | Reference lock (L2) | Exclusive | 1-5ms | Medium |
| VM metadata update | VM lock (L4) | Exclusive | 1-5ms | Low (per-VM) |
| Overlay creation | None | N/A | 10-50ms | None |

### Optimization: In-Process Read-Write Cache (Optional — Deferred to Daemon Mode)

If a future daemon mode needs many concurrent readers (e.g., frequent GC scans)
and few writers, an in-process `sync.RWMutex` cache can be layered on top of the
flock-based `ReferenceCounter`. This is NOT needed for Phase 1 (one CLI
invocation = one process = no contention within process). Design and implement
this when daemon mode is introduced; it must correctly wrap `RefData`
(`map[string]*RefEntry`) and invalidate the cache after every flock-protected
write.

## 8. Deadlock Prevention

### Example Deadlock Scenario

```
Process A:                    Process B:
1. Lock VM-001               1. Lock Reference Counter
2. Read VM-001 config        2. Read references
3. Lock Reference Counter    3. Lock VM-001
   └─> BLOCKED               └─> BLOCKED
```

Both processes wait forever (deadlock).

### Prevention: Lock Ordering

**Rule**: Always acquire locks in hierarchy order.

```go
// WRONG: Can deadlock
func deleteVM(vmID string) {
    vmLock.Lock()         // Level 3
    defer vmLock.Unlock()

    config := readConfig(vmID)

    refLock.Lock()        // Level 4 (lower)
    defer refLock.Unlock()

    removeReference(config.BaseKey, vmID)
}

// CORRECT: Follows hierarchy
func deleteVM(vmID string) {
    refLock.Lock()        // Level 2 (acquire first)
    defer refLock.Unlock()

    vmLock.Lock()         // Level 4 (lower in hierarchy)
    defer vmLock.Unlock()

    config := readConfig(vmID)
    removeReference(config.BaseKey, vmID)
}
```

### Deadlock Detection

Use Go's `-race` detector and timeout-based detection.
**Note**: The `sync.Mutex` below is illustrative for in-process testing utilities.
All production locks use file-based `flock` as defined in this document.

```go
func lockWithTimeout(mu *sync.Mutex, timeout time.Duration) error {
    done := make(chan struct{})

    go func() {
        mu.Lock()
        close(done)
    }()

    select {
    case <-done:
        return nil
    case <-time.After(timeout):
        return fmt.Errorf("lock timeout: possible deadlock")
    }
}
```

## Summary

### Concurrency Guarantees

1. **Cross-process safety**: File-based locks (flock) work across multiple CLI invocations
2. **Image conversion**: Only one conversion per unique image, concurrent creates wait and reuse
3. **Reference counting**: Atomic updates, no lost updates or corruption
4. **VM metadata**: Per-VM locks prevent concurrent update conflicts
5. **GC safety**: Global GC lock + atomic check-and-delete prevents deletion of in-use resources
6. **Crash recovery**: File locks auto-released by kernel, orphan detection handles incomplete operations
7. **Deadlock-free**: Strict lock hierarchy (GC → Ref → Image → VM; Phase 2: → Checkpoint/Network → dnsmasq) prevents deadlocks

### File Lock Locations Summary

| Lock Purpose | File Path | Lock Level |
|--------------|-----------|------------|
| GC operations | `/var/lib/cocoon/db/gc.lock` | Level 1 (highest) |
| Reference counter | `/var/lib/cocoon/db/references.lock` | Level 2 |
| Name index | `/var/lib/cocoon/db/name-index.lock` | Level 2 (never held with references.lock) |
| OCI build txn | `/var/lib/cocoon/db/oci-build-txn.lock` | Level 2 (parent lock for OCI cross-index updates) |
| OCI build tags | `/var/lib/cocoon/db/oci-build-tags.lock` | Level 2 (never held with references.lock) |
| OCI layer refs | `/var/lib/cocoon/db/oci-layer-refs.lock` | Level 2 (never held with references.lock) |
| Image conversion | `/var/lib/cocoon/cache/locks/{checksum}_{arch}.lock` | Level 3 |
| VM metadata | `/var/lib/cocoon/vms/{vm-id}/metadata.lock` | Level 4 |
| Checkpoint (Phase 2) | `/var/lib/cocoon/vms/{vm-id}/checkpoint.lock` | Level 5 |
| Network (Phase 2) | `/run/cocoon/vms/{vm-id}/network.lock` | Level 5 |
| dnsmasq (Phase 2) | `/run/cocoon/dnsmasq/dnsmasq.lock` | Level 6 (lowest) |

### Crash Consistency

All file locks are automatically released on process crash:
- **kill -9**: Kernel releases all flock locks immediately
- **Incomplete operations**: Temp files remain but originals intact
- **Recovery**: Next operation retries successfully
- **No stale locks**: No manual cleanup needed

### Performance Characteristics

| Scenario | Throughput |
|----------|-----------|
| 20 concurrent creates (same image, multi-process) | 1 download + 20 overlays (~30s) |
| 50 concurrent creates (different images) | 50 downloads (limited by network) |
| Reference updates | ~1000 ops/sec (serialized, 1ms each) |
| VM metadata updates | ~1000 ops/sec per VM (no cross-VM contention) |
| GC scan (1000 images) | ~1 second (1ms per image) |

### Testing Recommendations

```bash
# Stress test: 20 concurrent creates (multi-process)
parallel -j 20 cocoon create myorg/ubuntu-bootable:22.04 --name vm-{} ::: {001..020}

# Verify only 1 conversion happened
ls -lh /var/lib/cocoon/cache/images/

# Verify all references recorded
cat /var/lib/cocoon/db/references.json | jq 'keys | length'

# Crash test: Kill process mid-create
cocoon create myorg/ubuntu-bootable:22.04 --name vm-test &
PID=$!
sleep 5
kill -9 $PID

# Verify no stale locks (next create should succeed)
cocoon create myorg/ubuntu-bootable:22.04 --name vm-test2

# Race condition test: Create + Delete + GC
cocoon create myorg/ubuntu-bootable:22.04 --name vm-race &
sleep 2
cocoon delete vm-race &
cocoon gc &
wait

# Verify no corruption
cocoon doctor

# Lock contention test: Many processes same image
time parallel -j 100 cocoon create myorg/ubuntu-bootable:22.04 --name vm-{} ::: {001..100}
# Should take ~30s (1 conversion) not ~50min (100 conversions)
```

### Acceptance Criteria Met

**P0-C1: Image Conversion Lock**
- ✅ Changed from sync.Mutex to file-based flock
- ✅ Lock path: `/var/lib/cocoon/cache/locks/{checksum}_{arch}.lock`
- ✅ Handles process crash (kernel auto-releases)
- ✅ 20 concurrent processes = 1 pull+convert, others wait

**P0-C2: File Lock Strategy**
- ✅ Defined lock locations for all resources
- ✅ Strict lock ordering prevents deadlocks
- ✅ Documented crash recovery behavior
- ✅ Under kill -9: no lost updates, no incorrect deletions, recoverable state

This design provides production-ready concurrency control for high-scale AI agent sandbox operations with full cross-process safety.

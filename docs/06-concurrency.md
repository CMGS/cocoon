# Concurrency Design

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
Level 1: Global Operation Lock
    ↓
Level 2: Image Conversion Lock (per-checksum)
    ↓
Level 3: VM Metadata Lock (per-VM)
    ↓
Level 4: Reference Counter Lock (global)
```

**Rules**:
- Never acquire a higher-level lock while holding a lower-level lock
- Always release locks in reverse order of acquisition
- If you need multiple locks at the same level, acquire them in sorted order by ID

## 1. Image Conversion Lock

### Problem

When multiple `cocoon create` commands run concurrently with the same OCI image:

```bash
# Terminal 1
$ cocoon create ubuntu:22.04 --name vm-001

# Terminal 2 (at the same time)
$ cocoon create ubuntu:22.04 --name vm-002

# Terminal 3 (at the same time)
$ cocoon create ubuntu:22.04 --name vm-003
```

Without locking, all three would:
1. Check cache → miss (image not yet cached)
2. Pull from registry → 3 parallel downloads
3. Convert to qcow2 → 3 conversions
4. Save to cache → race condition, corruption

### Solution: Per-Image Lock

Lock on the image checksum to serialize conversion operations:

```go
package storage

import (
    "crypto/sha256"
    "encoding/hex"
    "sync"
)

// ImageLockManager manages locks for image conversion operations
type ImageLockManager struct {
    locks sync.Map // map[string]*sync.Mutex
}

// NewImageLockManager creates a new lock manager
func NewImageLockManager() *ImageLockManager {
    return &ImageLockManager{}
}

// LockImage acquires the lock for a specific image checksum
func (m *ImageLockManager) LockImage(checksum string) {
    lock, _ := m.locks.LoadOrStore(checksum, &sync.Mutex{})
    lock.(*sync.Mutex).Lock()
}

// UnlockImage releases the lock for a specific image checksum
func (m *ImageLockManager) UnlockImage(checksum string) {
    lock, ok := m.locks.Load(checksum)
    if ok {
        lock.(*sync.Mutex).Unlock()
    }
}

// TryLockImage attempts to acquire the lock without blocking
func (m *ImageLockManager) TryLockImage(checksum string) bool {
    lock, _ := m.locks.LoadOrStore(checksum, &sync.Mutex{})
    return lock.(*sync.Mutex).TryLock()
}
```

### Usage Pattern

```go
func (mgr *ImageManager) PrepareBaseImage(image string) (*ImageInfo, error) {
    // 1. Calculate checksum (no lock needed, read-only operation)
    checksum, err := mgr.cache.CalculateManifestChecksum(image)
    if err != nil {
        return nil, err
    }

    // 2. Fast path: check if already cached (no lock for read)
    cachedPath := mgr.cache.GetCachedImage(checksum)
    if cachedPath != nil {
        return &ImageInfo{Path: cachedPath, Checksum: checksum}, nil
    }

    // 3. Slow path: acquire lock for conversion
    mgr.imageLocks.LockImage(checksum)
    defer mgr.imageLocks.UnlockImage(checksum)

    // 4. Double-check cache (another process may have finished while we waited)
    cachedPath = mgr.cache.GetCachedImage(checksum)
    if cachedPath != nil {
        return &ImageInfo{Path: cachedPath, Checksum: checksum}, nil
    }

    // 5. Pull and convert (only one process does this)
    containerID, err := mgr.buildah.PullImage(image)
    if err != nil {
        return nil, err
    }
    defer mgr.buildah.Cleanup(containerID)

    mountPoint, err := mgr.buildah.MountImage(containerID)
    if err != nil {
        return nil, err
    }

    baseImage, err := mgr.converter.CreateBaseImage(mountPoint, checksum)
    if err != nil {
        return nil, err
    }

    // 6. Store in cache with atomic write (see next section)
    err = mgr.cache.StoreImage(checksum, baseImage)
    if err != nil {
        return nil, err
    }

    return &ImageInfo{Path: baseImage, Checksum: checksum}, nil
}
```

### Behavior

- **First process**: Acquires lock, pulls image, converts, caches
- **Subsequent processes**: Wait on lock, then find cached image in step 4
- **Result**: Only one download and conversion per unique image

## 2. Reference Counter Atomicity

### Problem

`references.json` tracks which VMs use which base images:

```json
{
  "/cache/images/abc123.qcow2": ["vm-001", "vm-002"],
  "/cache/images/def456.qcow2": ["vm-003"]
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
)

// ReferenceCounter tracks base image usage with atomic operations
type ReferenceCounter struct {
    storageDir string
    lockFile   string
    dataFile   string
}

func NewReferenceCounter(storageDir string) *ReferenceCounter {
    return &ReferenceCounter{
        storageDir: storageDir,
        lockFile:   filepath.Join(storageDir, "cache", "references.lock"),
        dataFile:   filepath.Join(storageDir, "cache", "references.json"),
    }
}

// RefData represents the reference data structure
type RefData struct {
    References map[string][]string `json:"references"`
}

// updateReferences performs an atomic update operation
func (rc *ReferenceCounter) updateReferences(op func(*RefData) error) error {
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

    // 2. Read current references
    refs := &RefData{References: make(map[string][]string)}

    data, err := os.ReadFile(rc.dataFile)
    if err != nil && !os.IsNotExist(err) {
        return err
    }
    if len(data) > 0 {
        if err := json.Unmarshal(data, refs); err != nil {
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

// AddReference adds a VM reference to a base image
func (rc *ReferenceCounter) AddReference(baseImage, vmID string) error {
    return rc.updateReferences(func(refs *RefData) error {
        vms := refs.References[baseImage]
        // Check if already exists
        for _, vm := range vms {
            if vm == vmID {
                return nil // Already referenced
            }
        }
        refs.References[baseImage] = append(vms, vmID)
        return nil
    })
}

// RemoveReference removes a VM reference from a base image
func (rc *ReferenceCounter) RemoveReference(baseImage, vmID string) error {
    return rc.updateReferences(func(refs *RefData) error {
        vms := refs.References[baseImage]
        newVMs := []string{}
        for _, vm := range vms {
            if vm != vmID {
                newVMs = append(newVMs, vm)
            }
        }
        if len(newVMs) > 0 {
            refs.References[baseImage] = newVMs
        } else {
            delete(refs.References, baseImage)
        }
        return nil
    })
}

// GetReferences returns all VMs referencing a base image
func (rc *ReferenceCounter) GetReferences(baseImage string) ([]string, error) {
    var result []string

    err := rc.updateReferences(func(refs *RefData) error {
        vms := refs.References[baseImage]
        result = make([]string, len(vms))
        copy(result, vms)
        return nil
    })

    return result, err
}

// IsReferenced checks if a base image has any references
func (rc *ReferenceCounter) IsReferenced(baseImage string) (bool, error) {
    var referenced bool

    err := rc.updateReferences(func(refs *RefData) error {
        vms := refs.References[baseImage]
        referenced = len(vms) > 0
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
parallel -j 50 cocoon create ubuntu:22.04 --name vm-{} ::: {001..050}
```

**Expected Behavior**:

1. **Image Conversion** (Level 2 Lock):
   - Process 1: Acquires image lock, pulls and converts ubuntu:22.04
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
$ cat /var/lib/cocoon/cache/references.json
{
  "/var/lib/cocoon/cache/images/abc123.qcow2": [
    "vm-001", "vm-002", ..., "vm-050"
  ]
}

# Check disk usage (1 base + 50 overlays, not 50 full images)
$ du -sh /var/lib/cocoon/cache/images/abc123.qcow2
5.2G    abc123.qcow2

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
$ cocoon gc --aggressive

# Terminal 2: Delete VM (during GC)
$ cocoon delete vm-042
```

**Expected Behavior**:

1. **GC Process**:
   - Acquires reference lock
   - Reads references: `{"abc123.qcow2": ["vm-042"]}`
   - Sees vm-042 references abc123.qcow2
   - Skips deletion (image is referenced)
   - Releases lock
   - Moves to next image

2. **Delete Process** (runs after GC releases lock):
   - Acquires reference lock
   - Removes vm-042 from references
   - Writes: `{"abc123.qcow2": []}`
   - Releases lock
   - Deletes overlay file

3. **Next GC Run**:
   - Acquires reference lock
   - Reads references: `{"abc123.qcow2": []}`
   - Sees no references
   - Checks grace period (24 hours default)
   - If elapsed, moves image to trash

**Race Condition Prevention**:

The critical section is protected by the reference lock:

```go
func (gc *GarbageCollector) CollectImage(baseImage string) error {
    // WRONG: Check references outside lock (race condition)
    refs, _ := gc.refs.GetReferences(baseImage)
    if len(refs) > 0 {
        return nil // Image in use
    }
    // VM could be deleted here!
    os.Remove(baseImage) // DANGEROUS

    // CORRECT: Check and delete in same critical section
    return gc.refs.updateReferences(func(refData *RefData) error {
        refs := refData.References[baseImage]
        if len(refs) > 0 {
            return nil // Image in use, skip
        }

        // No references, safe to delete
        trashPath := gc.trashDir + filepath.Base(baseImage)
        return os.Rename(baseImage, trashPath)
    })
}
```

**Result**: No data loss or corruption. Either GC sees the reference and skips deletion, or delete completes first and GC cleans up on next run.

### Scenario C: Create VM While Base Image is Updating

**Command**:
```bash
# Terminal 1: Create VM with ubuntu:22.04
$ cocoon create ubuntu:22.04 --name vm-new

# Terminal 2: Update ubuntu:22.04 (new version released)
$ cocoon pull ubuntu:22.04 --force-update
```

**Expected Behavior**:

1. **Existing VMs Using Old Base**:
   ```
   /cache/images/abc123.qcow2  (old ubuntu:22.04)
     ├── vm-001/overlay.qcow2
     └── vm-002/overlay.qcow2
   ```

2. **Force Update** (Terminal 2):
   - Pulls new version of ubuntu:22.04
   - Calculates new checksum: `def456`
   - Converts and stores: `/cache/images/def456.qcow2`
   - Updates manifest cache to point ubuntu:22.04 → def456
   - Old base `abc123.qcow2` still exists (vm-001, vm-002 reference it)

3. **Create New VM** (Terminal 1):
   - Checks cache for ubuntu:22.04
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
    vmDir      string
    refs       *storage.ReferenceCounter
    imageLocks *storage.ImageLockManager
}

type Orphan struct {
    Type       string   // "overlay", "config", "reference"
    VMID       string
    Path       string
    BaseImage  string
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

        // Read config to get base image
        configData, err := os.ReadFile(configPath)
        if err != nil {
            continue
        }

        var config struct {
            BaseImage string `json:"base_image"`
        }
        json.Unmarshal(configData, &config)

        // Check if VM is in references
        refs, _ := r.refs.GetReferences(config.BaseImage)
        found := false
        for _, ref := range refs {
            if ref == vmID {
                found = true
                break
            }
        }

        if !found {
            orphans = append(orphans, Orphan{
                Type:      "reference",
                VMID:      vmID,
                BaseImage: config.BaseImage,
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
            // Safe to delete (VM was never fully created)
            trashPath := filepath.Join(r.vmDir, "..", "trash", orphan.VMID+"-orphan.qcow2")
            os.Rename(orphan.Path, trashPath)
            os.RemoveAll(filepath.Dir(orphan.Path)) // Remove VM dir

        case "reference":
            // Config exists but no reference -> add missing reference
            // This completes the incomplete operation
            err := r.refs.AddReference(orphan.BaseImage, orphan.VMID)
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
$ cocoon reconcile --check
Found 2 orphans:
- vm-042: overlay exists but no config (incomplete creation)
- vm-043: config exists but no reference (interrupted operation)

# Fix automatically
$ cocoon reconcile --fix
Cleaned up orphaned overlay: vm-042
Restored missing reference: vm-043

# Verify
$ cocoon reconcile --check
No orphans found. System is consistent.
```

### Crash Recovery Guarantees

1. **Atomicity of reference updates**: Using file lock + atomic write ensures references are never corrupted
2. **Idempotent operations**: Running reconcile multiple times produces same result
3. **Safe defaults**: When in doubt, preserve data (move to trash, don't delete)
4. **Manual override**: Admin can inspect trash before permanent deletion

## 5. GC Locking Strategy

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

### Solution: Atomic Check-And-Delete

The check and delete must be atomic (in same critical section):

```go
func (gc *GarbageCollector) DeleteUnreferencedImage(image string) error {
    return gc.refs.updateReferences(func(refData *RefData) error {
        // Check references inside lock
        refs := refData.References[image]
        if len(refs) > 0 {
            return nil // Image in use, skip
        }

        // No references, safe to delete
        // Check grace period
        info, err := os.Stat(image)
        if err != nil {
            return err
        }

        cutoffTime := time.Now().Add(-24 * time.Hour)
        if info.ModTime().After(cutoffTime) {
            return nil // Too recent, skip
        }

        // Move to trash (soft delete)
        trashPath := filepath.Join(gc.trashDir, filepath.Base(image))
        return os.Rename(image, trashPath)
    })
}
```

### GC Algorithm

```go
func (gc *GarbageCollector) Run() error {
    // 1. Find all base images
    images, err := filepath.Glob(filepath.Join(gc.imageDir, "*.qcow2"))
    if err != nil {
        return err
    }

    // 2. Process each image
    for _, image := range images {
        // Atomic check-and-delete
        err := gc.DeleteUnreferencedImage(image)
        if err != nil {
            log.Printf("Failed to GC %s: %v", image, err)
        }
    }

    // 3. Collect orphaned overlays
    err = gc.CollectOrphanedOverlays()
    if err != nil {
        return err
    }

    // 4. Collect old temp files
    err = gc.CollectTempFiles()
    if err != nil {
        return err
    }

    return nil
}
```

## 6. Lock Performance Characteristics

### Expected Lock Contention

| Operation | Lock Level | Duration | Contention |
|-----------|-----------|----------|------------|
| Image conversion | Image lock | 20-60s | High (first time only) |
| Overlay creation | None | 10-50ms | None |
| Reference update | Reference lock | 1-5ms | Medium |
| VM deletion | Reference lock | 1-5ms | Medium |
| GC scan | Reference lock (per image) | 1-5ms | Low |

### Optimization: Read-Write Locks

For scenarios with many readers and few writers:

```go
type ReferenceCounterRW struct {
    storageDir string
    lockFile   string
    dataFile   string
    rwLock     sync.RWMutex
    cache      *RefData
}

// GetReferences uses read lock (multiple readers allowed)
func (rc *ReferenceCounterRW) GetReferences(baseImage string) ([]string, error) {
    rc.rwLock.RLock()
    defer rc.rwLock.RUnlock()

    refs := rc.cache.References[baseImage]
    result := make([]string, len(refs))
    copy(result, refs)
    return result, nil
}

// AddReference uses write lock (exclusive)
func (rc *ReferenceCounterRW) AddReference(baseImage, vmID string) error {
    rc.rwLock.Lock()
    defer rc.rwLock.Unlock()

    // Update cache and persist to disk
    return rc.updateReferencesLocked(func(refs *RefData) error {
        vms := refs.References[baseImage]
        refs.References[baseImage] = append(vms, vmID)
        return nil
    })
}
```

**When to use**:
- Read-heavy workloads (many GC scans, few creates/deletes)
- Reduces contention for read operations
- Adds complexity (cache invalidation)

## 7. Deadlock Prevention

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

    removeReference(config.BaseImage, vmID)
}

// CORRECT: Follows hierarchy
func deleteVM(vmID string) {
    refLock.Lock()        // Level 4 (acquire first)
    defer refLock.Unlock()

    vmLock.Lock()         // Level 3
    defer vmLock.Unlock()

    config := readConfig(vmID)
    removeReference(config.BaseImage, vmID)
}
```

### Deadlock Detection

Use Go's `-race` detector and timeout-based detection:

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

1. **Image conversion**: Only one conversion per unique image, concurrent creates wait and reuse
2. **Reference counting**: Atomic updates, no lost updates or corruption
3. **GC safety**: Cannot delete image that's being used (atomic check-and-delete)
4. **Crash recovery**: Orphan detection and reconciliation restores consistency
5. **Deadlock-free**: Lock hierarchy prevents deadlocks

### Performance Characteristics

| Scenario | Throughput |
|----------|-----------|
| 50 concurrent creates (same image) | 1 download + 50 overlays (~30s) |
| 50 concurrent creates (different images) | 50 downloads (limited by network) |
| Reference updates | ~1000 ops/sec (serialized, 1ms each) |
| GC scan (1000 images) | ~1 second (1ms per image) |

### Testing Recommendations

```bash
# Stress test: 100 concurrent creates
parallel -j 100 cocoon create ubuntu:22.04 --name vm-{} ::: {001..100}

# Verify references
cat /var/lib/cocoon/cache/references.json | jq '.references | length'

# Crash test: Kill process mid-create
cocoon create ubuntu:22.04 --name vm-test &
PID=$!
sleep 5
kill -9 $PID
cocoon reconcile --check

# Race condition test: Create + GC
cocoon create ubuntu:22.04 --name vm-race &
cocoon gc --aggressive &
wait

# Verify no corruption
cocoon reconcile --check
```

This design provides production-ready concurrency control for high-scale AI agent sandbox operations.

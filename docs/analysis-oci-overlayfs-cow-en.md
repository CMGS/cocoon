# OCI VM Image OverlayFS and COW Analysis

**Date**: 2026-02-19  
**Status**: Analysis Document  
**Related Docs**: [04.1-oci-vm-images.md](./04.1-oci-vm-images.md), [05-storage-management.md](./05-storage-management.md)

---

## Question

**Does the project use OverlayFS when running OCI image-based VMs, and how is COW (Copy-on-Write) implemented?**

---

## Executive Summary

### Short Answer

1. **OverlayFS Purpose**: OverlayFS is a core component of the Phase 2 OCI VM image design, but is **NOT YET IMPLEMENTED**. It is designed to:
   - Compose OCI image layers (kernel, rootfs, customization) into a unified filesystem
   - Serve the composed filesystem to VM guests via virtiofs
   - Enable layer sharing across VMs with per-VM write isolation

2. **COW Implementation**:
   - **Phase 1 (Current)**: Uses qcow2 overlay files with `qemu-img create -b` backing files for COW
   - **Phase 2 (Planned)**: Uses Linux OverlayFS kernel feature with upperdir/lowerdir for COW

---

## Current State: Phase 1 qcow2 COW

### Storage Structure
```
/var/lib/cocoon/
+-- cache/
|   +-- images/
|       +-- {baseKey}.qcow2           # Shared base image (read-only)
+-- vms/
    +-- {vmID}/
        +-- overlay.qcow2              # Per-VM COW overlay
        +-- config.json                # VM configuration
```

### COW Mechanism
- **Base images**: Stored in `cache/images/`, mode 0444 (read-only)
- **Overlay files**: Each VM has its own overlay.qcow2 file
- **Create command**: `qemu-img create -f qcow2 -F qcow2 -b <backing> <overlay>`
- **Write behavior**: 
  - First write to a block → read from base → copy to overlay → modify
  - Subsequent writes → directly modify overlay block
  - Base remains unchanged, shared by multiple VMs

### Implementation
- **File**: `storage/local/cow.go`
- **Interface**: `storage.COWManager`
- **Key methods**:
  - `CreateBaseImage()` - Copy source image to cache
  - `CreateOverlay()` - Create qcow2 overlay
  - `RemoveOverlay()` - Delete overlay (optionally move to trash)

**Status**: ✅ **Fully implemented and working**

---

## Planned: Phase 2 OverlayFS COW

### Design Goals

According to `docs/04.1-oci-vm-images.md`, Phase 2 uses OverlayFS instead of qcow2 because:

1. **Direct kernel boot**: Skip UEFI firmware, kernel and initrd passed directly to Cloud Hypervisor
2. **Layer sharing**: OCI image layers stored as directories, shared across VMs
3. **Instant composition**: OverlayFS composes layers at mount time, no qcow2 rebuild needed
4. **virtiofs delivery**: Merged directory served to guest as rootfs via virtiofs

### Storage Structure (Not Implemented)

```
/var/lib/cocoon/
+-- cache/
|   +-- oci/
|       +-- {manifest-digest}/          # Indexed by manifest digest
|           +-- manifest.json
|           +-- config.json
|           +-- vmlinuz                 # Extracted kernel
|           +-- initrd.img              # Extracted initrd
|           +-- rootfs/                 # Base rootfs layer (directory, read-only)
|           +-- custom-1/               # First customization layer (directory, read-only)
|           +-- custom-2/               # Second customization layer (directory, read-only)
+-- vms/
    +-- {vmID}/
        +-- upper/                      # OverlayFS upperdir (COW write layer)
        +-- work/                       # OverlayFS workdir (kernel internal)
        +-- merged/                     # OverlayFS mount point
        +-- virtiofsd.sock              # virtiofsd socket
        +-- config.json
```

### OverlayFS Mount Command (Not Implemented)

For a VM with 2 customization layers:

```bash
mount -t overlay overlay \
  -o lowerdir=/var/lib/cocoon/cache/oci/{digest}/custom-2:\
              /var/lib/cocoon/cache/oci/{digest}/custom-1:\
              /var/lib/cocoon/cache/oci/{digest}/rootfs,\
     upperdir=/var/lib/cocoon/vms/{vmID}/upper,\
     workdir=/var/lib/cocoon/vms/{vmID}/work \
  /var/lib/cocoon/vms/{vmID}/merged
```

### COW Mechanism Design

**Layer Stack**:
```
┌─────────────────────────────────────┐
│ Guest VM                            │
│ (sees merged filesystem via         │
│  virtiofs with tag "cocoonfs")      │
└─────────────────┬───────────────────┘
                  │ virtiofs
                  ↓
┌─────────────────────────────────────┐
│ Host: /var/lib/cocoon/vms/{vmID}/   │
│       merged/  (OverlayFS mount)    │
└─────────────────┬───────────────────┘
                  │ OverlayFS kernel
                  ↓
┌─────────────────────────────────────┐
│ Layers (top to bottom)              │
├─────────────────────────────────────┤
│ upper/          ← Guest writes here │  Per-VM (read-write)
├─────────────────────────────────────┤
│ custom-2/       ← User customization│  Shared (read-only)
│ custom-1/       ← User customization│  Shared (read-only)
│ rootfs/         ← Base OS           │  Shared (read-only)
└─────────────────────────────────────┘
```

**Write Behavior**:
1. Guest writes file in rootfs (e.g., create `/tmp/foo`)
2. virtiofsd receives FUSE write request
3. OverlayFS detects write to read-only layer
4. OverlayFS performs copy-up:
   - If file exists in lower → copy entire file to upper/
   - New file → create directly in upper/
5. Subsequent modifications → directly modify version in upper/
6. Lower layers (rootfs/, custom-*/) remain unchanged

**Advantages**:
- ✅ Layer sharing: Multiple VMs share same base and custom layers (directories)
- ✅ Instant startup: No qcow2 copy or conversion needed
- ✅ Space efficiency: Only per-VM writes stored in upper/
- ✅ Aligned with OCI standard: layer = directory tree

**Status**: ❌ **Not implemented**

---

## Implementation Status

### Implemented (Phase 1)

| Component | File | Status |
|-----------|------|--------|
| qcow2 COW management | `storage/local/cow.go` | ✅ Fully implemented |
| Reference counting | `storage/local/refcount.go` | ✅ Fully implemented |
| Garbage collection | `storage/local/gc.go` | ✅ Fully implemented |
| qcow2 base image cache | `cache/images/` | ✅ Operational |

### Not Implemented (Phase 2)

| Component | Expected Location | Status |
|-----------|-------------------|--------|
| OCI layer extraction | `image/pipeline/oci_extract.go` | ❌ Not implemented |
| OverlayFS mount management | `storage/local/overlay.go` | ❌ Not implemented |
| OCI reference counting | `storage/local/oci_refcount.go` | ❌ Not implemented |
| virtiofsd lifecycle | `vm/engine/virtiofsd.go` | ❌ Not implemented |
| Direct kernel boot | `vm/engine/manager.go` | ⚠️ Partial schema added |
| OCI cache directory | `cache/oci/` | ❌ Not created |

---

## Comparison: Phase 1 vs Phase 2 COW

| Aspect | Phase 1 (qcow2) | Phase 2 (OverlayFS) |
|--------|-----------------|---------------------|
| **Base storage** | qcow2 files | Directory trees |
| **COW mechanism** | qemu-img overlay | OverlayFS upperdir |
| **Sharing unit** | File (qcow2 backing) | Directory tree (lowerdir) |
| **Write location** | overlay.qcow2 internal blocks | vms/{vmID}/upper/ |
| **Boot method** | UEFI firmware | Direct kernel boot |
| **Rootfs delivery** | virtio-blk (block device) | virtiofs (filesystem) |
| **Layer concept** | Single base qcow2 | Multiple layer directories |
| **Space efficiency** | Good (block-level COW) | Better (file-level COW) |
| **Boot speed** | Slower (UEFI init) | Fast (direct boot) |
| **Implementation complexity** | Simple (mature tools) | Medium (mount management) |
| **Current status** | ✅ Implemented | ❌ Not implemented |

---

## Phase 2 Implementation Dependencies

```
┌────────────────────────────────────────┐
│ 1. OCI Image Pull & Layer Extraction   │
│    - Pull OCI manifest                 │
│    - Extract layers to cache/oci/      │
│    - Validate media types              │
└────────────────┬───────────────────────┘
                 │ Must implement first
                 ↓
┌────────────────────────────────────────┐
│ 2. OverlayFS Mount Management          │
│    - Create upper/work/merged dirs     │
│    - Mount OverlayFS                   │
│    - Handle unmount on VM stop         │
└────────────────┬───────────────────────┘
                 │ Must implement first
                 ↓
┌────────────────────────────────────────┐
│ 3. virtiofsd Daemon Management         │
│    - Spawn virtiofsd process           │
│    - Point to merged/ directory        │
│    - Manage socket lifecycle           │
└────────────────┬───────────────────────┘
                 │ Must implement first
                 ↓
┌────────────────────────────────────────┐
│ 4. Direct Kernel Boot Integration      │
│    - Wire kernel/initrd to CH API      │
│    - Configure fs[] for virtiofs       │
│    - Pass kernel cmdline               │
└────────────────┬───────────────────────┘
                 │ Must implement first
                 ↓
┌────────────────────────────────────────┐
│ 5. OCI Reference Counting & GC         │
│    - Track manifest → VM references    │
│    - GC unreferenced cache entries     │
│    - Clean up upper/work/merged        │
└────────────────────────────────────────┘
```

---

## Key Findings

1. **OverlayFS is core to Phase 2** but the project currently **operates entirely on Phase 1 qcow2 mechanism**
2. **COW is implemented differently in two phases**:
   - Phase 1: qcow2 block-level COW (implemented)
   - Phase 2: OverlayFS filesystem-level COW (not implemented)
3. **Documentation is complete**, design is clear, but code implementation has not started
4. **Architecture is compatible**, both modes can coexist

---

## Recommendations

If implementing Phase 2 OverlayFS support, suggested order:

1. **PR 1**: OCI layer extraction to directories
   - Implement OCI manifest parsing
   - Implement layer extraction to `cache/oci/{digest}/`
   - Add media type validation

2. **PR 2**: OverlayFS mount management
   - Implement mount/unmount logic
   - Create upper/work/merged directories
   - Handle layer ordering and merging

3. **PR 3**: virtiofsd lifecycle
   - Spawn virtiofsd process
   - Manage socket
   - Handle process cleanup

4. **PR 4**: Direct kernel boot
   - Complete CH REST API integration
   - Wire kernel/initrd paths
   - Pass kernel cmdline

5. **PR 5**: OCI reference counting and GC
   - Implement oci-references.json
   - Extend gc command
   - Clean up OverlayFS mounts

---

## Conclusion

**Answer to the original question**: 

OverlayFS is a critical component of the Phase 2 design that will provide efficient layer sharing and COW mechanism for OCI VM images, but the project currently uses Phase 1's qcow2 approach. Implementing Phase 2 requires coordinated development across multiple modules.

The current implementation uses qcow2 overlay files for COW, which is mature and working. The future OverlayFS implementation will provide better performance and better alignment with OCI standards, but requires significant additional development work.

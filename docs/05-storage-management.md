# Storage Management

## Overview

This document describes the storage management strategy for the Cocoon AI Agent Sandbox, focusing on directory layout, copy-on-write (COW) optimization, reference counting, and garbage collection. The design enables efficient disk space usage while supporting high-concurrency VM operations.

## Storage Layout

### Directory Structure

The storage system uses a well-organized hierarchy that separates cached base images, per-VM overlays, temporary files, and soft-deleted resources:

```
/var/lib/cocoon/
├── cache/
│   ├── manifests/              # Image metadata (checksum-based)
│   │   ├── abc123...json       # Manifest checksum → metadata
│   │   └── def456...json
│   ├── images/                 # Base qcow2 files (backing files)
│   │   ├── abc123...qcow2      # Checksum-based filenames
│   │   └── def456...qcow2
│   ├── locks/                  # Image conversion locks (per-checksum)
│   │   ├── abc123...lock
│   │   └── def456...lock
│   ├── references.json         # Reference counter data
│   └── references.lock         # Reference counter lock file
├── vms/
│   ├── vm-001/
│   │   ├── overlay.qcow2       # VM's COW overlay
│   │   ├── config.json         # VM configuration
│   │   ├── metadata.json       # Runtime metadata
│   │   └── metadata.lock       # VM metadata lock file
│   ├── vm-002/
│   │   ├── overlay.qcow2
│   │   ├── config.json
│   │   ├── metadata.json
│   │   └── metadata.lock
│   └── ...
├── temp/                       # Temporary images during creation
├── trash/                      # Soft-deleted images (for recovery)
└── gc.lock                     # Global GC lock file
```

### Storage Configuration

```python
from dataclasses import dataclass
from pathlib import Path

@dataclass
class StorageConfig:
    root: Path
    cache_dir: Path
    vm_dir: Path
    temp_dir: Path
    trash_dir: Path

    @classmethod
    def default(cls, root: Path = None) -> "StorageConfig":
        root = root or Path("/var/lib/cocoon")
        return cls(
            root=root,
            cache_dir=root / "cache",
            vm_dir=root / "vms",
            temp_dir=root / "temp",
            trash_dir=root / "trash"
        )

    def ensure_dirs(self):
        """Create all required directories."""
        for path in [
            self.cache_dir / "manifests",
            self.cache_dir / "images",
            self.cache_dir / "locks",
            self.vm_dir,
            self.temp_dir,
            self.trash_dir
        ]:
            path.mkdir(parents=True, exist_ok=True)
```

## Copy-on-Write (COW) Strategy

### qcow2 Backing Files

The core of the storage efficiency comes from qcow2's backing file feature. Multiple VMs can share a single base image, with each VM storing only its differences in a lightweight overlay.

```python
from pathlib import Path
import subprocess
import json

class COWImageManager:
    def __init__(self, overlay_dir: Path):
        self.overlay_dir = overlay_dir
        self.overlay_dir.mkdir(parents=True, exist_ok=True)

    def create_overlay(
        self,
        base_image: Path,
        vm_id: str,
        size: str | None = None
    ) -> Path:
        """
        Create COW overlay image with base as backing file.

        The overlay image only stores differences from the base.
        Multiple VMs can share the same base image.
        """
        overlay_path = self.overlay_dir / f"{vm_id}.qcow2"

        cmd = [
            "qemu-img", "create",
            "-f", "qcow2",
            "-F", "qcow2",  # Backing file format
            "-b", str(base_image),  # Backing file
            str(overlay_path)
        ]

        if size:
            cmd.append(size)

        subprocess.run(cmd, check=True)

        return overlay_path

    def get_overlay_info(self, overlay_path: Path) -> dict:
        """Get overlay image information."""
        result = subprocess.run([
            "qemu-img", "info",
            "--output=json",
            str(overlay_path)
        ], capture_output=True, text=True, check=True)

        return json.loads(result.stdout)

    def get_backing_chain(self, overlay_path: Path) -> list[Path]:
        """Get full backing file chain."""
        chain = [overlay_path]
        info = self.get_overlay_info(overlay_path)

        while "backing-filename" in info:
            backing = Path(info["backing-filename"])
            chain.append(backing)
            info = self.get_overlay_info(backing)

        return chain
```

### Space Efficiency Example

The COW strategy dramatically reduces disk space requirements:

```python
# Scenario: 100 VMs from the same ubuntu:22.04 image

# 1. Prepare base image (cached, done once)
base_image = await image_manager.prepare_base_image("ubuntu:22.04")
# Result: /var/lib/cocoon/cache/images/abc123...qcow2 (5GB)

# 2. Create overlay for VM-1
vm1_overlay = cow_manager.create_overlay(base_image, "vm-001")
# Result: /var/lib/cocoon/vms/vm-001/overlay.qcow2 (~200KB initially)

# 3. Create overlay for VM-2 (shares same base)
vm2_overlay = cow_manager.create_overlay(base_image, "vm-002")
# Result: /var/lib/cocoon/vms/vm-002/overlay.qcow2 (~200KB initially)

# ... repeat 98 more times ...

# Total disk space for 100 VMs:
# - 1 base image: 5GB
# - 100 overlays: 100 × 200KB = 20MB
# - Total: ~5.02GB (not 500GB!)
```

Each overlay only grows as the VM writes data. If each VM writes 100MB of unique data, the total becomes 5GB + 10GB = 15GB, still far less than 100 × 5GB = 500GB.

## Reference Counting

### Tracking Image Usage

Reference counting ensures base images are not deleted while VMs still depend on them. The system maintains a mapping of base images to the VMs using them.

```python
import json
from collections import defaultdict
from pathlib import Path
from typing import Dict, Set

class ReferenceCounter:
    def __init__(self, storage_config: StorageConfig):
        self.storage = storage_config
        self.ref_file = self.storage.cache_dir / "references.json"
        self.refs: Dict[str, Set[str]] = defaultdict(set)
        self.load()

    def load(self):
        """Load reference counts from disk."""
        if self.ref_file.exists():
            data = json.loads(self.ref_file.read_text())
            self.refs = {k: set(v) for k, v in data.items()}

    def save(self):
        """Save reference counts to disk."""
        data = {k: list(v) for k, v in self.refs.items()}
        self.ref_file.write_text(json.dumps(data, indent=2))

    def add_reference(self, base_image: Path, vm_id: str):
        """Add VM reference to base image."""
        self.refs[str(base_image)].add(vm_id)
        self.save()

    def remove_reference(self, base_image: Path, vm_id: str):
        """Remove VM reference from base image."""
        self.refs[str(base_image)].discard(vm_id)
        if not self.refs[str(base_image)]:
            del self.refs[str(base_image)]
        self.save()

    def get_references(self, base_image: Path) -> Set[str]:
        """Get all VMs referencing base image."""
        return self.refs.get(str(base_image), set())

    def is_referenced(self, base_image: Path) -> bool:
        """Check if base image is referenced by any VM."""
        return len(self.get_references(base_image)) > 0

    def get_unreferenced_images(self) -> list[Path]:
        """Get all base images with zero references."""
        all_images = set(
            str(p) for p in self.storage.cache_dir.glob("images/*.qcow2")
        )
        referenced = set(self.refs.keys())
        unreferenced = all_images - referenced
        return [Path(p) for p in unreferenced]
```

### references.json Structure

The reference count file stores a mapping of base image paths to VM IDs:

```json
{
  "/var/lib/cocoon/cache/images/abc123...qcow2": [
    "vm-001",
    "vm-002",
    "vm-003"
  ],
  "/var/lib/cocoon/cache/images/def456...qcow2": [
    "vm-010",
    "vm-011"
  ]
}
```

### Add/Remove Operations

Reference counting operations are performed during VM lifecycle events:

```python
# When creating a VM
async def create_vm(image: str, vm_id: str) -> Path:
    # ... prepare base image ...
    base_image = await image_mgr.prepare_base_image(image)

    # ... create overlay ...
    overlay = cow_mgr.create_overlay(base_image, vm_id)

    # Register reference
    ref_counter.add_reference(base_image, vm_id)

    return overlay

# When deleting a VM
def delete_vm(vm_id: str):
    # ... load config ...
    config = json.loads((vm_dir / "config.json").read_text())
    base_image = Path(config["base_image"])

    # Remove reference
    ref_counter.remove_reference(base_image, vm_id)

    # ... cleanup overlay ...
```

### Concurrency Considerations

Reference counting operations must be **cross-process safe**. See [06-concurrency.md](./06-concurrency.md) for details on:

- File-based locking (flock) for `references.json` updates at `/var/lib/cocoon/cache/references.lock`
- Atomic read-modify-write operations using temp files and fsync
- Race condition prevention during simultaneous VM creation/deletion across multiple processes
- Crash recovery (locks auto-released by kernel on process crash)
- Lock hierarchy to prevent deadlocks (Reference Lock is Level 2)

## Garbage Collection

### Overview

Garbage collection automatically reclaims disk space from resources that are no longer needed. The system uses grace periods to avoid premature deletion of recently created resources.

```python
import time
from datetime import datetime, timedelta

class GarbageCollector:
    def __init__(
        self,
        storage_config: StorageConfig,
        ref_counter: ReferenceCounter
    ):
        self.storage = storage_config
        self.refs = ref_counter

    def collect_unreferenced_images(
        self,
        grace_period: timedelta = timedelta(hours=24)
    ) -> list[Path]:
        """
        Collect unreferenced base images older than grace period.

        Grace period prevents deleting recently created images that
        might be about to be used.
        """
        collected = []
        cutoff_time = time.time() - grace_period.total_seconds()

        for image in self.refs.get_unreferenced_images():
            if not image.exists():
                continue

            # Check age
            if image.stat().st_mtime > cutoff_time:
                print(f"Skipping recent image: {image}")
                continue

            # Move to trash (soft delete)
            trash_path = self.storage.trash_dir / image.name
            image.rename(trash_path)
            collected.append(image)

            print(f"Collected: {image} -> {trash_path}")

        return collected

    def collect_orphaned_overlays(self) -> list[Path]:
        """
        Collect overlay images without corresponding VM config.
        """
        collected = []

        for vm_dir in self.storage.vm_dir.iterdir():
            if not vm_dir.is_dir():
                continue

            overlay = vm_dir / "overlay.qcow2"
            config = vm_dir / "config.json"

            # Overlay exists but config missing = orphaned
            if overlay.exists() and not config.exists():
                trash_path = self.storage.trash_dir / f"{vm_dir.name}-overlay.qcow2"
                overlay.rename(trash_path)
                collected.append(overlay)

                print(f"Collected orphaned overlay: {overlay}")

        return collected

    def collect_temp_files(
        self,
        max_age: timedelta = timedelta(hours=1)
    ) -> list[Path]:
        """Collect temporary files older than max_age."""
        collected = []
        cutoff_time = time.time() - max_age.total_seconds()

        for temp_file in self.storage.temp_dir.iterdir():
            if temp_file.stat().st_mtime < cutoff_time:
                trash_path = self.storage.trash_dir / temp_file.name
                temp_file.rename(trash_path)
                collected.append(temp_file)

                print(f"Collected temp file: {temp_file}")

        return collected

    def empty_trash(self, max_age: timedelta = timedelta(days=7)):
        """Permanently delete trash older than max_age."""
        cutoff_time = time.time() - max_age.total_seconds()

        for item in self.storage.trash_dir.iterdir():
            if item.stat().st_mtime < cutoff_time:
                item.unlink()
                print(f"Permanently deleted: {item}")

    def full_gc(self):
        """Run full garbage collection cycle."""
        print("Starting garbage collection...")

        # Collect unreferenced images
        images = self.collect_unreferenced_images()
        print(f"Collected {len(images)} unreferenced images")

        # Collect orphaned overlays
        overlays = self.collect_orphaned_overlays()
        print(f"Collected {len(overlays)} orphaned overlays")

        # Collect old temp files
        temps = self.collect_temp_files()
        print(f"Collected {len(temps)} temp files")

        # Empty old trash
        self.empty_trash()

        print("Garbage collection complete")
```

### Garbage Collection Locking

GC operations must coordinate with concurrent VM create/delete operations. See [06-concurrency.md](./06-concurrency.md) for details on:

- Global GC lock at `/var/lib/cocoon/gc.lock` (Level 1 in lock hierarchy)
- Atomic check-and-delete using reference counter lock (Level 2)
- Lock ordering to prevent deadlocks with VM operations
- Crash recovery and lock auto-release

**Key guarantee**: GC cannot delete a base image while any VM references it, even under high concurrency or process crashes.

### Collection Categories

#### 1. Unreferenced Base Images

Base images with zero VM references are candidates for collection after a grace period (default: 24 hours).

**Why grace period?** A newly downloaded base image might not have references yet if VMs are still being provisioned. The grace period prevents premature deletion.

**Locking**: GC acquires both GC lock (L1) and reference lock (L2) to perform atomic check-and-delete.

#### 2. Orphaned Overlays

Overlay images whose parent VM configuration has been deleted or corrupted. These indicate incomplete cleanup operations.

**Detection:** `overlay.qcow2` exists but `config.json` is missing in the same VM directory.

**Locking**: GC lock (L1) only, as this operates on already-deleted VMs.

#### 3. Temporary Files

Files in the `/var/lib/cocoon/temp/` directory older than a threshold (default: 1 hour).

**Source:** Failed image conversions, interrupted downloads, or crashed operations.

**Locking**: GC lock (L1) only, as temp files are not referenced.

#### 4. Trash Cleanup

Permanently delete soft-deleted items from trash after a recovery period (default: 7 days).

**Purpose:** Allows recovery from accidental deletions while eventually reclaiming disk space.

**Locking**: GC lock (L1) only, as trash items are already soft-deleted.

### Grace Periods

Different resource types use different grace periods:

| Resource Type | Default Grace Period | Rationale |
|--------------|---------------------|-----------|
| Unreferenced images | 24 hours | Allow for batch VM creation |
| Temporary files | 1 hour | Short-lived by nature |
| Trash items | 7 days | Recovery window |

These values are configurable in production deployments.

### Scheduled Garbage Collection

Run GC periodically in the background:

```python
import asyncio

async def scheduled_gc_loop():
    """Run garbage collection periodically."""
    storage = StorageConfig.default()
    ref_counter = ReferenceCounter(storage)
    gc = GarbageCollector(storage, ref_counter)

    while True:
        # Run GC every hour
        await asyncio.sleep(3600)

        try:
            gc.full_gc()
        except Exception as e:
            print(f"GC error: {e}")
```

## Storage Quotas

### Future Implementation

Storage quotas will limit disk space usage per tenant or VM pool. Planned features include:

- **Per-tenant quotas**: Limit total disk space per customer
- **Per-VM quotas**: Restrict individual VM overlay growth
- **Pool quotas**: Limit shared base image cache size
- **Soft/hard limits**: Warnings before enforcement
- **Quota enforcement**: Reject VM creation or writes when exceeded

See [future/storage-quotas.md](./future/storage-quotas.md) for detailed design.

## Example Workflows

### Workflow 1: Create VM with Storage Tracking

```python
async def create_vm(image: str, vm_id: str) -> Path:
    """Complete workflow to create a new VM from an OCI image."""
    # 1. Setup
    storage = StorageConfig.default()
    storage.ensure_dirs()

    cache = ImageCache(storage.cache_dir)
    image_mgr = ImageManager(cache)
    cow_mgr = COWImageManager(storage.vm_dir)
    ref_counter = ReferenceCounter(storage)

    # 2. Prepare base image (pulls if not cached)
    base_image = await image_mgr.prepare_base_image(image)
    print(f"Base image ready: {base_image}")

    # 3. Create VM directory
    vm_dir = storage.vm_dir / vm_id
    vm_dir.mkdir(exist_ok=True)

    # 4. Create COW overlay
    overlay = cow_mgr.create_overlay(base_image, vm_id)
    print(f"Overlay created: {overlay}")

    # 5. Register reference
    ref_counter.add_reference(base_image, vm_id)

    # 6. Save VM config
    config = {
        "vm_id": vm_id,
        "image": image,
        "base_image": str(base_image),
        "overlay": str(overlay),
        "created": datetime.now().isoformat()
    }
    (vm_dir / "config.json").write_text(json.dumps(config, indent=2))

    return overlay
```

### Workflow 2: Delete VM with Reference Cleanup

```python
def delete_vm(vm_id: str):
    """Complete workflow to delete a VM and cleanup resources."""
    storage = StorageConfig.default()
    ref_counter = ReferenceCounter(storage)

    vm_dir = storage.vm_dir / vm_id
    if not vm_dir.exists():
        raise ValueError(f"VM not found: {vm_id}")

    # 1. Load config
    config = json.loads((vm_dir / "config.json").read_text())
    base_image = Path(config["base_image"])
    overlay = Path(config["overlay"])

    # 2. Remove reference
    ref_counter.remove_reference(base_image, vm_id)

    # 3. Move overlay to trash (soft delete)
    trash_overlay = storage.trash_dir / f"{vm_id}-overlay.qcow2"
    if overlay.exists():
        overlay.rename(trash_overlay)

    # 4. Remove VM directory
    import shutil
    shutil.rmtree(vm_dir)

    print(f"VM deleted: {vm_id}")
    print(f"Overlay moved to trash: {trash_overlay}")

    # 5. Check if base image can be garbage collected
    if not ref_counter.is_referenced(base_image):
        print(f"Base image {base_image} can be garbage collected")
```

### Workflow 3: High-Concurrency VM Pool

```python
async def create_vm_pool(image: str, count: int) -> list[Path]:
    """Create multiple VMs concurrently from the same base image."""
    storage = StorageConfig.default()
    storage.ensure_dirs()

    cache = ImageCache(storage.cache_dir)
    image_mgr = ImageManager(cache)
    cow_mgr = COWImageManager(storage.vm_dir)
    ref_counter = ReferenceCounter(storage)

    # 1. Prepare base image once (shared by all VMs)
    base_image = await image_mgr.prepare_base_image(image)
    print(f"Base image ready: {base_image}")

    # 2. Create overlays concurrently
    async def create_one_vm(idx: int) -> Path:
        vm_id = f"vm-{idx:03d}"
        vm_dir = storage.vm_dir / vm_id
        vm_dir.mkdir(exist_ok=True)

        overlay = cow_mgr.create_overlay(base_image, vm_id)
        ref_counter.add_reference(base_image, vm_id)

        config = {
            "vm_id": vm_id,
            "image": image,
            "base_image": str(base_image),
            "overlay": str(overlay)
        }
        (vm_dir / "config.json").write_text(json.dumps(config, indent=2))

        return overlay

    # Create all VMs in parallel
    overlays = await asyncio.gather(*[
        create_one_vm(i) for i in range(count)
    ])

    print(f"Created {count} VMs from 1 base image")
    print(f"Disk usage: 1 base (~5GB) + {count} overlays (~200KB each)")
    print(f"Total: ~{5 + count * 0.0002:.2f} GB instead of {count * 5} GB")

    return overlays
```

## Performance Considerations

### Filesystem-Level COW

Use filesystem-level copy-on-write when available for even faster operations:

```bash
# Use reflink for instant copies on btrfs/xfs
cp --reflink=auto base.qcow2 copy.qcow2

# zstd compression for base images
qemu-img convert -f qcow2 -O qcow2 -o compression_type=zstd input.qcow2 output.qcow2
```

### Monitoring and Metrics

Track storage usage metrics:

- Base image cache size
- Total overlay size per VM
- Reference count per base image
- GC collection rates
- Trash directory size

## Summary

The storage management system provides:

1. **Efficient Layout**: Organized directories separating base images, overlays, temp, and trash
2. **Space Optimization**: COW overlays allow 100 VMs to use ~5GB instead of 500GB
3. **Safety**: Reference counting prevents premature deletion of in-use base images
4. **Automation**: Garbage collection with grace periods cleans up unused resources
5. **Recoverability**: Soft-delete to trash allows recovery from accidents
6. **Scalability**: Supports high-concurrency VM creation through shared base images

The combination of qcow2 backing files, checksum-based caching, and intelligent reference counting delivers an optimal storage solution for high-concurrency VM operations.

# OCI Image Handling and qcow2 Management Strategy

## Overview

This document describes the strategy for handling OCI container images and managing qcow2 disk images for the Cocoon AI Agent Sandbox. The design focuses on efficiency, reusability, and copy-on-write (COW) optimization to support high-concurrency VM operations.

## Architecture

### Component Stack

```
OCI Registry (e.g., Docker Hub)
         ↓
    Buildah (pull & extract)
         ↓
   Image Cache (checksum-based)
         ↓
  qcow2 Base Images (backing files)
         ↓
Per-VM Overlay Images (COW layers)
```

## OCI Image Workflow

### 1. Image Pull

Use Buildah to pull OCI images from registries:

```bash
# Pull image
buildah pull docker://ubuntu:22.04

# Pull to specific storage location
buildah pull --storage-driver overlay docker://python:3.11
```

**Key Operations**:
- Pull from any OCI-compatible registry (Docker Hub, GitHub Container Registry, private registries)
- Verify image signatures and checksums
- Store in Buildah's local storage (`~/.local/share/containers/storage`)

### 2. Image Extract

Extract OCI image layers into a rootfs:

```bash
# Create container from image
CONTAINER=$(buildah from ubuntu:22.04)

# Mount container filesystem
MOUNT_POINT=$(buildah mount $CONTAINER)

# Access rootfs at $MOUNT_POINT
ls -la $MOUNT_POINT

# Unmount when done
buildah umount $CONTAINER
```

**Implementation**:

```python
import subprocess
from pathlib import Path

class BuildahImageHandler:
    def pull_image(self, image: str) -> str:
        """Pull OCI image and return container ID."""
        result = subprocess.run(
            ["buildah", "from", image],
            capture_output=True,
            text=True,
            check=True
        )
        return result.stdout.strip()

    def mount_image(self, container_id: str) -> Path:
        """Mount container and return mount point."""
        result = subprocess.run(
            ["buildah", "mount", container_id],
            capture_output=True,
            text=True,
            check=True
        )
        return Path(result.stdout.strip())

    def cleanup(self, container_id: str):
        """Unmount and remove container."""
        subprocess.run(["buildah", "umount", container_id], check=True)
        subprocess.run(["buildah", "rm", container_id], check=True)
```

### 3. Convert to qcow2

Convert the extracted rootfs into a qcow2 base image:

```bash
# Create empty qcow2 image (10GB)
qemu-img create -f qcow2 base.qcow2 10G

# Format with ext4
virt-format --filesystem=ext4 -a base.qcow2

# Copy rootfs into image
virt-copy-in -a base.qcow2 $MOUNT_POINT/* /

# Or use guestfish for more control
guestfish -a base.qcow2 -i <<EOF
  copy-in $MOUNT_POINT/* /
  sync
EOF
```

**Optimized Implementation**:

```python
import subprocess
from pathlib import Path

class QcowImageConverter:
    def __init__(self, storage_dir: Path):
        self.storage_dir = storage_dir
        self.storage_dir.mkdir(parents=True, exist_ok=True)

    def create_base_image(
        self,
        rootfs_path: Path,
        output_path: Path,
        size: str = "10G"
    ) -> Path:
        """Create qcow2 base image from rootfs."""

        # Create qcow2 image
        subprocess.run([
            "qemu-img", "create",
            "-f", "qcow2",
            str(output_path),
            size
        ], check=True)

        # Format with ext4
        subprocess.run([
            "virt-format",
            "--filesystem=ext4",
            "-a", str(output_path)
        ], check=True)

        # Copy rootfs
        subprocess.run([
            "virt-copy-in",
            "-a", str(output_path),
            f"{rootfs_path}/*",
            "/"
        ], check=True)

        # Optimize image (compress, remove unused space)
        self.optimize_image(output_path)

        return output_path

    def optimize_image(self, image_path: Path):
        """Optimize qcow2 image size."""
        subprocess.run([
            "qemu-img", "convert",
            "-f", "qcow2",
            "-O", "qcow2",
            "-c",  # Compress
            str(image_path),
            f"{image_path}.tmp"
        ], check=True)

        image_path.rename(f"{image_path}.bak")
        Path(f"{image_path}.tmp").rename(image_path)
        Path(f"{image_path}.bak").unlink()
```

## Image Caching Strategy

### Checksum Calculation

Generate a stable checksum from the OCI image manifest to identify unique images:

```python
import hashlib
import json
from pathlib import Path
from typing import Dict, Any

class ImageCache:
    def __init__(self, cache_dir: Path):
        self.cache_dir = cache_dir
        self.cache_dir.mkdir(parents=True, exist_ok=True)
        self.manifest_cache = self.cache_dir / "manifests"
        self.image_cache = self.cache_dir / "images"
        self.manifest_cache.mkdir(exist_ok=True)
        self.image_cache.mkdir(exist_ok=True)

    def calculate_manifest_checksum(self, image: str) -> str:
        """
        Calculate SHA256 checksum from OCI image manifest.

        The manifest includes:
        - Layer digests (content-addressable)
        - Config digest
        - Platform information

        This ensures different image versions get different checksums.
        """
        # Get manifest using skopeo
        result = subprocess.run([
            "skopeo", "inspect",
            "--raw",
            f"docker://{image}"
        ], capture_output=True, text=True, check=True)

        manifest = json.loads(result.stdout)

        # Create stable representation
        # Include config digest and all layer digests
        stable_data = {
            "config": manifest.get("config", {}).get("digest", ""),
            "layers": [
                layer.get("digest", "")
                for layer in manifest.get("layers", [])
            ],
            "platform": manifest.get("platform", {})
        }

        # Calculate SHA256
        manifest_json = json.dumps(stable_data, sort_keys=True)
        checksum = hashlib.sha256(manifest_json.encode()).hexdigest()

        return checksum

    def get_cached_image(self, image: str) -> Path | None:
        """Get cached qcow2 image if it exists."""
        checksum = self.calculate_manifest_checksum(image)
        cached_path = self.image_cache / f"{checksum}.qcow2"

        if cached_path.exists():
            return cached_path
        return None

    def store_image(self, image: str, qcow2_path: Path) -> Path:
        """Store qcow2 image in cache."""
        checksum = self.calculate_manifest_checksum(image)
        cache_path = self.image_cache / f"{checksum}.qcow2"

        # Copy or move image to cache
        if qcow2_path != cache_path:
            subprocess.run([
                "cp", "--reflink=auto",  # Use COW copy if supported
                str(qcow2_path),
                str(cache_path)
            ], check=True)

        # Store metadata
        metadata = {
            "image": image,
            "checksum": checksum,
            "size": cache_path.stat().st_size,
            "created": cache_path.stat().st_ctime
        }

        metadata_path = self.manifest_cache / f"{checksum}.json"
        metadata_path.write_text(json.dumps(metadata, indent=2))

        return cache_path
```

### Cache Directory Structure

```
$CACHE_DIR/
├── manifests/          # Image metadata
│   ├── abc123...json   # Manifest checksum → metadata
│   └── def456...json
├── images/             # Base qcow2 images
│   ├── abc123...qcow2  # Checksum-based filenames
│   └── def456...qcow2
└── overlays/           # Per-VM overlay images
    ├── vm-001.qcow2
    ├── vm-002.qcow2
    └── ...
```

### Deduplication Logic

Images with the same content are automatically deduplicated through checksum-based storage:

```python
class ImageManager:
    def __init__(self, cache: ImageCache):
        self.cache = cache
        self.buildah = BuildahImageHandler()
        self.converter = QcowImageConverter(cache.image_cache)

    async def prepare_base_image(self, image: str) -> Path:
        """
        Prepare base qcow2 image, using cache if available.

        Returns path to base image (backing file).
        """
        # Check cache first
        cached = self.cache.get_cached_image(image)
        if cached:
            print(f"Using cached image: {cached}")
            return cached

        print(f"Pulling and converting image: {image}")

        # Pull image with Buildah
        container_id = self.buildah.pull_image(image)

        try:
            # Mount image
            mount_point = self.buildah.mount_image(container_id)

            # Convert to qcow2
            temp_qcow2 = self.cache.image_cache / f"temp-{container_id}.qcow2"
            self.converter.create_base_image(mount_point, temp_qcow2)

            # Store in cache
            base_image = self.cache.store_image(image, temp_qcow2)

            # Cleanup temp
            if temp_qcow2.exists():
                temp_qcow2.unlink()

            return base_image

        finally:
            # Always cleanup Buildah container
            self.buildah.cleanup(container_id)
```

## Copy-on-Write (COW) Implementation

### qcow2 Backing Files

Use qcow2 backing files to create lightweight per-VM overlay images:

```python
from pathlib import Path
import subprocess

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

### Example: Creating VM Images

```python
# 1. Prepare base image (cached)
base_image = await image_manager.prepare_base_image("ubuntu:22.04")
# Result: /cache/images/abc123...qcow2 (5GB)

# 2. Create overlay for VM-1
vm1_overlay = cow_manager.create_overlay(base_image, "vm-001")
# Result: /cache/overlays/vm-001.qcow2 (initially ~200KB)

# 3. Create overlay for VM-2 (shares same base)
vm2_overlay = cow_manager.create_overlay(base_image, "vm-002")
# Result: /cache/overlays/vm-002.qcow2 (initially ~200KB)

# Both VMs read from the same base image
# Only write differences to their overlays
# Disk space: 5GB + 200KB + 200KB ≈ 5GB (not 15GB!)
```

### Handling Image Updates

When a base image is updated, handle existing VMs gracefully:

```python
class ImageUpdateHandler:
    def __init__(
        self,
        image_manager: ImageManager,
        cow_manager: COWImageManager
    ):
        self.image_manager = image_manager
        self.cow_manager = cow_manager

    async def update_base_image(self, image: str) -> tuple[Path, Path]:
        """
        Update base image, return (old_base, new_base).
        """
        # Get current cached image
        old_base = self.image_manager.cache.get_cached_image(image)

        # Force re-pull by clearing cache entry
        if old_base:
            checksum = self.image_manager.cache.calculate_manifest_checksum(image)
            manifest_file = self.image_manager.cache.manifest_cache / f"{checksum}.json"
            manifest_file.unlink(missing_ok=True)

        # Pull new version
        new_base = await self.image_manager.prepare_base_image(image)

        return old_base, new_base

    def migrate_vm_to_new_base(
        self,
        vm_id: str,
        old_overlay: Path,
        new_base: Path
    ) -> Path:
        """
        Migrate VM overlay to use new base image.

        Strategy: Commit overlay changes, then rebase.
        """
        # 1. Commit current overlay to standalone image
        standalone = self.cow_manager.overlay_dir / f"{vm_id}-standalone.qcow2"
        subprocess.run([
            "qemu-img", "convert",
            "-f", "qcow2",
            "-O", "qcow2",
            str(old_overlay),
            str(standalone)
        ], check=True)

        # 2. Create new overlay with new base
        new_overlay = self.cow_manager.overlay_dir / f"{vm_id}-new.qcow2"
        self.cow_manager.create_overlay(new_base, f"{vm_id}-new")

        # 3. Copy differences from standalone to new overlay
        # This requires guest filesystem operations
        # (mount both, rsync changes, or use virt-diff)

        # 4. Replace old overlay
        old_overlay.rename(f"{old_overlay}.old")
        new_overlay.rename(old_overlay)

        # 5. Cleanup
        standalone.unlink()

        return old_overlay

    async def rolling_update_strategy(
        self,
        image: str,
        active_vms: list[str]
    ):
        """
        Rolling update strategy: new VMs use new base, keep old VMs on old base.
        """
        old_base, new_base = await self.update_base_image(image)

        # New VMs automatically use new base
        # Old VMs continue using old base (still valid)

        # Old base is only deleted when no VMs reference it (ref counting)
        print(f"Updated base image: {old_base} -> {new_base}")
        print(f"Active VMs ({len(active_vms)}) continue using old base")
        print(f"New VMs will use new base: {new_base}")
```

## Storage Layout and Organization

### Directory Structure

```
$STORAGE_ROOT/
├── cache/
│   ├── manifests/              # Image metadata
│   ├── images/                 # Base qcow2 images (backing files)
│   └── buildah/                # Buildah storage
│       └── overlay/
├── vms/
│   ├── vm-001/
│   │   ├── overlay.qcow2       # VM's COW overlay
│   │   ├── config.json         # VM configuration
│   │   └── metadata.json       # Runtime metadata
│   ├── vm-002/
│   │   ├── overlay.qcow2
│   │   ├── config.json
│   │   └── metadata.json
│   └── ...
├── temp/                       # Temporary images during creation
└── trash/                      # Soft-deleted images (for recovery)
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
        root = root or Path.home() / ".cocoon"
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
            self.cache_dir / "buildah",
            self.vm_dir,
            self.temp_dir,
            self.trash_dir
        ]:
            path.mkdir(parents=True, exist_ok=True)
```

## Cleanup Strategy

### Reference Counting

Track which VMs reference which base images:

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

### Garbage Collection

Implement automatic cleanup of unused resources:

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

## Example Workflows

### Workflow 1: Create New VM

```python
async def create_vm(image: str, vm_id: str) -> Path:
    """
    Complete workflow to create a new VM from an OCI image.
    """
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

### Workflow 2: Delete VM

```python
def delete_vm(vm_id: str):
    """
    Complete workflow to delete a VM and cleanup resources.
    """
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

### Workflow 3: High-Concurrency VM Creation

```python
async def create_vm_pool(image: str, count: int) -> list[Path]:
    """
    Create multiple VMs concurrently from the same base image.
    """
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

### Workflow 4: Image Update with Rolling Migration

```python
async def update_image_rolling(image: str, active_vms: list[str]):
    """
    Update base image with rolling migration strategy.
    """
    storage = StorageConfig.default()
    cache = ImageCache(storage.cache_dir)
    image_mgr = ImageManager(cache)
    update_handler = ImageUpdateHandler(image_mgr, cow_mgr)

    # 1. Pull new version
    old_base, new_base = await update_handler.update_base_image(image)

    print(f"Updated: {old_base} -> {new_base}")

    # 2. New VMs use new base automatically
    print("New VMs will use new base image")

    # 3. Existing VMs continue on old base
    print(f"{len(active_vms)} active VMs continue using old base")

    # 4. Old base will be garbage collected when all VMs shut down
    print("Old base will be cleaned up when no longer referenced")
```

### Workflow 5: Scheduled Garbage Collection

```python
import asyncio

async def scheduled_gc_loop():
    """
    Run garbage collection periodically.
    """
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

## Performance Optimization

### Parallel Image Processing

```python
async def prepare_multiple_images(images: list[str]) -> dict[str, Path]:
    """Prepare multiple base images in parallel."""
    storage = StorageConfig.default()
    cache = ImageCache(storage.cache_dir)
    image_mgr = ImageManager(cache)

    async def prepare_one(image: str) -> tuple[str, Path]:
        base = await image_mgr.prepare_base_image(image)
        return image, base

    results = await asyncio.gather(*[prepare_one(img) for img in images])
    return dict(results)
```

### Copy-on-Write Optimization

Use filesystem-level COW when available:

```bash
# Use reflink for instant copies on btrfs/xfs
cp --reflink=auto base.qcow2 copy.qcow2

# zstd compression for base images
qemu-img convert -f qcow2 -O qcow2 -o compression_type=zstd input.qcow2 output.qcow2
```

## Summary

This strategy provides:

1. **Efficiency**: Checksum-based caching eliminates duplicate downloads
2. **Reusability**: COW overlays allow many VMs to share base images
3. **Safety**: Reference counting prevents premature deletion
4. **Automation**: Garbage collection cleans up unused resources
5. **Scalability**: Supports high-concurrency VM creation
6. **Flexibility**: Handles image updates gracefully

The combination of Buildah, qcow2 backing files, and intelligent caching delivers an optimal solution for managing OCI images in a high-concurrency AI Agent sandbox environment.

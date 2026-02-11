# Cocoon Overview

## ⚠️ Supported Image Contract

**IMPORTANT**: Cocoon does NOT support regular container images (e.g., `ubuntu:latest`, `python:3.11`, `node:20`).

### What Cocoon Supports

Cocoon requires **bootable VM images** with a complete operating system, not application containers:

**1. Bootable OCI Images** (Custom-built OS images packaged as OCI):
- **MUST contain**: kernel (`/boot/vmlinuz*`), initrd/initramfs, init system (`/sbin/init` → systemd)
- **MUST have**: GRUB bootloader in ESP (EFI System Partition), GPT partition table
- **SHOULD have**: cloud-init (recommended for VM initialization with Cocoon metadata server)
- **Reality**: Building bootable OCI images is complex - see [11-bootable-oci-build.md](./11-bootable-oci-build.md)

**2. Cloud Hypervisor Native Cloud Images** (recommended, faster):
- Standard cloud images in qcow2 format (Ubuntu Cloud, Fedora Cloud, Debian Cloud)
- Pre-configured for cloud-init and PVH/UEFI boot
- Direct boot without OCI conversion overhead

### Why Regular Container Images Don't Work

Container images like `ubuntu:latest` are **application filesystems**, not bootable operating systems:
- ❌ No kernel or initrd
- ❌ No bootloader (GRUB/systemd-boot)
- ❌ Missing system services (systemd, udev)
- ❌ Not designed for VM boot process

**If you try to use a container image, you will get**: `ERROR: Bootability check failed: kernel not found`

### Recommended Images

**Cloud Images (Recommended)**:
```bash
# Ubuntu 22.04 Cloud Image
cocoon image pull https://cloud-images.ubuntu.com/releases/22.04/release/ubuntu-22.04-server-cloudimg-amd64.img

# Fedora Cloud Image
cocoon image pull https://download.fedoraproject.org/pub/fedora/linux/releases/39/Cloud/x86_64/images/Fedora-Cloud-Base-39-*.qcow2
```

**Bootable OCI Images** (Advanced - requires custom build process):
```bash
# Example: Pull a pre-built bootable OCI image (if available)
cocoon image pull myorg/ubuntu-bootable:22.04

# Building your own: See docs/11-bootable-oci-build.md (Phase 2 - planned)
# Currently: Use cloud images (recommended) instead of building custom OCI
```

**Reality Check**: Building bootable OCI images is complex (kernel installation, GRUB setup, ESP partition).
For Phase 1, **we recommend using cloud images** instead. See [11-bootable-oci-build.md](./11-bootable-oci-build.md) for details.

### How to Get Bootable Images

**Option 1: Cloud Images (Recommended for Phase 1)**:
- Download: Ubuntu Cloud, Fedora Cloud, Debian Cloud (qcow2 format)
- Pre-configured with kernel, bootloader, systemd, cloud-init
- Works immediately with Cocoon (no conversion needed)
- Sources:
  - Ubuntu: https://cloud-images.ubuntu.com/
  - Fedora: https://fedoraproject.org/cloud/download
  - Debian: https://cloud.debian.org/images/cloud/

**Option 2: Build Custom Bootable OCI (Advanced - Phase 2)**:
- Requires multi-stage Dockerfile with package installation
- Must install: kernel, initrd, systemd, GRUB, cloud-init
- Must configure: ESP partition, GRUB config, bootloader installation
- See [11-bootable-oci-build.md](./11-bootable-oci-build.md) for build process

**What DOESN'T Work**:
- ❌ Regular container images: `ubuntu:latest`, `python:3.11`, `node:20`
- ❌ Application containers from Docker Hub (no kernel/bootloader)
- ❌ Minimal base images like `alpine:latest` (missing systemd, GRUB)

---

## Project Motivation

Modern VM workloads and development environments face a challenging trade-off between isolation, performance, and usability:

- **Docker/Podman**: Fast startup and familiar tooling, but only container-level isolation with shared kernel
- **Traditional VMs (QEMU/KVM)**: Strong VM-level security boundaries but slow startup and heavyweight operation
- **Cloud Hypervisor**: Production-grade VMM with strong isolation and fast startup, but requires manual image preparation

**Cocoon bridges this gap** by combining Cloud Hypervisor's maturity and performance with streamlined image management, providing:
- **Strong isolation**: VM-level security boundaries via KVM
- **Fast startup**: Sub-second VM creation using microVM technology
- **High concurrency**: Efficient management of hundreds of VMs simultaneously
- **Dual image support**: Cloud Hypervisor native cloud images (recommended) + Bootable OCI images
- **Simplified workflow**: Image conversion and caching for easy VM provisioning

## Architecture Overview

Cocoon's architecture integrates several components to provide seamless OCI-to-VM conversion:

```
┌─────────────────────────────────────────────────────────────┐
│                     Cocoon CLI                               │
│          (create/start/stop/delete/inspect)                  │
└──────────────────┬──────────────────────────────────────────┘
                   │
        ┌──────────┴──────────┐
        │                     │
        v                     v
┌───────────────┐     ┌──────────────────────────┐
│   Buildah     │     │   Cloud Hypervisor       │
│  (OCI Pull &  │     │   (VMM with REST API)    │
│   Extract)    │     │                          │
└───────┬───────┘     └────────┬─────────────────┘
        │                      │
        v                      │
┌───────────────────┐          │
│  qemu-img +       │          │
│  libguestfs       │          │
│  (OCI → qcow2)    │          │
└─────────┬─────────┘          │
          │                    │
          v                    v
┌─────────────────────────────────────────────┐
│         qcow2 Storage Layer                  │
│  ┌──────────────┐    ┌──────────────────┐  │
│  │ Base Images  │───▶│  VM Overlays     │  │
│  │ (checksum-   │    │  (COW per-VM)    │  │
│  │  cached)     │    │                  │  │
│  └──────────────┘    └──────────────────┘  │
└─────────────────────────────────────────────┘
          │
          v
┌─────────────────────────────────────────────┐
│      Reference Counter + GC                  │
│   (Automatic cleanup of unused images)       │
└─────────────────────────────────────────────┘
```

### Component Flow

1. **User Request**: `cocoon create --image ubuntu:22.04 --name myvm`
2. **Image Pull**: Buildah downloads OCI image from registry (if not cached)
3. **Image Conversion**: qemu-img converts OCI rootfs to qcow2 base image with checksum-based filename
4. **Storage Creation**: qcow2 COW overlay created from base image (instant, ~200KB initial size)
5. **VM Launch**: Cloud Hypervisor boots VM using UEFI firmware and overlay disk
6. **Resource Tracking**: Reference counter tracks which VMs use which base images

## Key Design Decisions

### 1. UEFI Boot for OS Compatibility

**Decision**: Use UEFI firmware (OVMF) instead of PVH for VM boot.

**Rationale**:
- Standard Linux distributions (Ubuntu, Fedora, Debian) require UEFI
- Supports secure boot, GPT partitions, and larger disk sizes
- Broader OS compatibility without custom kernel builds
- Trade-off: ~500ms boot time vs <100ms for PVH (acceptable for most VM workloads)

### 2. Per-VM Cloud Hypervisor Process for Isolation

**Decision**: Run separate Cloud Hypervisor process for each VM, not shared daemon.

**Rationale**:
- **Strong isolation**: VM failures don't affect other VMs
- **Resource limits**: Each process can be independently controlled
- **Security**: Reduced blast radius for security vulnerabilities
- **Simplicity**: No need for complex multi-tenant process management

### 3. qcow2 Copy-on-Write for Efficiency

**Decision**: Use qcow2 backing files for per-VM storage instead of full disk copies.

**Benefits**:
- **Disk space**: 100 VMs from 5GB image uses ~5.02GB total (99% savings)
- **Creation speed**: Instant overlay creation regardless of base image size
- **Isolation**: Each VM has independent writable overlay
- **Performance**: Only modified blocks stored in overlay

**Example**:
```
Base image:     ubuntu-22.04-abc123...qcow2  (5GB, read-only, shared)
VM overlays:    vm-001-overlay.qcow2 (200KB, writable)
                vm-002-overlay.qcow2 (200KB, writable)
                ... (98 more VMs)
```

### 4. Checksum-Based Image Caching

**Decision**: Use manifest-based checksums for image deduplication, not image name/tag.

**Rationale**:
- **Content-addressable**: Same image content = same cached qcow2, regardless of tag
- **Version tracking**: `ubuntu:22.04` updated upstream? New checksum = new cache entry
- **Automatic deduplication**: Multiple pulls of same content reuse cache
- **Space efficiency**: No duplicate conversions for identical images

**Implementation**:
1. Calculate SHA256 of (config_digest + all_layer_digests + platform)
2. Store qcow2 as `{checksum}.qcow2` in cache directory
3. Reference counter tracks VM usage of each base image

## Phase 1 vs Phase 2 Scope

### Phase 1: Core Functionality (Current Focus)

**In Scope**:
- ✅ Cloud Hypervisor installation and setup documentation
- ✅ OCI image pull, cache, and qcow2 conversion
- ✅ VM lifecycle management (create/start/stop/delete)
- ✅ Copy-on-write storage with backing files
- ✅ Reference counting and garbage collection
- ✅ UEFI firmware handling and configuration
- ✅ CLI tool with Docker-like interface
- ✅ Interface-driven Go architecture (following core project patterns)

**Implementation Timeline**: 10 weeks

### Phase 2: Advanced Features (Future Work)

**Explicitly Deferred**:
- ❌ Network configuration (TAP devices, bridges, network policies)
- ❌ Live migration between hosts
- ❌ GPU passthrough for AI workloads
- ❌ Kubernetes integration (CRI plugin)
- ❌ Multi-host orchestration

**Why Deferred**:
- Phase 1 delivers complete, production-ready VM lifecycle management
- Network configuration adds significant complexity and testing burden
- Advanced features can be added incrementally without breaking existing functionality

## Goals and Non-Goals

### Goals

1. **Easy Installation**: Comprehensive Cloud Hypervisor setup with prerequisites, verification, and troubleshooting
2. **OCI Native**: Pull and use any OCI image (ubuntu:22.04, python:3.11, custom images) as VM root filesystem
3. **Efficient Storage**: Checksum-based caching eliminates duplicate conversions
4. **Space Optimization**: qcow2 COW allows hundreds of VMs from single base image
5. **Automatic Cleanup**: Garbage collection removes unused base images and orphaned overlays
6. **Firmware Automation**: Download, configure, and manage UEFI/PVH firmware automatically
7. **Production Architecture**: Follow proven patterns from core project (interfaces, factories, YAML config)
8. **Intuitive CLI**: Docker-like commands (create, start, stop, delete, inspect, image pull)

### Non-Goals (Phase 1)

- **Network Configuration**: Explicitly deferred to Phase 2
- **Live Migration**: Out of scope for Phase 1
- **GPU Passthrough**: Future consideration
- **Kubernetes Integration**: Future consideration
- **Multi-host Orchestration**: Future consideration

## Use Cases

Cocoon is a general-purpose lightweight VM manager. Common use cases include:

### Development and Testing Environments

**Features**:
- Spin up isolated VMs from bootable images
- Efficient disk usage with copy-on-write overlays
- Quick provisioning and cleanup
- Strong isolation for testing untrusted code

### Sandboxed Workloads

**Features**:
- Strong isolation for untrusted code execution
- Fast VM creation for on-demand workloads
- High concurrency for parallel operations
- Support for various workload types (including AI agents, build jobs, etc.)

### Infrastructure and CI/CD

**Features**:
- Consistent VM environments from standardized images
- Fast provisioning for build and test pipelines
- Multi-tenant isolation with VM boundaries
- Resource efficiency for large-scale operations

## Technology Stack

- **Hypervisor**: Cloud Hypervisor (Rust-based VMM, production-grade)
- **Language**: Go 1.25+ (interface-driven, factory pattern)
- **OCI Tools**: Buildah (daemonless, rootless-capable)
- **Storage**: qcow2 via qemu-img and libguestfs
- **Firmware**: OVMF (UEFI) or rust-hypervisor-firmware (PVH)
- **CLI Framework**: urfave/cli/v2
- **Configuration**: YAML with sensible defaults

## Next Steps

1. **Read 01-installation.md**: Detailed Cloud Hypervisor installation guide
2. **Read 02-architecture.md**: Deep dive into code architecture and interfaces
3. **Read 03-implementation.md**: Step-by-step implementation plan with code examples

# Cocoon Overview

**Version**: 1.0
**Status**: Implemented
**Phase**: Phase 1
**Last Updated**: 2026-02-15

## ⚠️ Supported Image Contract

**IMPORTANT**: Cocoon does NOT support regular container images (e.g., `ubuntu:latest`, `python:3.11`, `node:20`).

### What Cocoon Supports

Cocoon requires **bootable VM images** with a complete operating system, not application containers:

**1. Bootable OCI Images** (Custom-built OS images packaged as OCI):
- **MUST contain**: kernel (`/boot/vmlinuz*`), initrd/initramfs, init system (`/sbin/init` → systemd)
- **MUST have**: GRUB bootloader in ESP (EFI System Partition), GPT partition table
- **Guest initialization**: SSH keys, users, and hostname setup is the user's responsibility. Cocoon does not depend on cloud-init.
- **Reality**: Building bootable OCI images is complex - see [11-bootable-oci-build.md](./11-bootable-oci-build.md)

**2. Cloud Hypervisor Native Cloud Images** (recommended, faster):
- Standard cloud images in qcow2 format (Ubuntu Cloud, Fedora Cloud, Debian Cloud)
- Pre-configured for UEFI boot
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

**CI/Verified Images**: For deterministic testing, Cocoon provides pinned reference images with fixed digests.
See [04-oci-conversion.md § 10 Verified Images](./04-oci-conversion.md#10-verified-images-ci-reference) for the CI verification matrix.

### How to Get Bootable Images

**Option 1: Cloud Images (Recommended for Phase 1)**:
- Download: Ubuntu Cloud, Fedora Cloud, Debian Cloud (qcow2 format)
- Pre-configured with kernel, bootloader, systemd
- Works immediately with Cocoon (no conversion needed)
- Sources:
  - Ubuntu: https://cloud-images.ubuntu.com/
  - Fedora: https://fedoraproject.org/cloud/download
  - Debian: https://cloud.debian.org/images/cloud/

**Option 2: Build Custom Bootable OCI (Advanced - Phase 2)**:
- Requires multi-stage Dockerfile with package installation
- Must install: kernel, initrd, systemd, GRUB (cloud-init is optional for the user's use case)
- Must configure: ESP partition, GRUB config, bootloader installation
- See [11-bootable-oci-build.md](./11-bootable-oci-build.md) for build process

**What DOESN'T Work**:
- ❌ Regular container images: `ubuntu:latest`, `python:3.11`, `node:20`
- ❌ Application containers from Docker Hub (no kernel/bootloader)
- ❌ Minimal base images like `alpine:latest` (missing systemd, GRUB)

---

## Terminology

| Term | Definition |
|------|-----------|
| **cloud image** | Pre-built qcow2/img VM disk from a distro vendor (Ubuntu Cloud, Fedora Cloud, Debian Cloud). Ready to boot without conversion. |
| **bootable OCI image** | Custom-built OCI container image containing kernel, bootloader, systemd. Requires OCI→qcow2 conversion. |
| **base image** | Cached qcow2 file in `/var/lib/cocoon/cache/images/`. Either a downloaded cloud image or a converted OCI image. Content-addressed by checksum. |
| **overlay** | Per-VM copy-on-write disk at `/var/lib/cocoon/vms/{vm-id}/overlay.qcow2`. Backed by a base image. |
| **vm_id** | Internal primary key (`vm-{ulid}`). Never reused. Used in directory names, logs, locks. |
| **name** | User-facing VM alias. Globally unique. Optional on create. |
| **vm-ref** | CLI argument that accepts either vm_id or name. Resolved by the CLI. |
| **firmware** | Binary loaded by Cloud Hypervisor at boot. UEFI: `CLOUDHV.fd` for cloud images. Phase 2 will add direct kernel boot (`payload.kernel`) for OCI VM images. |

**Resource Units**:
- CLI accepts human-readable units: `512M`, `1G`, `2G`, `10G`
- config.json stores: `memory_mb` as integer (megabytes), `disk_size` as string (`"10G"`), `cpus` as integer
- metadata.json stores timestamps as RFC 3339 strings, durations as strings

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

```
┌──────────────────────────────────────────────────────────────────┐
│                          Cocoon CLI                               │
│   create/start/stop/kill/delete/inspect/ps/logs/doctor            │
└───────────────────────────┬──────────────────────────────────────┘
                            │
          ┌─────────────────┼─────────────────────┐
          │                 │                     │
          v                 v                     v
┌─────────────────┐ ┌──────────────────┐ ┌──────────────────────┐
│  Image Pipeline │ │   VM Manager     │ │  CNI Network Mgr [2] │
│  (Pull/Convert) │ │  (State Machine) │ │  (TAP/Bridge/IPAM)   │
└────────┬────────┘ └────────┬─────────┘ └──────────┬───────────┘
         │                   │                      │
         v                   v                      v
┌─────────────────┐ ┌──────────────────┐ ┌──────────────────────┐
│  Buildah/Skopeo │ │ Cloud Hypervisor │ │  CNI Plugins         │
│  qemu-img       │ │ (per-VM process) │ │  (bridge/macvlan/    │
│  libguestfs     │ │  REST API        │ │   host-local/dhcp)   │
└────────┬────────┘ └────────┬─────────┘ └──────────────────────┘
         │                   │
         v                   v
┌──────────────────────────────────────────────────────────────────┐
│                     qcow2 Storage Layer                          │
│  Base Images (checksum-cached) ──▶ VM Overlays (COW per-VM)      │
│  Reference Counter + GC + Trash                                  │
└──────────────────────────────────────────────────────────────────┘

[2] = Phase 2 planned feature
```

### Component Flow

1. **User Request**: `cocoon create ubuntu-22.04-cloudimg --name myvm`
2. **Image Pull**: Buildah downloads OCI image from registry (if not cached)
3. **Image Conversion**: qemu-img converts OCI rootfs to qcow2 base image with checksum-based filename
4. **Storage Creation**: qcow2 COW overlay created from base image (instant, ~200KB initial size)
5. **VM Launch**: Cloud Hypervisor boots VM using UEFI firmware and overlay disk
6. **Resource Tracking**: Reference counter tracks which VMs use which base images

## Key Design Decisions

### 1. Boot Strategy: UEFI for Cloud Images (Phase 1), Direct Kernel Boot for OCI (Phase 2)

**Decision**: Phase 1 supports UEFI boot with CLOUDHV.fd for cloud images (qcow2/URL). Direct kernel boot (`payload.kernel` + `payload.initramfs` + `payload.cmdline`) for OCI VM images is designed in [docs/04.1-oci-vm-images.md](./04.1-oci-vm-images.md) and planned for Phase 2.

**Rationale**:
- **UEFI (Cloud Images — Phase 1)**: Broadest compatibility with cloud images, supports secure boot, CH project recommended firmware
- **Direct Kernel Boot (OCI — Phase 2)**: Will boot extracted kernel and initramfs directly via Cloud Hypervisor's `payload.kernel`, bypassing the need for a bootloader in the image. This is not yet implemented.
- UEFI default for cloud images eliminates boot failures from images that lack specific kernel layouts

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
- **Version tracking**: `myorg/ubuntu-bootable:22.04` updated upstream? New checksum = new cache entry
- **Automatic deduplication**: Multiple pulls of same content reuse cache
- **Space efficiency**: No duplicate conversions for identical images

**Implementation**:
1. Calculate SHA256 of (config_digest + all_layer_digests + platform)
2. Store qcow2 as `{checksum}_{arch}.qcow2` in cache directory
3. Reference counter tracks VM usage of each base image

## Phased Roadmap

### Phase 1: Core VM Management (Implemented)

Phase 1 delivers a complete, production-ready VM lifecycle management system.

- ✅ Cloud Hypervisor installation, setup, and firmware management
- ✅ OCI image pull, cache, and qcow2 conversion pipeline
- ✅ Manifest ref-index for `IMAGE_REF` local resolution (`image inspect` / `image rm` / `image verify`)
- ✅ VM lifecycle (create/start/stop/kill/delete) with state machine
- ✅ Copy-on-write storage with qcow2 backing files
- ✅ Reference counting and garbage collection
- ✅ UEFI boot for cloud images (direct kernel boot for OCI VM images is Phase 2)
- ✅ TPM 2.0 emulation via swtpm (`--tpm` flag)
- ✅ Reconciliation and crash recovery (`cocoon doctor`)
- ✅ CLI tool with Docker-like interface (`cocoon run/ps/logs/inspect`)
- ✅ Concurrency control with file-based lock hierarchy

Design docs: [00-overview](./00-overview.md) through [11-bootable-oci-build](./11-bootable-oci-build.md)

### Phase 2: Advanced Features (Planned)

Phase 2 adds interactive access, VM state management, fast provisioning, and networking.

- **Console** ([docs/12](./12-console.md)): Interactive bidirectional PTY console via `cocoon console`, dual-port strategy (serial for logs, virtio-console for interactive access), SSH-style escape sequences
- **Pause/Resume** ([docs/13](./13-pause-resume.md)): New PAUSED state in the VM state machine, vCPU freeze/unfreeze via Cloud Hypervisor `vm.pause`/`vm.resume` API, prerequisite for checkpoint/restore
- **Warm Start** ([docs/15](./15-warm-start.md)): VM checkpoint and restore for sub-second creation (~200ms vs 5-30s cold boot), golden checkpoint workflow, snapshot management with GC integration
- **CNI Networking** ([docs/16](./16-networking.md)): CNI plugin integration for VM network attachment, TAP device bridging into VMs, IPAM (host-local/dhcp), port forwarding via portmap plugin, DNS injection

### Phase 3: Hardware and Ecosystem (Future)

Phase 3 extends Cocoon to hardware-accelerated workloads and broader ecosystem integration.

- **Device Passthrough** ([docs/14](./14-device-passthrough.md)): VFIO PCI device passthrough, IOMMU group validation, GPU convenience flags, hotplug support
- **Kubernetes Integration**: CRI plugin for using Cocoon VMs as Pod sandboxes (not yet designed)
- **Live Migration**: Cross-host VM migration (not yet designed)
- **Multi-host Orchestration**: Distributed VM scheduling (not yet designed)

## Goals and Non-Goals

### Goals

1. **Easy Installation**: Comprehensive Cloud Hypervisor setup with prerequisites, verification, and troubleshooting
2. **Cloud Images First**: Native support for cloud images (Ubuntu Cloud, Fedora Cloud) as primary path
3. **Bootable Images Only**: Support OCI images with strict bootability validation (kernel, initrd, bootloader required)
4. **Efficient Storage**: Checksum-based caching eliminates duplicate conversions
5. **Space Optimization**: qcow2 COW allows hundreds of VMs from single base image
6. **Automatic Cleanup**: Garbage collection removes unused base images and orphaned overlays
7. **Firmware Automation**: Download, configure, and manage UEFI firmware automatically
8. **Production Architecture**: Follow proven patterns from core project (interfaces, factories, JSON config)
9. **Intuitive CLI**: Docker-like commands (run, create, start, stop, delete, doctor, firmware)

### Non-Goals

- **Live Migration**: Cross-host VM migration (Phase 3+)
- **Kubernetes Integration**: CRI plugin for K8s Pod sandboxes (Phase 3+)
- **Multi-host Orchestration**: Distributed VM scheduling (Phase 3+)
- **Container Compatibility**: Cocoon is NOT a container runtime — regular container images are not supported

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

**Core (Phase 1)**:
- **Hypervisor**: Cloud Hypervisor (Rust-based VMM, production-grade)
- **Language**: Go 1.25+ (interface-driven, factory pattern)
- **OCI Tools**: Buildah (daemonless)
- **Storage**: qcow2 via qemu-img and libguestfs
- **Firmware**: OVMF (UEFI via CLOUDHV.fd) for cloud images; direct kernel boot for OCI VM images (Phase 2)
- **TPM**: swtpm (optional TPM 2.0 emulation)
- **CLI Framework**: urfave/cli/v2
- **Configuration**: JSON with sensible defaults

**Phase 2 Additions**:
- **Networking**: CNI plugins (bridge, macvlan, host-local IPAM, portmap)
- **Console**: virtio-console PTY via Cloud Hypervisor `vm.info` API
- **Checkpoint**: Cloud Hypervisor `vm.snapshot`/`vm.restore` API

## Deployment Strategy

### Privilege Model

Cocoon requires root privileges. All VM operations (hypervisor management, image conversion, storage) run as root.

### 30-Minute Getting Started Path

For quick evaluation:

1. **Install dependencies** (5 min):
   ```bash
   # Ubuntu
   sudo apt-get install -y cloud-hypervisor buildah skopeo qemu-utils ovmf

   # Add your user to kvm group
   sudo usermod -aG kvm $USER
   newgrp kvm
   ```

2. **Download a cloud image** (10 min):
   ```bash
   # Ubuntu 22.04 cloud image (pre-built qcow2, no conversion needed)
   mkdir -p ~/cocoon-images
   cd ~/cocoon-images
   wget https://cloud-images.ubuntu.com/releases/22.04/release/ubuntu-22.04-server-cloudimg-amd64.img
   ```

3. **Install cocoon binary** (5 min):
   ```bash
   # Build from source (replace with release URL when available)
   git clone https://github.com/CMGS/cocoon.git
   cd cocoon
   go build -o cocoon ./cmd/cocoon
   sudo mv cocoon /usr/local/bin/
   ```

4. **Run health check and install firmware** (2 min):
   ```bash
   # Verify installation
   cocoon doctor

   # Install UEFI firmware (explicit URL required)
   cocoon firmware install --uefi-url https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v50.0/CLOUDHV.fd
   ```

5. **Create and start a VM** (3 min):
   ```bash
   # Use cloud image directly (no OCI conversion)
   cocoon create ~/cocoon-images/ubuntu-22.04-server-cloudimg-amd64.img --name test-vm
   cocoon start test-vm
   ```

6. **Verify it works** (5 min):
   ```bash
   cocoon list
   cocoon inspect test-vm
   cocoon logs test-vm --follow
   cocoon stop test-vm
   cocoon delete test-vm
   ```

**Note**: This path uses cloud images directly, bypassing the OCI conversion pipeline. For OCI image support, you also need libguestfs tools.

## Document Index

### Phase 1: Core VM Management

| Doc | Title | Status |
|-----|-------|--------|
| [00-overview.md](./00-overview.md) | Cocoon Overview | Implemented |
| [01-boot-contract.md](./01-boot-contract.md) | Boot Contract Specification | Implemented |
| [02-installation.md](./02-installation.md) | Installation | Implemented |
| [03-hypervisor-integration.md](./03-hypervisor-integration.md) | Cloud Hypervisor Integration | Implemented |
| [04-oci-conversion.md](./04-oci-conversion.md) | OCI to qcow2 Conversion | Implemented |
| [05-storage-management.md](./05-storage-management.md) | Storage Management | Implemented |
| [06-concurrency.md](./06-concurrency.md) | Concurrency Design | Implemented |
| [07-vm-lifecycle.md](./07-vm-lifecycle.md) | VM Lifecycle Management | Implemented |
| [08-dependencies.md](./08-dependencies.md) | Dependencies and Requirements | Implemented |
| [09-cli-design.md](./09-cli-design.md) | CLI Design and Commands | Implemented |
| [10-implementation-roadmap.md](./10-implementation-roadmap.md) | Implementation Roadmap | Implemented |
| [11-bootable-oci-build.md](./11-bootable-oci-build.md) | Building Bootable OCI Images | Implemented |

### Phase 2: Advanced Features

| Doc | Title | Status |
|-----|-------|--------|
| [12-console.md](./12-console.md) | VM Console | Planned |
| [13-pause-resume.md](./13-pause-resume.md) | VM Pause and Resume | Planned |
| [15-warm-start.md](./15-warm-start.md) | VM Warm Start (Checkpoint/Restore) | Planned |
| [16-networking.md](./16-networking.md) | CNI Networking | Planned |

### Phase 3: Hardware and Ecosystem

| Doc | Title | Status |
|-----|-------|--------|
| [14-device-passthrough.md](./14-device-passthrough.md) | PCI Device Passthrough | Planned |

See [docs/future/](./future/) for feature requests not yet promoted to design documents.

## Next Steps

1. **Read [02-installation.md](./02-installation.md)**: Detailed Cloud Hypervisor installation guide
2. **Read [08-dependencies.md](./08-dependencies.md)**: Dependencies and permission models
3. **Read [04-oci-conversion.md](./04-oci-conversion.md)**: OCI to qcow2 conversion (requires root)

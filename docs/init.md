# RFC 001: Vibe - Cloud Hypervisor based MicroVM Manager with OCI Image Support

## Metadata

- **RFC Number**: 001
- **Title**: Vibe - Cloud Hypervisor based MicroVM Manager with OCI Image Support
- **Author**: Vibe RFC Team
- **Status**: Draft
- **Created**: 2026-02-11
- **Updated**: 2026-02-11

## Abstract

This RFC proposes the design and implementation of **Vibe**, a lightweight virtual machine management CLI tool built on Cloud Hypervisor with native OCI image support. Vibe bridges the gap between container images and virtual machines, enabling AI Agent sandboxes and other use cases to leverage OCI images (e.g., `ubuntu:latest`) directly as VM root filesystems with automatic qcow2 conversion, image caching, and copy-on-write optimization.

**Key Features**:
- Native OCI image support via Buildah integration
- Automated qcow2 conversion and caching with checksum-based deduplication
- Copy-on-Write (COW) VM disk management using qcow2 backing files
- Cloud Hypervisor REST API integration
- Go 1.25+ implementation following the proven patterns from the `core` project
- Interface-driven architecture for extensibility and testability

## Table of Contents

1. [Motivation](#motivation)
2. [Goals and Non-Goals](#goals-and-non-goals)
3. [Cloud Hypervisor Installation and Deployment](#cloud-hypervisor-installation-and-deployment)
4. [OCI Image Handling and qcow2 Management Strategy](#oci-image-handling-and-qcow2-management-strategy)
5. [CLI Architecture and Command Structure](#cli-architecture-and-command-structure)
6. [Implementation Timeline](#implementation-timeline)
7. [Security Considerations](#security-considerations)
8. [Future Work](#future-work)
9. [References](#references)

---

## Motivation

Modern AI Agent sandboxes and code execution platforms require:
- **Strong isolation**: VM-level security boundaries
- **Fast startup**: Sub-second VM creation
- **High concurrency**: Managing hundreds of VMs simultaneously
- **Familiar tooling**: Using existing OCI images (Docker Hub, ghcr.io, etc.)

Existing solutions present trade-offs:
- **Docker/Podman**: Fast but container-level isolation (shared kernel)
- **Traditional VMs (QEMU/KVM)**: Strong isolation but slow startup and heavyweight
- **BoxLite**: Excellent for AI sandboxes but young project (v0.5.x)
- **Cloud Hypervisor**: Production-grade VMM but lacks OCI image support

**Vibe** combines the strengths of Cloud Hypervisor's maturity and performance with the convenience of OCI images, providing a purpose-built tool for AI Agent sandbox and microVM use cases.

## Goals and Non-Goals

### Goals

1. ✅ **Easy Installation**: Provide comprehensive Cloud Hypervisor setup documentation
2. ✅ **OCI Image Support**: Pull, cache, and convert OCI images to qcow2 format
3. ✅ **Image Reuse**: Checksum-based caching to avoid redundant conversions
4. ✅ **Copy-on-Write**: Use qcow2 backing files for efficient disk management
5. ✅ **Resource Cleanup**: Automatic garbage collection of unused images and overlays
6. ✅ **Firmware Management**: Automated firmware download and configuration
7. ✅ **Production-Ready Architecture**: Follow proven patterns from the `core` project
8. ✅ **CLI Usability**: Intuitive Docker-like command interface

### Non-Goals (Phase 1)

- ❌ **Network Configuration**: Explicitly deferred (Phase 2)
- ❌ **Live Migration**: Not required for AI sandbox use case
- ❌ **GPU Passthrough**: Future consideration
- ❌ **Kubernetes Integration**: Future consideration
- ❌ **Multi-host Orchestration**: Future consideration

---

## Cloud Hypervisor Installation and Deployment

### Overview

Cloud Hypervisor is a production-grade Virtual Machine Monitor (VMM) built in Rust, designed for modern cloud workloads. This section provides comprehensive guidance on installing and deploying Cloud Hypervisor for AI Agent sandbox environments.

### Prerequisites

#### Linux Kernel Requirements

Cloud Hypervisor requires a modern Linux kernel with KVM support:

- **Minimum kernel version**: 5.6 or later
- **Recommended kernel version**: 5.10 LTS or newer
- **Architecture support**: x86_64 (Intel/AMD), aarch64 (ARM64)

Verify your kernel version:

```bash
uname -r
```

#### KVM Support Verification

Cloud Hypervisor relies on KVM (Kernel-based Virtual Machine) for hardware virtualization. Verify KVM support with the following checks:

**1. Check CPU virtualization support:**

```bash
# For Intel processors (look for 'vmx')
grep -E 'vmx' /proc/cpuinfo

# For AMD processors (look for 'svm')
grep -E 'svm' /proc/cpuinfo
```

**2. Verify KVM modules are loaded:**

```bash
lsmod | grep kvm
```

Expected output should include:
- `kvm_intel` (for Intel CPUs) or `kvm_amd` (for AMD CPUs)
- `kvm` (core KVM module)

**3. Check KVM device accessibility:**

```bash
ls -l /dev/kvm
```

The device should be accessible (typically requires membership in the `kvm` group):

```bash
# Add current user to kvm group
sudo usermod -aG kvm $USER
# Log out and back in for changes to take effect
```

**4. Verify virtualization is enabled in BIOS/UEFI:**

If `/dev/kvm` does not exist, ensure virtualization is enabled in your system's BIOS/UEFI settings. Look for options like:
- Intel VT-x / VT-d
- AMD-V / AMD SVM Mode

#### System Requirements

**Minimum specifications:**
- CPU: 2 cores with hardware virtualization support
- RAM: 4 GB (2 GB for host OS + 2 GB for VMs)
- Disk: 10 GB free space
- OS: Linux distribution with kernel 5.6+

**Recommended specifications for AI Agent sandbox:**
- CPU: 8+ cores
- RAM: 16 GB+ (for running multiple concurrent VMs)
- Disk: 50 GB+ SSD (for fast VM I/O)
- OS: Ubuntu 22.04 LTS, Fedora 38+, or Debian 12+

### Dependencies

#### Build Dependencies (Source Installation)

If building from source, install the following dependencies:

**Ubuntu/Debian:**

```bash
sudo apt-get update
sudo apt-get install -y \
    build-essential \
    git \
    curl \
    pkg-config \
    libssl-dev \
    libcap-ng-dev \
    libseccomp-dev
```

**Fedora/RHEL/CentOS:**

```bash
sudo dnf install -y \
    gcc \
    git \
    curl \
    pkg-config \
    openssl-devel \
    libcap-ng-devel \
    libseccomp-devel
```

#### Rust Toolchain

Cloud Hypervisor is written in Rust. Install the latest stable Rust toolchain:

```bash
# Install rustup (Rust installer)
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh

# Configure current shell
source $HOME/.cargo/env

# Verify installation
rustc --version
cargo --version
```

**Required Rust version:** 1.70.0 or later

#### Runtime Dependencies

Cloud Hypervisor requires the following runtime components:

**1. QEMU utilities (for disk image management):**

```bash
# Ubuntu/Debian
sudo apt-get install -y qemu-utils

# Fedora/RHEL
sudo dnf install -y qemu-img
```

**2. TAP networking tools (for VM network connectivity):**

```bash
# Ubuntu/Debian
sudo apt-get install -y bridge-utils iproute2

# Fedora/RHEL
sudo dnf install -y bridge-utils iproute
```

**3. Optional: virtiofsd (for shared filesystem access):**

```bash
# Ubuntu/Debian
sudo apt-get install -y virtiofsd

# Fedora/RHEL
sudo dnf install -y virtiofsd
```

### Installation Methods

#### Method 1: Pre-built Releases (Recommended)

Using pre-built binaries is the fastest and most reliable installation method.

**1. Download the latest release:**

```bash
# Set desired version (check https://github.com/cloud-hypervisor/cloud-hypervisor/releases)
CH_VERSION="v38.0"

# Download for x86_64
curl -LO https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/${CH_VERSION}/cloud-hypervisor-static

# Download for aarch64
# curl -LO https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/${CH_VERSION}/cloud-hypervisor-static-aarch64
```

**2. Verify download integrity (optional but recommended):**

```bash
# Download checksum file
curl -LO https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/${CH_VERSION}/cloud-hypervisor-static.sha256

# Verify checksum
sha256sum -c cloud-hypervisor-static.sha256
```

**3. Install the binary:**

```bash
# Make executable
chmod +x cloud-hypervisor-static

# Move to system PATH
sudo mv cloud-hypervisor-static /usr/local/bin/cloud-hypervisor

# Verify installation
cloud-hypervisor --version
```

#### Method 2: Source Compilation

Building from source provides the latest features and allows customization.

**1. Clone the repository:**

```bash
git clone https://github.com/cloud-hypervisor/cloud-hypervisor.git
cd cloud-hypervisor
```

**2. Checkout stable version (recommended):**

```bash
# List available tags
git tag -l

# Checkout specific version
git checkout v38.0
```

**3. Build the project:**

```bash
# Build release version with optimizations
cargo build --release

# Binary will be located at: target/release/cloud-hypervisor
```

**4. Optional: Build with additional features:**

```bash
# Build with TDX (Intel Trust Domain Extensions) support
cargo build --release --features tdx

# Build with SEV-SNP (AMD Secure Encrypted Virtualization) support
cargo build --release --features sev_snp

# Build with all features
cargo build --release --all-features
```

**5. Install the binary:**

```bash
sudo cp target/release/cloud-hypervisor /usr/local/bin/
sudo chmod +x /usr/local/bin/cloud-hypervisor
```

**6. Verify installation:**

```bash
cloud-hypervisor --version
```

### Runtime Dependencies

#### Firmware Requirements

Cloud Hypervisor requires firmware to boot virtual machines. Two firmware types are supported:

##### 1. UEFI Firmware (Recommended)

**What is UEFI firmware?**
- UEFI (Unified Extensible Firmware Interface) provides a modern boot environment
- Supports secure boot, GPT partitions, and larger disk sizes
- Required for booting standard Linux distributions (Ubuntu, Fedora, etc.)

**Download OVMF (Open Virtual Machine Firmware):**

```bash
# Ubuntu/Debian (install via package manager)
sudo apt-get install -y ovmf

# Firmware files will be located at:
# - /usr/share/OVMF/OVMF_CODE.fd
# - /usr/share/OVMF/OVMF_VARS.fd
```

**Manual download (for other distributions):**

```bash
# Create firmware directory
sudo mkdir -p /opt/cloud-hypervisor/firmware

# Download OVMF firmware from upstream
curl -L https://github.com/cloud-hypervisor/rust-hypervisor-firmware/releases/download/0.4.2/hypervisor-fw -o /tmp/hypervisor-fw
sudo mv /tmp/hypervisor-fw /opt/cloud-hypervisor/firmware/

# Make executable
sudo chmod +x /opt/cloud-hypervisor/firmware/hypervisor-fw
```

##### 2. PVH Firmware (Alternative)

**What is PVH?**
- PVH (Paravirtualized Hardware) is a lighter-weight boot protocol
- Faster boot times compared to UEFI
- Limited OS support (requires kernel built with PVH support)

**Download rust-hypervisor-firmware:**

```bash
# Download latest PVH firmware
curl -L https://github.com/cloud-hypervisor/rust-hypervisor-firmware/releases/download/0.4.2/hypervisor-fw -o /tmp/hypervisor-fw

# Install to firmware directory
sudo mkdir -p /opt/cloud-hypervisor/firmware
sudo mv /tmp/hypervisor-fw /opt/cloud-hypervisor/firmware/
sudo chmod +x /opt/cloud-hypervisor/firmware/hypervisor-fw
```

#### Firmware Selection Guide

| Boot Method | Firmware Type | Use Case | Boot Time | OS Support |
|-------------|--------------|----------|-----------|------------|
| UEFI | OVMF | Standard Linux distributions | ~500ms | Excellent |
| PVH | rust-hypervisor-firmware | Custom minimal kernels | <100ms | Limited |

**Recommendation:** Use UEFI firmware (OVMF) unless you have specific requirements for ultra-fast boot times and are using a PVH-capable kernel.

### Firmware Handling

#### Firmware Storage Structure

Recommended directory structure for Cloud Hypervisor firmware and resources:

```
/opt/cloud-hypervisor/
├── firmware/
│   ├── hypervisor-fw              # PVH firmware
│   ├── OVMF_CODE.fd              # UEFI firmware (code)
│   └── OVMF_VARS.fd              # UEFI firmware (variables)
├── images/
│   ├── disk1.raw                  # VM disk images
│   └── rootfs.ext4
└── kernels/
    └── vmlinux-5.15              # Custom VM kernels
```

**Create the directory structure:**

```bash
sudo mkdir -p /opt/cloud-hypervisor/{firmware,images,kernels}
sudo chown -R $USER:$USER /opt/cloud-hypervisor
```

#### Firmware Configuration

**For UEFI boot, specify firmware in VM launch:**

```bash
cloud-hypervisor \
    --kernel /opt/cloud-hypervisor/firmware/OVMF_CODE.fd \
    --disk path=/opt/cloud-hypervisor/images/ubuntu.qcow2
```

**For PVH boot with custom kernel:**

```bash
cloud-hypervisor \
    --kernel /opt/cloud-hypervisor/kernels/vmlinux \
    --disk path=/opt/cloud-hypervisor/images/rootfs.ext4
```

#### Firmware Updates

Firmware should be updated periodically for security patches and bug fixes:

```bash
# Update OVMF via package manager (Ubuntu/Debian)
sudo apt-get update && sudo apt-get upgrade ovmf

# Manual update of rust-hypervisor-firmware
curl -L https://github.com/cloud-hypervisor/rust-hypervisor-firmware/releases/latest/download/hypervisor-fw \
    -o /opt/cloud-hypervisor/firmware/hypervisor-fw.new

# Backup old firmware
cp /opt/cloud-hypervisor/firmware/hypervisor-fw /opt/cloud-hypervisor/firmware/hypervisor-fw.backup

# Replace with new firmware
mv /opt/cloud-hypervisor/firmware/hypervisor-fw.new /opt/cloud-hypervisor/firmware/hypervisor-fw
chmod +x /opt/cloud-hypervisor/firmware/hypervisor-fw
```

### Verification Steps

#### Basic Functionality Test

**1. Verify Cloud Hypervisor installation:**

```bash
cloud-hypervisor --version
# Expected output: cloud-hypervisor v38.0
```

**2. Check KVM access:**

```bash
ls -l /dev/kvm
# Ensure /dev/kvm is accessible by your user
```

**3. Test API server mode:**

```bash
# Start Cloud Hypervisor in API mode
cloud-hypervisor --api-socket /tmp/ch-api.sock &
CH_PID=$!

# Check if API socket is created
ls -l /tmp/ch-api.sock

# Stop the process
kill $CH_PID
```

### Directory Structure Recommendations

#### AI Agent Sandbox Setup

Recommended structure for AI agent sandbox deployments:

```
/srv/vibe/
├── cloud-hypervisor/
│   ├── firmware/                 # UEFI/PVH firmware
│   ├── base-images/              # Read-only base images (qcow2)
│   │   ├── ubuntu-22.04.qcow2
│   │   └── python-3.11.qcow2
│   └── kernels/
├── cache/
│   ├── manifests/                # OCI image manifests
│   ├── images/                   # Cached qcow2 base images
│   └── buildah/                  # Buildah storage
├── vms/
│   ├── vm-001/
│   │   ├── overlay.qcow2         # VM's COW overlay
│   │   └── config.json           # VM configuration
│   └── vm-002/
│       ├── overlay.qcow2
│       └── config.json
└── logs/
    ├── vm-logs/                  # Individual VM logs
    └── hypervisor-logs/          # Cloud Hypervisor logs
```

**Setup commands:**

```bash
sudo mkdir -p /srv/vibe/cloud-hypervisor/{firmware,base-images,kernels}
sudo mkdir -p /srv/vibe/cache/{manifests,images,buildah}
sudo mkdir -p /srv/vibe/vms
sudo mkdir -p /srv/vibe/logs/{vm-logs,hypervisor-logs}

# Set appropriate permissions
sudo chown -R $USER:$USER /srv/vibe
sudo chmod -R 755 /srv/vibe
```

### Security Considerations

#### File Permissions

Set appropriate permissions for sensitive components:

```bash
# Firmware (read-only for users)
sudo chmod 644 /opt/cloud-hypervisor/firmware/*

# VM images (read-write for owner only)
chmod 600 /srv/vibe/cache/images/*.qcow2

# Logs (read-only for owner)
chmod 644 /srv/vibe/logs/*.log
```

#### KVM Device Access

Restrict KVM device access to authorized users:

```bash
# Create kvm group if not exists
sudo groupadd -f kvm

# Set /dev/kvm ownership
sudo chown root:kvm /dev/kvm
sudo chmod 660 /dev/kvm

# Add users to kvm group
sudo usermod -aG kvm <username>
```

#### Seccomp and Landlock

Cloud Hypervisor includes built-in security features:

- **Seccomp**: Restricts system calls available to the VM process
- **Landlock**: Linux Security Module for fine-grained access control

These are enabled by default in release builds.

### Troubleshooting

#### Common Issues

**1. KVM not accessible:**

```
Error: Failed to open /dev/kvm: Permission denied
```

**Solution:**
```bash
sudo usermod -aG kvm $USER
# Log out and back in
```

**2. Firmware not found:**

```
Error: Failed to load firmware
```

**Solution:**
```bash
# Ensure firmware is installed
ls -l /opt/cloud-hypervisor/firmware/

# Install OVMF if missing
sudo apt-get install -y ovmf
```

---

## OCI Image Handling and qcow2 Management Strategy

### Overview

This section describes the strategy for handling OCI container images and managing qcow2 disk images for Vibe. The design focuses on efficiency, reusability, and copy-on-write (COW) optimization to support high-concurrency VM operations.

### Architecture

#### Component Stack

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

### OCI Image Workflow

#### 1. Image Pull

Use Buildah to pull OCI images from registries. Buildah is chosen for its:
- Daemonless operation (no Docker daemon required)
- Root-less support
- OCI standard compliance
- Mature Go API

**Key Operations**:
- Pull from any OCI-compatible registry (Docker Hub, GitHub Container Registry, private registries)
- Verify image signatures and checksums
- Store in local storage

#### 2. Image Extract

Extract OCI image layers into a rootfs using Buildah's mount capabilities.

#### 3. Convert to qcow2

Convert the extracted rootfs into a qcow2 base image using `qemu-img` and `libguestfs` tools:

**Conversion Pipeline**:
1. Create empty qcow2 image with specified size
2. Format with ext4 filesystem
3. Copy rootfs contents into image
4. Optimize (compress, remove unused space)

**Tools Used**:
- `qemu-img create` - Create qcow2 images
- `virt-format` - Format filesystem inside image
- `virt-copy-in` - Copy files into image
- `qemu-img convert -c` - Compress and optimize

### Image Caching Strategy

#### Checksum Calculation

Generate a stable checksum from the OCI image manifest to identify unique images:

**Checksum Sources**:
- Image config digest
- All layer digests
- Platform information

**Benefits**:
- Content-addressable storage
- Automatic deduplication
- Version tracking

**Implementation**:
1. Fetch manifest using `skopeo inspect --raw`
2. Extract config digest and layer digests
3. Create stable JSON representation
4. Calculate SHA256 hash

#### Cache Directory Structure

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

#### Deduplication Logic

Images with the same content are automatically deduplicated through checksum-based storage:

**Workflow**:
1. User requests image (e.g., `ubuntu:22.04`)
2. Calculate manifest checksum
3. Check if qcow2 with this checksum exists in cache
4. If exists: Reuse cached image
5. If not: Pull, convert, store with checksum as filename

**Result**: Multiple VMs requesting `ubuntu:22.04` share the same base qcow2 image.

### Copy-on-Write (COW) Implementation

#### qcow2 Backing Files

Use qcow2 backing files to create lightweight per-VM overlay images:

**Concept**:
- **Base image**: Read-only qcow2 file (shared by multiple VMs)
- **Overlay image**: Writable qcow2 file storing only differences
- **Backing chain**: Overlay → Base

**Creation**:
```bash
qemu-img create \
    -f qcow2 \
    -F qcow2 \
    -b /path/to/base.qcow2 \
    /path/to/overlay.qcow2
```

**Benefits**:
- **Disk space**: Multiple VMs share one base image
- **Creation speed**: Instant overlay creation (~200KB initial size)
- **Isolation**: Each VM has its own overlay

#### Example Storage Efficiency

**Scenario**: Create 100 VMs from `ubuntu:22.04` (5GB image)

**Without COW**:
- Disk usage: 100 VMs × 5 GB = 500 GB

**With COW**:
- Disk usage: 1 base (5 GB) + 100 overlays (200 KB each) ≈ 5.02 GB
- **Savings**: 99% disk space reduction

### Storage Layout and Organization

#### Directory Structure

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

### Cleanup Strategy

#### Reference Counting

Track which VMs reference which base images to prevent premature deletion:

**Reference Database**:
- Store in `$CACHE_DIR/references.json`
- Map: `base_image_path → set of VM IDs`

**Operations**:
- `add_reference(base, vm_id)`: Called when VM created
- `remove_reference(base, vm_id)`: Called when VM deleted
- `is_referenced(base)`: Check if any VM uses this base
- `get_unreferenced_images()`: Find bases with zero references

#### Garbage Collection

Implement automatic cleanup of unused resources:

**Collection Targets**:

1. **Unreferenced base images**: Images with no VM references
   - Grace period: 24 hours (prevent deleting recently created images)
   - Action: Move to trash (soft delete)

2. **Orphaned overlays**: Overlay images without corresponding VM config
   - Detection: `overlay.qcow2` exists but `config.json` missing
   - Action: Move to trash

3. **Temporary files**: Files in temp directory
   - Max age: 1 hour
   - Action: Move to trash

4. **Trash cleanup**: Permanently delete old trash
   - Max age: 7 days
   - Action: Permanent deletion

**GC Schedule**:
- Run every hour (background process)
- Or triggered manually via CLI command

### Example Workflows

#### Workflow 1: Create New VM

```go
async def create_vm(image: str, vm_id: str) -> Path:
    # 1. Prepare base image (pulls if not cached)
    base_image = await image_mgr.prepare_base_image(image)

    # 2. Create VM directory
    vm_dir = storage.vm_dir / vm_id
    vm_dir.mkdir(exist_ok=True)

    # 3. Create COW overlay
    overlay = cow_mgr.create_overlay(base_image, vm_id)

    # 4. Register reference
    ref_counter.add_reference(base_image, vm_id)

    # 5. Save VM config
    config = {
        "vm_id": vm_id,
        "image": image,
        "base_image": str(base_image),
        "overlay": str(overlay)
    }
    (vm_dir / "config.json").write_text(json.dumps(config))

    return overlay
```

#### Workflow 2: Delete VM

```go
def delete_vm(vm_id: str):
    # 1. Load config
    config = load_vm_config(vm_id)

    # 2. Remove reference
    ref_counter.remove_reference(config.base_image, vm_id)

    # 3. Move overlay to trash (soft delete)
    move_to_trash(config.overlay)

    # 4. Remove VM directory
    remove_vm_dir(vm_id)

    # 5. Trigger GC if base is unreferenced
    if not ref_counter.is_referenced(config.base_image):
        schedule_gc()
```

#### Workflow 3: High-Concurrency VM Creation

```go
async def create_vm_pool(image: str, count: int) -> list[Path]:
    # 1. Prepare base image once (shared by all VMs)
    base_image = await image_mgr.prepare_base_image(image)

    # 2. Create overlays concurrently
    overlays = await asyncio.gather(*[
        create_one_vm(base_image, f"vm-{i:03d}")
        for i in range(count)
    ])

    # Result: 1 base (~5GB) + count overlays (~200KB each)
    # Total: ~5GB instead of count * 5GB

    return overlays
```

### Performance Optimization

#### Parallel Image Processing

Pull and convert multiple images concurrently to reduce total time.

#### Copy-on-Write Filesystem Support

Use filesystem-level COW when available:
- **btrfs/xfs reflinks**: Instant copies with `cp --reflink=auto`
- **zstd compression**: Reduce base image size

### Summary

The image handling strategy provides:

1. **Efficiency**: Checksum-based caching eliminates duplicate downloads
2. **Reusability**: COW overlays allow many VMs to share base images
3. **Safety**: Reference counting prevents premature deletion
4. **Automation**: Garbage collection cleans up unused resources
5. **Scalability**: Supports high-concurrency VM creation
6. **Flexibility**: Handles image updates gracefully

---

## CLI Architecture and Command Structure

### Overview

The vibe CLI follows the proven architectural patterns from the core project, implementing a clean, interface-driven design using Go 1.25+ with flat package organization. The architecture emphasizes modularity, testability, and maintainability through the "全部包接口化" (all packages as interfaces) principle.

### Project Structure

```
vibe/
├── main.go                    # CLI entry point using urfave/cli/v2
├── go.mod                     # Go 1.25+ module definition
├── config/
│   ├── config.go             # Configuration types and loading
│   └── defaults.go           # Default configuration values
├── vm/
│   ├── vm.go                 # VM lifecycle management interface
│   ├── create.go             # VM creation logic
│   ├── lifecycle.go          # Start/stop/delete operations
│   └── list.go               # List and inspect operations
├── image/
│   ├── image.go              # ImageManager interface
│   ├── buildah.go            # Buildah implementation
│   └── convert.go            # OCI to qcow2 conversion
├── storage/
│   ├── storage.go            # StorageManager interface
│   ├── qcow2.go              # qcow2 operations
│   └── layout.go             # Storage layout management
├── hypervisor/
│   ├── hypervisor.go         # Hypervisor interface
│   ├── cloudhypervisor.go   # Cloud Hypervisor implementation
│   └── factory/
│       └── factory.go        # Factory for hypervisor selection
├── types/
│   ├── vm.go                 # VM types and specifications
│   ├── image.go              # Image types
│   ├── config.go             # Configuration types
│   └── errors.go             # Error definitions
├── client/
│   ├── client.go             # Cloud Hypervisor REST API client
│   └── types.go              # API request/response types
├── utils/
│   ├── fs.go                 # Filesystem utilities
│   └── validation.go         # Input validation
└── version/
    └── version.go            # Version information
```

### Core Interfaces

#### Hypervisor Interface

Following the core project's engine pattern:

```go
package hypervisor

type API interface {
    // Info returns hypervisor information
    Info(ctx context.Context) (*types.HypervisorInfo, error)

    // Ping checks hypervisor connectivity
    Ping(ctx context.Context) error

    // VM lifecycle operations
    VMCreate(ctx context.Context, opts *types.VMCreateOptions) (*types.VMInfo, error)
    VMStart(ctx context.Context, id string) error
    VMStop(ctx context.Context, id string, gracefulTimeout time.Duration) error
    VMDelete(ctx context.Context, id string, force bool) error

    // VM information
    VMInspect(ctx context.Context, id string) (*types.VMInfo, error)
    VMList(ctx context.Context) ([]*types.VMInfo, error)

    // VM resource management
    VMResize(ctx context.Context, id string, cpus int, memory int64) error

    // Console access
    VMAttach(ctx context.Context, id string) (io.ReadWriteCloser, error)
}
```

#### ImageManager Interface

```go
package image

type Manager interface {
    // Pull downloads an OCI image from registry
    Pull(ctx context.Context, ref string) error

    // List returns available OCI images
    List(ctx context.Context, filter string) ([]*types.ImageInfo, error)

    // Inspect returns detailed image information
    Inspect(ctx context.Context, ref string) (*types.ImageInfo, error)

    // Remove deletes an OCI image
    Remove(ctx context.Context, ref string, force bool) error

    // ConvertToQcow2 converts OCI image to qcow2 format
    ConvertToQcow2(ctx context.Context, ref string, output string) (*types.Qcow2Info, error)
}
```

#### StorageManager Interface

```go
package storage

type Manager interface {
    // CreateVolume creates a new qcow2 volume
    CreateVolume(ctx context.Context, opts *types.VolumeCreateOptions) (*types.VolumeInfo, error)

    // DeleteVolume removes a qcow2 volume
    DeleteVolume(ctx context.Context, path string) error

    // ListVolumes returns all volumes for a VM
    ListVolumes(ctx context.Context, vmID string) ([]*types.VolumeInfo, error)

    // CloneVolume creates a copy-on-write clone
    CloneVolume(ctx context.Context, source, dest string) error
}
```

### Factory Pattern

Following core's factory pattern for engine selection:

```go
package factory

type factory func(ctx context.Context, config *config.Config, endpoint string) (hypervisor.API, error)

var hypervisors = map[string]factory{
    "cloud-hypervisor": cloudhypervisor.New,
    // Future: "firecracker": firecracker.New,
    // Future: "qemu": qemu.New,
}

func NewHypervisor(ctx context.Context, cfg *config.Config, hypervisorType string) (hypervisor.API, error) {
    fn, ok := hypervisors[hypervisorType]
    if !ok {
        return nil, fmt.Errorf("unsupported hypervisor type: %s", hypervisorType)
    }
    return fn(ctx, cfg, cfg.Hypervisor.Endpoint)
}
```

### CLI Commands

Using `urfave/cli/v2` (same as core project):

#### Main Application Structure

```go
package main

func main() {
    app := cli.NewApp()
    app.Name = version.NAME
    app.Usage = "Lightweight VM management with OCI images"
    app.Version = version.VERSION
    app.Flags = []cli.Flag{
        &cli.StringFlag{
            Name:        "config",
            Value:       "/etc/vibe/config.yaml",
            Usage:       "config file path",
            Destination: &configPath,
            EnvVars:     []string{"VIBE_CONFIG_PATH"},
        },
    }

    app.Commands = []*cli.Command{
        CreateCommand(),
        StartCommand(),
        StopCommand(),
        DeleteCommand(),
        ListCommand(),
        InspectCommand(),
        ImageCommand(),
    }

    app.Run(os.Args)
}
```

#### Command Definitions

**1. Create Command**

```bash
vibe create --name myvm --image ubuntu:22.04 --cpus 2 --memory 1G
```

Creates a new VM from an OCI image with specified resources.

**2. Start/Stop Commands**

```bash
vibe start myvm
vibe stop myvm --timeout 30s
vibe stop myvm --force
```

Manage VM lifecycle.

**3. List/Inspect Commands**

```bash
vibe list --all
vibe inspect myvm --format json
```

Query VM information.

**4. Delete Command**

```bash
vibe delete myvm
vibe delete myvm --force --volumes
```

Delete VM and cleanup associated resources (overlay, references).

**5. Image Commands**

```bash
vibe image pull ubuntu:22.04
vibe image list
vibe image inspect ubuntu:22.04
vibe image remove ubuntu:22.04
```

Manage OCI images.

### Configuration Structure

Following core's YAML-based configuration pattern:

```yaml
# /etc/vibe/config.yaml
storage:
  root: /var/lib/vibe
  images_dir: /var/lib/vibe/images
  volumes_dir: /var/lib/vibe/volumes
  default_volume_size: 20G

hypervisor:
  type: cloud-hypervisor
  endpoint: http://localhost:8080
  binary_path: /usr/local/bin/cloud-hypervisor
  socket_path: /run/vibe/ch.sock
  default_cpus: 2
  default_memory: 1G

image:
  cache_dir: /var/lib/vibe/cache
  buildah_root: /var/lib/vibe/buildah
  registries:
    docker.io:
      username: ""
      password: ""

global_timeout: 300s
connection_timeout: 10s

log:
  level: info
  use_json: false
  file: /var/log/vibe.log
```

### Cloud Hypervisor REST API Integration

#### Client Implementation

```go
package client

type CloudHypervisorClient struct {
    baseURL    string
    httpClient *http.Client
}

func (c *CloudHypervisorClient) CreateVM(ctx context.Context, config *types.VMConfig) error {
    // PUT /api/v1/vm.create
    // Request body: VM configuration JSON
}

func (c *CloudHypervisorClient) BootVM(ctx context.Context) error {
    // PUT /api/v1/vm.boot
}

func (c *CloudHypervisorClient) ShutdownVM(ctx context.Context) error {
    // PUT /api/v1/vm.shutdown
}

func (c *CloudHypervisorClient) DeleteVM(ctx context.Context) error {
    // PUT /api/v1/vm.delete
}

func (c *CloudHypervisorClient) GetVMInfo(ctx context.Context) (*types.VMInfo, error) {
    // GET /api/v1/vm.info
}
```

### Implementation Flow

#### VM Creation Flow

1. **CLI receives create command** → Parse and validate flags
2. **Load configuration** → Read YAML config
3. **Initialize managers** → Create hypervisor, image, storage managers via factories
4. **Check image availability** → Pull if not in Buildah storage
5. **Convert image** → OCI to qcow2 (or use cached)
6. **Create storage** → Create COW overlay from base image
7. **Configure VM** → Build Cloud Hypervisor VM configuration
8. **Create VM** → Call Cloud Hypervisor REST API
9. **Persist metadata** → Save VM state to disk
10. **Return VM ID** → Display success

#### VM Deletion Flow

1. **Parse VM ID** → Validate input
2. **Load VM metadata** → Read config from disk
3. **Stop VM** → If running, stop gracefully
4. **Delete VM** → Call Cloud Hypervisor API
5. **Cleanup storage** → Remove overlay, update references
6. **Trigger GC** → If base image unreferenced
7. **Remove metadata** → Delete VM directory

### Testing Strategy

Following core's testing patterns:

1. **Interface mocks** → Generate mocks using mockery
2. **Unit tests** → Test packages with mocked dependencies
3. **Integration tests** → Test with real Cloud Hypervisor
4. **CLI tests** → Test command parsing and execution

### Key Design Principles

1. **Interface-driven** → All major components as interfaces
2. **Factory pattern** → Dynamic implementation selection
3. **Flat packages** → Avoid deep nesting
4. **Configuration-first** → YAML with sensible defaults
5. **Error handling** → Consistent error types
6. **Context propagation** → Pass context through all operations
7. **Graceful degradation** → Handle failures with proper cleanup

---

## Implementation Timeline

### Phase 1: Foundation (Weeks 1-2)

**Week 1: Project Setup**
- Initialize Go 1.25 module
- Set up project structure (flat package organization)
- Implement configuration loading (YAML + defaults)
- Create core interfaces (Hypervisor, ImageManager, StorageManager)
- Set up logging infrastructure
- Write initial unit tests

**Week 2: Cloud Hypervisor Integration**
- Implement Cloud Hypervisor REST API client
- Implement hypervisor factory pattern
- Create Cloud Hypervisor implementation of Hypervisor interface
- Test basic VM operations (create, start, stop, delete)
- Document API integration

**Deliverables**:
- Working Go project structure
- Cloud Hypervisor client library
- Basic VM lifecycle operations

### Phase 2: Image Handling (Weeks 3-4)

**Week 3: Buildah Integration**
- Implement Buildah integration for OCI image operations
- Create ImageManager interface implementation
- Implement image pull/list/inspect operations
- Set up Buildah storage configuration
- Test with various OCI registries

**Week 4: qcow2 Conversion**
- Implement OCI to qcow2 conversion pipeline
- Integrate qemu-img and libguestfs tools
- Implement image checksum calculation
- Create image cache with deduplication
- Test conversion with different image sizes

**Deliverables**:
- OCI image pull and management
- Automated qcow2 conversion
- Image caching system

### Phase 3: Storage Management (Weeks 5-6)

**Week 5: COW Implementation**
- Implement StorageManager interface
- Create qcow2 backing file functionality
- Implement overlay creation and management
- Set up storage directory structure
- Test COW isolation and performance

**Week 6: Reference Counting & GC**
- Implement reference counting system
- Create garbage collector for unused images
- Implement soft delete (trash) mechanism
- Create cleanup scheduler
- Test with high-concurrency scenarios

**Deliverables**:
- Copy-on-write storage system
- Automatic garbage collection
- Resource cleanup mechanisms

### Phase 4: CLI Development (Weeks 7-8)

**Week 7: Core Commands**
- Implement CLI using urfave/cli/v2
- Create `create`, `start`, `stop`, `delete` commands
- Implement `list` and `inspect` commands
- Add command validation and error handling
- Create progress indicators for long operations

**Week 8: Image Commands & Polish**
- Implement `image` subcommands (pull, list, inspect, remove)
- Add output formatting (table, JSON, YAML)
- Implement configuration file handling
- Create comprehensive help text
- Add command completion support

**Deliverables**:
- Complete CLI tool
- User documentation
- Command examples

### Phase 5: Testing & Documentation (Weeks 9-10)

**Week 9: Integration Testing**
- Write integration tests with real Cloud Hypervisor
- Test high-concurrency scenarios (100+ VMs)
- Performance benchmarking
- Load testing
- Bug fixes

**Week 10: Documentation & Release**
- Write user guide
- Create API documentation
- Write deployment guide
- Create example configurations
- Prepare v1.0.0 release

**Deliverables**:
- Comprehensive test suite
- Complete documentation
- v1.0.0 release

### Phase 6: Future Enhancements (Post-v1.0)

**Planned Features**:
- Network configuration support (TAP devices, bridges)
- Snapshots and restore
- Live migration support
- Multiple hypervisor backends (Firecracker, QEMU)
- gRPC API for remote management
- Kubernetes integration
- Monitoring and metrics

---

## Security Considerations

### VM Isolation

1. **Hardware-level isolation**: KVM provides strong isolation boundaries
2. **Seccomp filters**: Cloud Hypervisor restricts system calls
3. **Landlock LSM**: Fine-grained filesystem access control
4. **Resource limits**: CPU and memory constraints enforced by hypervisor

### Image Security

1. **Image verification**: Verify checksums and signatures during pull
2. **Registry authentication**: Support for private registry credentials
3. **Read-only base images**: Base images are immutable, only overlays are writable
4. **Image scanning**: Integration point for vulnerability scanning tools

### Filesystem Security

1. **Permission restrictions**: Proper file permissions on VM images and configs
2. **User isolation**: Support for running as non-root user
3. **Storage quotas**: Prevent disk exhaustion attacks
4. **Secure deletion**: Overwrite sensitive data before deletion

### Network Security

1. **Isolated networks**: Each VM on isolated virtual network (Phase 2)
2. **Firewall rules**: iptables integration for traffic filtering (Phase 2)
3. **Network policies**: Restrict VM-to-VM communication (Phase 2)

### Secrets Management

1. **No secrets in configs**: Use environment variables or external secret stores
2. **Registry credentials**: Encrypted storage for registry passwords
3. **API authentication**: Token-based authentication for REST API (future)

---

## Future Work

### Short-term (3-6 months)

1. **Network Support**: Implement TAP devices, bridges, and network policies
2. **Monitoring**: Prometheus metrics for VM and system health
3. **Snapshots**: VM state snapshots for backup and restore
4. **Web UI**: Browser-based management interface

### Medium-term (6-12 months)

1. **Multi-host**: Distributed VM management across multiple hosts
2. **Firecracker backend**: Alternative hypervisor for AWS environments
3. **GPU passthrough**: NVIDIA GPU support for AI workloads
4. **Storage plugins**: Support for Ceph, GlusterFS, NFS backends

### Long-term (12+ months)

1. **Kubernetes integration**: CRI plugin for running VMs as pods
2. **Live migration**: Move running VMs between hosts
3. **Confidential computing**: AMD SEV, Intel TDX support
4. **Multi-architecture**: ARM64, RISC-V support

---

## References

### Cloud Hypervisor
- GitHub: https://github.com/cloud-hypervisor/cloud-hypervisor
- Documentation: https://www.cloudhypervisor.org/
- REST API Reference: https://www.cloudhypervisor.org/api/

### OCI & Container Tools
- Buildah: https://buildah.io/
- OCI Image Spec: https://github.com/opencontainers/image-spec
- Skopeo: https://github.com/containers/skopeo

### qcow2 & Virtualization
- QEMU Documentation: https://www.qemu.org/docs/master/
- qcow2 Specification: https://gitlab.com/qemu-project/qemu/-/blob/master/docs/interop/qcow2.txt
- libguestfs: https://libguestfs.org/

### Related Projects
- BoxLite: https://github.com/boxlite-ai/boxlite
- Firecracker: https://github.com/firecracker-microvm/firecracker
- Kata Containers: https://katacontainers.io/

### Go Libraries
- urfave/cli: https://github.com/urfave/cli
- containers/image: https://github.com/containers/image
- containers/buildah: https://github.com/containers/buildah

---

## Appendix: Requirements Verification

This RFC addresses all requirements specified in the initial request:

1. ✅ **Cloud Hypervisor Installation**: Comprehensive installation guide with prerequisites, dependencies, and verification steps
2. ✅ **CLI with OCI Support**: Complete CLI design using Go 1.25 with Buildah integration for OCI images
3. ✅ **Image Reuse (qcow2 + COW)**: Checksum-based caching and qcow2 backing files for efficient disk management
4. ✅ **Network Explicitly Deferred**: Documented as Phase 2 future work
5. ✅ **Cleanup on Delete**: Reference counting, garbage collection, and automatic resource cleanup
6. ✅ **Firmware Handling**: Automated firmware download, storage recommendations, and configuration guidance
7. ✅ **Core Project Patterns**: Flat package organization, interface-driven design, factory pattern, YAML configuration

---

**End of RFC**

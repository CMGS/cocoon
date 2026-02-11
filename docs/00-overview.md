# Cocoon Overview

## Project Motivation

Modern AI Agent sandboxes and code execution platforms face a challenging trade-off between isolation, performance, and usability:

- **Docker/Podman**: Fast startup and familiar tooling, but only container-level isolation with shared kernel
- **Traditional VMs (QEMU/KVM)**: Strong VM-level security boundaries but slow startup and heavyweight operation
- **BoxLite**: Excellent for AI sandboxes with good balance, but young project (v0.5.x) still maturing
- **Cloud Hypervisor**: Production-grade VMM with strong isolation and fast startup, but lacks OCI image support

**Cocoon bridges this gap** by combining Cloud Hypervisor's maturity and performance with the convenience of OCI images, providing:
- **Strong isolation**: VM-level security boundaries via KVM
- **Fast startup**: Sub-second VM creation using microVM technology
- **High concurrency**: Efficient management of hundreds of VMs simultaneously
- **Familiar tooling**: Direct use of existing OCI images from Docker Hub, ghcr.io, and other registries

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
- Trade-off: ~500ms boot time vs <100ms for PVH (acceptable for AI sandbox use case)

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
- Phase 1 delivers complete, production-ready tool for AI sandbox use case
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
- **Live Migration**: Not required for AI sandbox use case
- **GPU Passthrough**: Future consideration
- **Kubernetes Integration**: Future consideration
- **Multi-host Orchestration**: Future consideration

## Use Cases

### Primary: AI Agent Sandbox

**Requirements Met**:
- Strong isolation for untrusted code execution
- Fast VM creation for rapid task execution
- High concurrency for parallel agent operations
- Familiar OCI images for environment consistency

### Secondary: Development Environments

**Enabled Features**:
- Spin up isolated test environments from OCI images
- Efficient disk usage for multiple similar VMs
- Quick cleanup and recreation

### Future: Kubernetes Workloads

**Potential Integration**:
- CRI plugin for VM-based pods
- Strong isolation for multi-tenant clusters
- GPU passthrough for ML workloads

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

# OCI to qcow2 Conversion Pipeline

**Version**: 2.0
**Status**: Implemented
**Phase**: Phase 1
**Last Updated**: 2026-02-20

## Executive Summary

This document specifies the pipeline for converting OCI container images into bootable qcow2 disk images for Cloud Hypervisor VMs. The conversion process must produce images that satisfy the [Boot Contract](01-boot-contract.md) while maintaining efficiency through caching and deduplication.

**Key Requirements**:
1. Pull OCI images using native Go library (`go-containerregistry`), eliminating `buildah`/`skopeo` CLI dependencies.
2. Materialize rootfs layers into a flattened directory, handling whiteouts and permissions.
3. Convert rootfs to qcow2 format with proper partitioning (via `guestfish` + `qemu-img`).
4. Validate GRUB config presence post-conversion.
5. Cache images based on content checksums (atomic rename into cache with `fsync`).
6. Provide robust error handling via ClassifiedError (transient/permanent).

---

## 1. Architecture Overview

### 1.1 Conversion Pipeline

```
+-----------------+
|  OCI Registry   | (Docker Hub, GHCR, etc.)
+--------+--------+
         | remote.Get (manifest only)
         | Recursive Index resolution (multi-arch)
         v
+-----------------+
| Image Identity  | (Checksum, Arch, ManifestDigest)
+--------+--------+
         | Lock(baseKey)
         | remote.Image (pull layers)
         v
+-----------------+
| Materialized FS | (Temp dir 0700)
+--------+--------+
         | guestfish (partition + tar-in)
         v
+-----------------+
|  Base qcow2     | (Bootable disk image, .tmp)
+--------+--------+
         | os.Rename + Parent Dir fsync
         v
+-----------------+
|  Image Cache    | (/var/lib/cocoon/cache/images/)
+-----------------+
```

### 1.2 Component Responsibilities

| Component | Responsibility | Implementation |
|-----------|----------------|------|
| **Image Identifier** | Compute content-addressed identity from OCI manifest | `go-containerregistry` |
| **Image Puller** | Download layers and config | `go-containerregistry` |
| **Rootfs Materializer** | Flatten layers, handle whiteouts, protect paths | `utils/tar.go` (`os.OpenRoot`) |
| **qcow2 Converter** | Create disk image with partitions, copy rootfs | `qemu-img`, `guestfish` |
| **Process Manager** | Manage external tool lifecycles (kill groups) | `utils.CommandContextWithGroup` |
| **Cache Manager** | Deduplicate and cache base images | `image/pipeline/manager.go` |

---

## 2. OCI Integration (Native)

**Decision**: Use `google/go-containerregistry` library instead of shelling out to `buildah` or `skopeo`.

**Rationale**:
- **Daemonless**: Pure Go implementation, no background process.
- **Dependency Reduction**: Removes requirement for users to install `buildah`/`skopeo`.
- **Control**: Fine-grained control over layer extraction, timeout handling, and retry logic.
- **Performance**: In-process execution avoids fork/exec overhead for metadata operations.

### 2.1 Manifest Resolution

The pipeline supports both single Manifests and Manifest Lists (OCI Indexes).
- **Manifest List**: Recursively resolves the manifest matching `linux/<GOARCH>`.
- **Config Digest**: Extracted from the resolved single manifest to form the cache key.

---

## 3. Pull & Materialize

### 3.1 Flow

1. **Identify (Cheap)**: Fetch manifest headers only. Compute `base_key` = `SHA256(config + layers + arch)[:16]`.
2. **Lock**: Acquire exclusive lock on `base_key`.
3. **Check Cache**: If exists, unlock and return.
4. **Pull (Expensive)**: Download all layers using the library.
5. **Materialize**: Extract layers to a temporary directory (`0700` permissions).
   - Uses `utils/tar.go` with `os.OpenRoot` (Go 1.25) to strictly confine extraction to the target directory, preventing path traversal attacks.
   - Applies OCI whiteouts (flattening).

---

## 4. Convert Workflow

### 4.1 Conversion Steps

```
Materialized Rootfs (Temp Dir)
    |
    v
1. Create empty qcow2 image (qemu-img create)
    |
    v
2. Check guestfish availability (Required)
    |
    v
3. Pack rootfs into tar archive (uncompressed)
    |
    v
4. Guestfish script: partition, format, tar-in rootfs
   (GPT table, ESP FAT32, root ext4, tar-in, sync)
    |
    v
5. Validate GRUB config (ensureGRUBConfig)
    |
    v
6. Atomic Publish (Rename + DirSync)
```

### 4.2 Guestfish Dependency

**Hard Requirement**: The OCI conversion path **strictly requires** `libguestfs-tools` (`guestfish`).
- If `guestfish` is missing, conversion fails immediately.
- This differs from the *verification* path for existing images, where `guestfish` is optional.

---

## 5. Security & Durability

### 5.1 Path Safety
- **Extraction**: Uses `os.OpenRoot` to enforce chroot-like containment during layer extraction.
- **Ref Classification**: Local files must be prefixed with `./`, `../`, or `/` to avoid shadowing remote registry references (e.g., `ubuntu:latest`).

### 5.2 Process Management
- External tools (`qemu-img`, `guestfish`) are executed in their own process groups (`Setpgid`).
- On context cancellation (timeout/interrupt), the entire process group is killed to prevent orphaned zombie processes.

### 5.3 Data Durability
- **Atomic Write**: `os.Rename` is followed by `SyncParentDir` (fsync on parent directory) to ensure metadata is flushed to stable storage.
- **Temp Permissions**: All temporary directories are created with `0700` to prevent information leakage to other users on the host.

---

## 6. Caching Strategy

Cache keys are content-addressable based on the **resolved** OCI configuration and layers.
- **Key**: `{checksum16}_{arch}`
- **Refcache**: Maintains a mapping of `IMAGE_REF` -> `base_key` to avoid repeated manifest fetches.

---

## 7. Implementation Status

| Feature | Status | Implementation |
|---------|--------|----------------|
| Native OCI Pull | **Done** | `go-containerregistry` |
| Manifest Lists | **Done** | Recursive resolution |
| Path Safety | **Done** | `os.OpenRoot` |
| Atomic Durability | **Done** | `SyncParentDir` |
| Process Cleanup | **Done** | `CommandContextWithGroup` |
| Sparse File Support | *Deferred* | Planned for Phase 2 |

---

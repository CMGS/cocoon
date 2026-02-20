# OCI-to-qcow2 Conversion (Native Pipeline)

**Version**: 2.0
**Status**: Implemented
**Scope**: `image/pipeline` cloud-image path (OCI/URL/local file -> cached qcow2)
**Last Updated**: 2026-02-20

---

## 1. Overview

Cocoon supports three source types for base image preparation:
- Registry OCI references
- HTTP/HTTPS image URLs
- Local image files

All three converge to a cached immutable qcow2 base image:

```
source ref -> identify -> conversion lock -> materialize/convert -> cache/base.qcow2
```

For OCI refs, Cocoon now uses native registry access (Go library) instead of external OCI CLIs.

---

## 2. Architecture

### 2.1 Main Components

- `image/pipeline/manager.go`
  - orchestrates `Pull`, `Convert`, `Prepare`, cache lookup, lock handling
- `image/pipeline/identify.go`
  - OCI identify and OCI rootfs materialization via go-containerregistry
- `image/pipeline/convert_linux.go`
  - rootfs directory -> qcow2 using `qemu-img` + `guestfish`
- `image/refcache`
  - `IMAGE_REF -> base_key` mapping for fast cache hits

### 2.2 Data Flow

```
Registry/URL/File
   -> ImageIdentity (checksum + arch + metadata)
   -> per-baseKey conversion lock
   -> conversion output
   -> /var/lib/cocoon/cache/images/{checksum16}_{arch}.qcow2
```

---

## 3. Reference Classification

Classification logic (`classifyRef`) in `image/pipeline/manager.go`:

- `http://` / `https://` -> URL source
- absolute/relative existing path -> local file source
- otherwise -> OCI registry reference

This keeps pull behavior deterministic and allows a unified `Prepare()` pipeline.

---

## 4. OCI Identify (No Layer Download)

`identifyOCIRemote(ctx, ref)` performs manifest-level identify:

1. Normalize implicit tag (`:latest`) with `oci.EnsureLatestTag`
2. Resolve tag with `name.NewTag`
3. Pull manifest for `linux/<GOARCH>` platform using `remote.Image`
4. Parse config digest + layer digests
5. Compute deterministic checksum (`computeOCIChecksum`)
6. Return `ImageIdentity` with:
   - `Checksum` (short)
   - `FullDigest` (full computed identity)
   - `ManifestDigest` (remote digest)
   - `Arch`

Goal: cheap identity probe outside conversion lock.

---

## 5. OCI Pull + Rootfs Materialization

Inside conversion lock, `pullAndMaterializeOCI` does:

1. Fetch image for the resolved platform
2. Re-check digest if identify produced `ManifestDigest` (TOCTOU guard)
3. Write temporary OCI layout locally (`writeImageToTempLayout`)
4. Materialize rootfs into temp dir via `oci.MaterializeRootfs`
5. Set `identity.TempPath` to materialized rootfs path

Whiteout semantics and layer application order are handled by `oci.MaterializeRootfs`.

---

## 6. Conversion to qcow2

`Convert()` uses a per-image lock (`cache/locks/{baseKey}.lock`) and double-check cache strategy:

1. Check cache before lock (fast path)
2. Acquire lock
3. Re-check cache after lock
4. Convert source if still missing

### 6.1 URL/Local Source

- detect format with `qemu-img info`
- if source is qcow2: atomic copy into cache
- if source is raw: `qemu-img convert -f raw -O qcow2`

### 6.2 OCI Source

- `convertOCI(identity.TempPath, tmpPath, diskSize)` in `convert_linux.go`
- creates qcow2 and imports materialized rootfs
- ensures boot contract expectations for Linux VM image path

### 6.3 Atomic Placement

For all sources:

- write `*.tmp` under cache dir
- `os.Rename(tmp, final)` for atomic publish
- `chmod 0444` on final base image (immutable shared backing)

---

## 7. Caching and Idempotency

### 7.1 Base Key

Base key format:

```
{checksum16}_{arch}
```

Example:

```
a1b2c3d4e5f6a7b8_amd64
```

### 7.2 Refcache

`image/refcache` tracks aliases (`IMAGE_REF -> base_key`) for pull/prepare shortcuts.

### 7.3 Concurrency Contract

Only one conversion for a specific `base_key` can run at a time. Concurrent callers either:

- hit cache immediately, or
- wait lock and then observe post-lock cache hit

This eliminates duplicate conversion work.

---

## 8. Verification Integration

After pull/prepare (via CLI), bootability verification is controlled by cache + verify state:

- cache miss: verify by default
- cache hit and previously verified: skip verify by default
- `--skip-verify`: force skip

Verification implementation is in `verify_linux.go` (guestfish-based deep checks).

---

## 9. Error Model

Cocoon uses classified errors (`types`):

- transient: retryable network/remote issues
- permanent: deterministic invalid input/config/state

OCI registry errors are normalized via `oci.ClassifyRegistryError`.

---

## 10. Platform Support

- Linux: full conversion pipeline supported
- Darwin: conversion stubs return explicit unsupported errors for Linux-only tooling paths

---

## 11. External Tooling Requirements

For this pipeline:

- `qemu-img` (required)
- `guestfish` (required)

OCI registry probe/pull/inspect in this path is implemented in Go and does not require external OCI CLI tools.

---

## 12. Sequence (Simplified)

```text
Prepare(ref)
  -> tryRefcacheHit
  -> classifyRef
  -> Pull(ref)
      - OCI: identifyOCIRemote
      - URL: download temp file + checksum
      - Local: checksum local file
  -> Convert(identity)
      - lock(baseKey)
      - cache recheck
      - materialize/convert
      - atomic publish
  -> return base image path
```

---

## 13. Known Limitations

- Conversion path remains Linux-tool dependent (`guestfish`, `qemu-img`)
- OCI runtime direct boot and cloud-image conversion are separate runtime paths with different storage contracts
- Very large rootfs conversion can be I/O intensive; SSD strongly recommended

---

## 14. Validation Checklist

- `go test ./image/pipeline ./oci ./image/refcache`
- `cocoon image pull <oci-ref|url|local-file>`
- `cocoon image ls` shows expected cache entries
- repeated pull/prepare for same ref should produce cache hit

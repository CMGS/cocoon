# OCI to qcow2 Conversion Pipeline

**Version**: 1.1
**Status**: Implemented
**Phase**: Phase 1
**Last Updated**: 2026-02-15

## Executive Summary

This document specifies the pipeline for converting OCI container images into bootable qcow2 disk images for Cloud Hypervisor VMs. The conversion process must produce images that satisfy the [Boot Contract](01-boot-contract.md) while maintaining efficiency through caching and deduplication.

**Key Requirements**:
1. Pull OCI images from registries using Buildah (with `--root` flag for custom storage)
2. Extract container rootfs to disk via Buildah mount
3. Convert rootfs to qcow2 format with proper partitioning (guestfish + tar-in)
4. Validate GRUB config presence post-conversion (fail if missing)
5. Cache images based on content checksums (atomic rename into cache)
6. Provide robust error handling via ClassifiedError (transient/permanent)

---

## Code Examples Disclaimer

**IMPORTANT**: Code examples in this document serve different purposes:

### Illustrative Pseudocode (Conceptual)
Examples marked as "**Illustrative**" or "**Conceptual**" show the high-level logic and are NOT production-ready:
- May omit error handling for clarity
- May use simplified command syntax
- Actual tool outputs/exit codes may differ

**Example**:
```go
// Illustrative: Shows concept, not exact implementation
if guestfish.Exists("/boot/vmlinuz") {  // Actual API differs
    return true
}
```

### Implementation-Required Behavior (Normative)
Examples marked as "**MUST**" or "**Implementation-Required**" define mandatory behavior:
- Exact validation rules that implementations must follow
- Required external tool usage patterns
- Critical error conditions

### Responsibility Boundaries

**Cocoon's Role**:
- **Validate**: Check if image has required components (kernel, bootloader) -- post-conversion via `cocoon image verify`
- **Configure**: Modify GRUB config, inject serial console settings (if GRUB config present)
- **Does NOT Install**: Does NOT install missing packages (kernel, GRUB)

**User's Role** (Image Provider):
- **MUST provide**: kernel, bootloader **pre-installed**
- Cocoon only verifies and configures existing components
- Users may optionally install cloud-init in their images for guest initialization

**Rationale**: Installing packages during conversion would require:
- Network access (package repos)
- Package manager knowledge (apt/yum/dnf/apk)
- Dependency resolution
- Significantly slower conversion

Instead, Cocoon **fails fast** with clear error messages when components are missing.

---

## Table of Contents

1. [Architecture Overview](#1-architecture-overview)
2. [Buildah Integration](#2-buildah-integration)
3. [Pull Workflow](#3-pull-workflow)
4. [Extract Workflow](#4-extract-workflow)
5. [Convert Workflow](#5-convert-workflow)
6. [Checksum-Based Caching](#6-checksum-based-caching)
7. [Manifest Refcache](#7-manifest-refcache)
8. [Error Handling](#8-error-handling)
9. [Design Principles](#9-design-principles)
10. [Implementation Checklist](#10-implementation-checklist)
11. [Verified Images (CI Reference)](#11-verified-images-ci-reference)
12. [References](#12-references)

---

## 1. Architecture Overview

### 1.1 Conversion Pipeline

```
+-----------------+
|  OCI Registry   | (Docker Hub, GHCR, etc.)
+--------+--------+
         | skopeo inspect (identify)
         | buildah --root <root> pull (inside lock)
         v
+-----------------+
| OCI Image Store | (/var/lib/cocoon/buildah/)
+--------+--------+
         | buildah --root <root> from + mount
         v
+-----------------+
|  Mounted Rootfs | (buildah mount point)
+--------+--------+
         | tar -cf (pack rootfs)
         | guestfish (partition + tar-in)
         v
+-----------------+
|  Base qcow2     | (Bootable disk image, .tmp)
+--------+--------+
         | os.Rename (atomic)
         v
+-----------------+
|  Image Cache    | (/var/lib/cocoon/cache/images/)
+-----------------+
         | COW Backing File
         v
+-----------------+
| VM Overlay Disk | (Per-VM instance)
+-----------------+
```

### 1.2 Component Responsibilities

| Component | Responsibility | Tool |
|-----------|----------------|------|
| **Image Identifier** | Compute content-addressed identity from OCI manifest | skopeo |
| **Image Puller** | Download OCI images from registries | Buildah (via `--root` flag) |
| **Rootfs Extractor** | Mount and extract container filesystem | Buildah mount |
| **qcow2 Converter** | Create disk image with partitions, copy rootfs via tar-in | qemu-img, guestfish |
| **GRUB Validator** | Validate GRUB config exists, best-effort serial console update | guestfish, virt-customize |
| **Cache Manager** | Deduplicate and cache base images via atomic rename | Custom Go code |
| **Checksum Calculator** | Generate stable image checksums | skopeo, Go crypto |
| **Manifest Refcache** | Map IMAGE_REF to base_key for fast cache lookups | `image/refcache/index.go` |

### 1.3 Design Principles

1. **Idempotent Operations**: Same input produces same output
2. **Checksum-Based Caching**: Never convert the same image twice
3. **Fail-Fast Validation**: Verify boot contract compliance (on-demand via `cocoon image verify`)
4. **Shell-Out Strategy**: Use external tools via `runCmd()` helper (Buildah, skopeo, qemu-img, guestfish)

---

## 2. Buildah Integration

### 2.1 Why Buildah?

**Decision**: Use Buildah for OCI image operations instead of Docker or containerd libraries.

**Rationale**:
- **Daemonless**: No background process required
- **OCI-compliant**: Handles any OCI-compatible registry
- **Simple CLI interface**: Easy to shell out from Go
- **No daemon required**: Lightweight, no background process
- **Battle-tested**: Used in production by Podman, OpenShift

**Alternatives Considered**:

| Alternative | Pros | Cons | Decision |
|-------------|------|------|----------|
| Docker CLI | Familiar, widely available | Requires Docker daemon | Not selected |
| containerd library | Native Go, no shell-out | Complex API, daemon required | Not selected |
| Podman | Same CLI as Docker | Uses Buildah internally anyway | Redundant |
| **Buildah** | **Daemonless, simple, OCI-native** | **None** | **Selected** |

### 2.2 Shell-Out Pattern

**Decision**: Shell out to Buildah CLI via a generic `runCmd()` helper, not a dedicated `BuildahClient` struct.

**Rationale**:
- **Stability**: CLI interface is stable, library internals change frequently
- **Error handling**: Easier to capture stderr and parse error messages
- **Process isolation**: Buildah runs in separate process, clean crash handling
- **Version flexibility**: Works with any Buildah version installed on host
- **Performance**: Minor overhead from process spawning (acceptable for infrequent operations)

**Actual Implementation** (`image/pipeline/oci_linux.go`):

The code uses a package-level `runCmd()` function shared across all external tool invocations (skopeo, buildah, guestfish). There is no `BuildahClient` struct.

```go
// Actual implementation: bare runCmd helper in pipeline package
func runCmd(ctx context.Context, name string, args ...string) ([]byte, error) {
    cmd := exec.CommandContext(ctx, name, args...)
    out, err := cmd.Output()
    if err != nil {
        if exitErr, ok := err.(*exec.ExitError); ok {
            return nil, fmt.Errorf("%s: %s",
                strings.Join(append([]string{name}, args...), " "),
                string(exitErr.Stderr))
        }
        return nil, err
    }
    return out, nil
}
```

### 2.3 Buildah Storage Configuration

Buildah's storage root is configured via the `--root` CLI flag (not an environment variable). The root path comes from `config.CocoonConfig.BuildahRoot`.

**Actual Implementation**:

```go
// Every buildah command uses --root to set the storage location.
// The root path is taken from cfg.BuildahRoot (default: /var/lib/cocoon/buildah).
root := cfg.BuildahRoot
runCmd(ctx, "buildah", "--root", root, "pull", ref)
runCmd(ctx, "buildah", "--root", root, "from", ref)
runCmd(ctx, "buildah", "--root", root, "mount", containerID)
```

**Storage Layout**:

```
/var/lib/cocoon/
+-- buildah/              # Buildah storage root (--root flag)
|   +-- overlay/          # OCI image layers
|   +-- vfs/
|   +-- storage.lock
+-- cache/
|   +-- images/           # Converted qcow2 base images
|   +-- manifests/        # Manifest refcache (index.json)
|   +-- locks/            # Per-image conversion locks
+-- vms/                  # Per-VM overlay disks
```

---

## 3. Pull Workflow

### 3.1 Two-Phase OCI Pull

The OCI pull is split into two phases for efficiency:

**Phase 1: Identify (cheap, outside lock)** -- Uses skopeo to inspect the manifest and compute the content-addressed identity. No image layers are downloaded.

**Phase 2: Pull + Mount (expensive, inside conversion lock)** -- Uses buildah to pull layers, create a working container, and mount the rootfs.

This design allows cache hits to skip the expensive pull entirely.

```go
// Actual implementation: Phase 1 -- identify via skopeo
func identifyOCIPlatform(ctx context.Context, ref string) (*image.ImageIdentity, error) {
    // Skopeo handles image name resolution (docker.io/library/ prefix, etc.)
    rawManifest, err := runCmd(ctx, "skopeo", "inspect", "--raw", "docker://"+ref)
    if err != nil {
        return nil, classifySkopeoError(...)
    }

    // Detect manifest list vs single manifest
    // If manifest list, re-inspect with --override-arch
    // Parse config digest + layer digests
    // Compute checksum identity
    // Return identity WITHOUT TempPath (layers not pulled yet)
    ...
}
```

**Note on image name normalization**: The code does NOT implement a `normalizeImageName()` function. Image references are passed directly to skopeo as `"docker://"+ref`. Skopeo handles registry resolution (e.g., resolving short names like `ubuntu:22.04` to `docker.io/library/ubuntu:22.04`) internally.

### 3.2 Image Verification (skopeo inspect)

Before pulling layers, the manifest is inspected to compute identity:

```go
// Actual: uses skopeo inspect --raw to get manifest without pulling layers
rawManifest, err := runCmd(ctx, "skopeo", "inspect", "--raw", "docker://"+ref)

// Parse manifest to detect if it's a manifest list
var index ociIndex
json.Unmarshal(rawManifest, &index)

isManifestList := strings.Contains(index.MediaType, "image.index") ||
    strings.Contains(index.MediaType, "manifest.list")
```

### 3.3 Authenticated Pulls

**Phase 1 (cloud images / bootable OCI)**: Relies on ambient credentials available to skopeo and buildah (e.g., `~/.docker/config.json`, podman login state, or environment variables). If the image requires authentication and credentials are not found, the pull fails with a permanent `ClassifiedError`.

**OCI VM image operations (build/push)**: Explicit registry login is implemented via `cocoon image login REGISTRY` (see `oci/login.go`). Credentials are stored in `~/.cocoon/config.json` using Docker-compatible auth format. The `cocoon image push` command uses these credentials via the `CocoonKeychain` (falling back to Docker's default keychain).

### 3.4 Pull Progress Tracking

> **Not Yet Implemented** -- Future Work

The current implementation does not track or report OCI pull progress. Buildah pull runs to completion without progress callbacks.

**Note**: For URL-based image downloads (`pullURL`), the code does log progress every 10% when content-length is known.

---

## 4. Extract Workflow

### 4.1 Container Creation and Mount

**Goal**: Mount the OCI image layers as a unified filesystem using buildah.

The actual implementation uses bare `runCmd()` calls with `buildah --root`:

```go
// Actual implementation (image/pipeline/oci_linux.go)
func pullAndMountOCIPlatform(ctx context.Context, cfg *config.CocoonConfig,
    identity *image.ImageIdentity) error {

    ref := identity.SourceRef
    root := cfg.BuildahRoot

    // 1. Pull image with buildah
    runCmd(ctx, "buildah", "--root", root, "pull", ref)

    // 2. Create working container
    containerOut, _ := runCmd(ctx, "buildah", "--root", root, "from", ref)
    containerID := strings.TrimSpace(string(containerOut))

    // 3. Mount container to get rootfs path
    mountOut, _ := runCmd(ctx, "buildah", "--root", root, "mount", containerID)
    mountPath := strings.TrimSpace(string(mountOut))

    // 4. Populate identity with transient paths
    identity.TempPath = mountPath
    identity.ContainerID = containerID
    return nil
}
```

### 4.2 Cleanup

Cleanup uses a dedicated function that unmounts and removes the buildah container:

```go
// Actual implementation (image/pipeline/cleanup_linux.go)
func cleanupBuildahContainer(containerID string, cfg *config.CocoonConfig) {
    if containerID == "" {
        return
    }
    root := cfg.BuildahRoot
    exec.Command("buildah", "--root", root, "umount", containerID).CombinedOutput()
    exec.Command("buildah", "--root", root, "rm", containerID).CombinedOutput()
}
```

Cleanup is called:
- After successful conversion (in `prepareOCI` and `Convert`)
- On pull failure (in `prepareOCI`)
- Both umount and rm failures are logged as warnings but do not fail the operation

### 4.3 Bootability Verification

> **Important timing note**: Bootability verification happens **post-conversion** on the resulting qcow2 image, NOT pre-conversion on the mounted rootfs. The conversion pipeline itself does not verify bootability. Verification is triggered in two ways:
>
> 1. **Automatically at VM creation time**: `cocoon create` and `cocoon run` auto-verify bootability of the base image by default. Use `--skip-verify` to bypass this check.
> 2. **Explicitly via standalone command**: `cocoon image verify` runs full verification on demand (it does not accept `--skip-verify` — it always runs the complete check).

The `VerifyBootability()` method on the `manager` performs a two-tier check:

**Basic verification** (always available):
- File exists and has non-zero size
- `qemu-img check` passes (image integrity)
- `qemu-img info` confirms valid qcow2 format
- Optimistically sets `Bootable=true` if qcow2 is valid

**Deep verification** (Linux only, requires guestfish):
- Kernel: glob-expand `/boot/vmlinuz*`
- Initrd/initramfs: glob-expand `/boot/initr*` and `/boot/initramfs*`
- systemd: readlink `/sbin/init` for "systemd", or check `/lib/systemd/systemd`
- UEFI bootloader: check multiple ESP paths (BOOTX64.EFI, BOOTAA64.EFI, shimx64.efi, grubx64.efi, etc.)

```go
// Actual implementation (image/pipeline/verify_linux.go)
// deepVerifyBoot uses guestfish --ro to inspect the qcow2 image contents.
// If guestfish is not installed, this is non-fatal: a warning is logged.
func deepVerifyBoot(imagePath string, result *image.BootCheckResult) error {
    // Check kernel via: guestfish --ro -a <image> -i glob-expand /boot/vmlinuz*
    // Check initrd via: guestfish --ro -a <image> -i glob-expand /boot/initr*
    // Check systemd via: guestfish --ro -a <image> -i readlink /sbin/init
    // Check bootloader via: guestfish --ro -a <image> -i is-file <path>
    //   (checks BOOTX64.EFI, BOOTAA64.EFI, shimx64.efi, grubx64.efi, etc.)
    ...
}
```

> **Known Limitation (False-Positive Bootability)**: When `libguestfs` (`guestfish`) is not available on the host, deep verification is skipped and the image is optimistically marked as bootable (`Bootable=true`) after basic qcow2 integrity checks pass. This means non-bootable images -- those missing a kernel, initrd, or UEFI bootloader -- may pass verification without warning when guestfish is not installed. The `--skip-verify` flag on `cocoon create` and `cocoon run` makes this trade-off explicit. For production use, ensure `libguestfs-tools` is installed to enable deep verification of boot components. Run `cocoon doctor` to check whether all required dependencies (including guestfish) are available.

### 4.4 Architecture Detection

> **Note**: The code does NOT implement a `DetectArchitecture()` function that inspects kernel binaries or dpkg/rpm metadata. Instead, architecture is always determined from `runtime.GOARCH` via the `goarchToOCI()` helper.

```go
// Actual implementation (image/pipeline/checksum.go)
func goarchToOCI(goarch string) string {
    switch goarch {
    case "amd64":
        return "amd64"
    case "arm64":
        return "arm64"
    default:
        return goarch
    }
}

func defaultArch() string {
    return goarchToOCI(runtime.GOARCH)
}
```

This means the architecture in the base_key always matches the host architecture. Cross-architecture detection from image contents is not implemented.

---

## 5. Convert Workflow

### 5.1 Conversion Steps Overview (Actual)

The actual conversion pipeline in `convertOCI()` (`image/pipeline/convert_linux.go`) has 6 steps:

```
Mounted Rootfs (buildah mount path)
    |
    v
1. Create empty qcow2 image (qemu-img create)
    |
    v
2. Check guestfish availability
    |
    v
3. Pack rootfs into tar archive (tar -C <mountPath> -cf <tarPath> .)
    |
    v
4. Guestfish script: partition, format, tar-in rootfs
   (GPT table, ESP FAT32, root ext4, tar-in, sync)
    |
    v
5. Validate GRUB config (ensureGRUBConfig)
   - Detect grub.cfg path (checks /boot/grub/grub.cfg and /boot/grub2/grub.cfg)
   - Best-effort: inject serial console param via virt-customize (if available)
    |
    v
6. Return (caller does atomic rename into cache)
    |
    v
Bootable qcow2 Image
```

### 5.2 Step 1: Create Empty qcow2

```go
// Actual implementation
createCmd := exec.CommandContext(ctx, "qemu-img", "create", "-f", "qcow2", outputPath, diskSize)
if out, err := createCmd.CombinedOutput(); err != nil {
    return fmt.Errorf("qemu-img create: %s: %w", strings.TrimSpace(string(out)), err)
}
```

Default disk size for OCI conversion is `"10G"`.

### 5.3 Step 2: Check guestfish Availability

```go
if _, err := exec.LookPath("guestfish"); err != nil {
    return fmt.Errorf("guestfish not found in PATH: OCI conversion requires libguestfs: %w", err)
}
```

### 5.4 Step 3: Pack Rootfs into Tar

The rootfs is packed into an uncompressed tar archive to preserve dotfiles and all top-level entries. This avoids shell glob issues with `copy-in` or `virt-copy-in`.

```go
// Actual implementation
rootfsTarPath := filepath.Join(filepath.Dir(outputPath),
    fmt.Sprintf(".rootfs-%d.tar", time.Now().UnixNano()))
tarCmd := exec.CommandContext(ctx, "tar", "-C", mountPath, "-cf", rootfsTarPath, ".")
if out, err := tarCmd.CombinedOutput(); err != nil {
    return fmt.Errorf("pack rootfs tar: %s: %w", strings.TrimSpace(string(out)), err)
}
defer os.Remove(rootfsTarPath)
```

**Note**: The tar is **uncompressed** (`-cf`, not `-czf`). The `tar-in` guestfish command receives it without a compression flag.

### 5.5 Step 4: Guestfish Script (Partition + Format + Copy)

All partitioning, formatting, and rootfs copying happens in a single guestfish script piped via stdin:

```go
// Actual guestfish script (image/pipeline/convert_linux.go)
script := fmt.Sprintf(`add %s
run
part-init /dev/sda gpt
part-add /dev/sda primary 2048 206847
part-set-gpt-type /dev/sda 1 C12A7328-F81F-11D2-BA4B-00A0C93EC93B
part-add /dev/sda primary 206848 -1
part-set-gpt-type /dev/sda 2 0FC63DAF-8483-4772-8E79-3D69D8477DE4
mkfs fat /dev/sda1
mkfs ext4 /dev/sda2
mount /dev/sda2 /
mkdir-p /boot/efi
mount /dev/sda1 /boot/efi
tar-in %s /
sync
umount-all
`, outputPath, rootfsTarPath)

gfCmd := exec.CommandContext(ctx, "guestfish")
gfCmd.Stdin = strings.NewReader(script)
if out, err := gfCmd.CombinedOutput(); err != nil {
    return fmt.Errorf("guestfish: %s: %w", strings.TrimSpace(string(out)), err)
}
```

Key details:
- ESP partition: 100MB (sectors 2048-206847), GPT type EFI System
- Root partition: rest of disk (sector 206848 to end), GPT type Linux filesystem
- ESP formatted as FAT32, root as ext4
- Rootfs copied via `tar-in` (not `copy-in` or `virt-copy-in`)
- Paths are validated for safe characters before interpolation (prevents injection)

### 5.6 Step 5: Validate GRUB Config (ensureGRUBConfig)

After the rootfs is copied, the code validates that a GRUB config exists and optionally updates it for serial console support:

```go
// Actual implementation (image/pipeline/convert_linux.go)
func ensureGRUBConfig(ctx context.Context, imagePath string) error {
    // 1. Detect GRUB config path (checks /boot/grub/grub.cfg and /boot/grub2/grub.cfg)
    grubPath, found, err := detectGRUBConfigPath(ctx, imagePath)
    if err != nil {
        return err
    }
    if !found {
        return fmt.Errorf("grub config not found in image (checked /boot/grub/grub.cfg and /boot/grub2/grub.cfg)")
    }

    // 2. Best-effort: inject serial console param via virt-customize (if available)
    if _, err := exec.LookPath("virt-customize"); err != nil {
        return nil  // virt-customize not required; skip silently
    }

    // Inject console=ttyS0,115200n8 into /etc/default/grub and regenerate
    ...
}

func detectGRUBConfigPath(ctx context.Context, imagePath string) (string, bool, error) {
    candidates := []string{"/boot/grub/grub.cfg", "/boot/grub2/grub.cfg"}
    for _, guestPath := range candidates {
        // guestfish --ro -a <image> -i is-file <path>
        ...
    }
    return "", false, nil
}
```

**Key difference from doc design**: There is no `BootloaderMissingError` struct. If the GRUB config is missing, a plain `fmt.Errorf` is returned. The UEFI bootloader binary itself (BOOTX64.EFI, etc.) is NOT checked during conversion -- that check only happens in `deepVerifyBoot` during on-demand `cocoon image verify`.

### 5.7 Complete Conversion Function

```go
// Actual implementation (image/pipeline/convert_linux.go)
func convertOCI(ctx context.Context, mountPath, outputPath, diskSize string) error {
    // Validate paths for safe characters (prevent guestfish injection)
    validateSafePath(mountPath)
    validateSafePath(outputPath)

    // 1. Create empty qcow2 image
    // 2. Check for guestfish
    // 3. Pack rootfs into tar archive (uncompressed)
    // 4. Run guestfish script (partition + format + tar-in)
    // 5. Validate GRUB config (ensureGRUBConfig)
    return nil
}
```

**Compatibility Scope** (what passes conversion):
| Image Type | GRUB Config | Passes? | Notes |
|------------|-----------|---------|-------|
| Ubuntu Cloud Image (qcow2) | Pre-installed | Yes | Recommended path (no OCI conversion needed) |
| Custom OCI with GRUB | grub.cfg present | Yes | Must be built with bootloader |
| Custom OCI without GRUB | Missing | **No** | Fails at `ensureGRUBConfig` |
| Fedora Cloud Image | grub2.cfg present | Yes | Uses /boot/grub2/grub.cfg path |
| Minimal OCI (alpine) | Missing | **No** | Application container, not VM image |

---

## 6. Checksum-Based Caching

### 6.1 Why Checksum-Based Caching?

**Problem**: Converting OCI images to qcow2 is expensive (5-60 seconds per image).

**Solution**: Cache converted images by content checksum. If the same OCI image is requested again, use the cached qcow2.

**Benefits**:
- Avoid redundant conversions
- Instant "pull" for previously seen images
- Automatic deduplication across tenants

### 6.2 Checksum Identity Contract (Normative)

This section defines the precise checksum algorithm used for cache filenames,
`references.json` keys, and conversion lock names. It is consistent with
[05-storage-management.md Image Checksum Identity](./05-storage-management.md#image-checksum-identity-normative),
which is the single source of truth for filesystem paths.

**Why not image tag?** Tags are mutable (`ubuntu:22.04` can point to different content over time).

**Why manifest?** Manifest is immutable and content-addressable.

#### For OCI Images (Primary Path)

```
checksum = SHA256(
    manifest.config.digest + "\n" +
    manifest.layers[*].digest.join("\n") + "\n" +
    platform_os + "/" + platform_arch       // e.g., "linux/amd64"
)
```

- Layer digests are joined in **manifest order** -- the OCI spec guarantees layer
  ordering within a manifest is immutable and meaningful (it encodes the
  filesystem stacking sequence). Sorting would lose this ordering and could map
  two semantically different images to the same checksum.
- The platform string (`linux/amd64`) is appended to distinguish identical layer
  sets built for different architectures.
- The full 64-character hex digest is computed; the first **16 hex characters**
  (64 bits) are used for filenames and keys. The full digest is stored in
  `references.json` metadata for collision verification.
- Cache filename: `{checksum_16}_{arch}.qcow2` (e.g., `a1b2c3d4e5f6a7b8_amd64.qcow2`)

**Multi-arch manifest lists**: When skopeo returns a manifest list
(`mediaType: application/vnd.oci.image.index.v1+json`), resolve to the
platform-specific manifest FIRST using `runtime.GOARCH`, then compute the
checksum above on the resolved single-platform manifest.

#### For Cloud Images (qcow2/img files)

```
checksum = SHA256(file_content)[:16]
arch     = runtime.GOARCH (always uses host architecture)
```

#### For URL-Based Images

```
checksum = SHA256(downloaded_file_content)[:16]
arch     = runtime.GOARCH (always uses host architecture)
```

**Note on architecture**: For all image types, the architecture defaults to
`runtime.GOARCH` via `goarchToOCI()`. There is no detection from image contents.

#### Implementation

```go
// Actual implementation (image/pipeline/oci_linux.go)
func computeOCIChecksum(configDigest string, layerDigests []string, arch string) (fullDigest string, checksum string) {
    var sb strings.Builder
    sb.WriteString(configDigest)
    sb.WriteString("\n")
    sb.WriteString(strings.Join(layerDigests, "\n"))
    sb.WriteString("\n")
    sb.WriteString("linux/" + arch)

    hash := sha256.Sum256([]byte(sb.String()))
    fullDigest = hex.EncodeToString(hash[:])
    checksum = fullDigest[:checksumHexLen]  // checksumHexLen = 16
    return fullDigest, checksum
}

// Actual implementation (image/pipeline/checksum.go)
func computeFileChecksum(path string) (fullDigest string, checksum string, err error) {
    f, err := os.Open(path)
    if err != nil { return "", "", err }
    defer f.Close()

    h := sha256.New()
    io.Copy(h, f)

    fullDigest = hex.EncodeToString(h.Sum(nil))
    checksum = fullDigest[:checksumHexLen]
    return fullDigest, checksum, nil
}
```

The `ImageIdentity` struct (`image/types.go`):

```go
type ImageIdentity struct {
    Checksum    string    // 16-char hex prefix of SHA-256
    Arch        string    // "amd64", "arm64", etc.
    FullDigest  string    // Full 64-char hex SHA-256 (for collision checks)
    SourceRef   string    // Original image reference
    ImageType   ImageType // OCI, URL, or LocalFile
    TempPath    string    // Transient: path to pulled image (not persisted)
    ContainerID string    // Transient: buildah container ID (not persisted)
}

func (id *ImageIdentity) BaseKey() string {
    return id.Checksum + "_" + id.Arch
}

func (id *ImageIdentity) CacheFilename() string {
    return id.Checksum + "_" + id.Arch + ".qcow2"
}
```

### 6.3 Cache Lookup and Storage

Cache operations use atomic rename (not `cp --reflink=auto`):

```go
// Actual implementation (image/pipeline/manager.go)
// Convert writes to a .tmp file, then atomically renames into cache.

tmpPath := basePath + ".tmp"
defer func() { _ = os.Remove(tmpPath) }()

// ... conversion writes to tmpPath ...

// Atomic rename into cache.
if err := os.Rename(tmpPath, basePath); err != nil {
    return "", fmt.Errorf("convert %s: rename to cache: %w", baseKey, err)
}
```

Cache lookup is a simple `os.Stat`:

```go
basePath := m.cfg.BaseImagePath(baseKey)
if _, err := os.Stat(basePath); err == nil {
    // Cache hit
    return identity, basePath, nil
}
```

### 6.4 Complete Pipeline with Caching (prepareOCI)

The OCI-specific `Prepare` pipeline runs the identify/pull/convert inside a
per-image conversion lock to prevent parallel pulls of the same image:

```go
// Actual implementation (image/pipeline/manager.go)
func (m *manager) prepareOCI(ctx context.Context, ref string) (*image.ImageIdentity, string, error) {
    // Phase 1: Identify (skopeo inspect) -- cheap, outside lock.
    identity, err := identifyOCIPlatform(ctx, ref)

    baseKey := identity.BaseKey()
    basePath := m.cfg.BaseImagePath(baseKey)

    // Phase 2: Fast-path cache check (no lock).
    if _, statErr := os.Stat(basePath); statErr == nil {
        return identity, basePath, nil  // Cache hit
    }

    // Phase 3: Acquire per-image conversion lock (Level 3).
    lockPath := m.cfg.ConversionLockPath(baseKey)
    fl := flock.New(lockPath)
    fl.Lock()
    defer fl.Unlock()

    // Phase 4: Double-check cache after lock (another process may have finished).
    if _, statErr := os.Stat(basePath); statErr == nil {
        return identity, basePath, nil  // Cache hit
    }

    // Phase 5: Pull + mount (buildah) -- inside lock.
    pullAndMountOCIPlatform(ctx, m.cfg, identity)

    // Phase 6: Convert OCI rootfs -> qcow2 -- inside lock.
    tmpPath := basePath + ".tmp"
    convertOCI(ctx, identity.TempPath, tmpPath, "10G")

    // Atomic rename into cache.
    os.Rename(tmpPath, basePath)

    // Cleanup buildah container.
    cleanupBuildahContainer(identity.ContainerID, m.cfg)

    return identity, basePath, nil
}
```

---

## 7. Manifest Refcache

### 7.1 Overview

The refcache (`image/refcache/index.go`) provides a persistent mapping from IMAGE_REF strings to base_key values. This allows fast cache lookups without re-inspecting remote registries.

**File location**: `{ManifestCacheDir}/index.json` (typically `/var/lib/cocoon/cache/manifests/index.json`)

### 7.2 Entry Structure

```go
// Actual implementation (image/refcache/index.go)
type Entry struct {
    BaseKey    string   `json:"base_key,omitempty"`     // Single mapping
    BaseKeys   []string `json:"base_keys,omitempty"`    // Multiple candidates (ambiguous)
    DigestFull string   `json:"digest_full,omitempty"`  // Full SHA-256 for collision check
    LastSeenAt string   `json:"last_seen_at"`           // RFC3339 timestamp
}
```

The index file is a `map[string]Entry` where keys are ref variants.

### 7.3 Alias Resolution

When a ref is upserted, the refcache generates multiple candidate keys (aliases) to support ergonomic lookups:

- Exact ref (e.g., `docker.io/library/ubuntu:22.04`)
- Basename (e.g., `ubuntu:22.04`)
- Without known extensions (`.qcow2`, `.img`, `.raw`, `.iso`)
- Without architecture suffix (`-amd64`, `-arm64`, `-x86_64`, `-aarch64`)
- Simplified alias (strip `-server-`, collapse `--`)

For OCI refs, tag-less aliases are also generated (e.g., `ubuntu` from `ubuntu:22.04`).

### 7.4 Ambiguity Handling

If an alias maps to multiple base_keys (e.g., `ubuntu` was used for two different images), the entry stores all candidates in `BaseKeys`. Resolution returns `ErrAmbiguousImageRef` for ambiguous matches, forcing the user to provide a more specific ref.

### 7.5 API

```go
// Upsert records IMAGE_REF -> base_key mapping with all aliases.
func Upsert(cfg *config.CocoonConfig, ref, baseKey, digestFull string) error

// ResolveBaseKey looks up IMAGE_REF -> base_key from local manifest cache.
// Returns (baseKey, true, nil) on unique match, ("", false, nil) on miss,
// or ("", false, ErrAmbiguousImageRef) on ambiguous match.
func ResolveBaseKey(cfg *config.CocoonConfig, ref string) (string, bool, error)

// RefsForBaseKey returns all IMAGE_REF aliases that map to a given base_key.
func RefsForBaseKey(cfg *config.CocoonConfig, baseKey string) ([]string, string, error)

// DeleteByBaseKey removes all mappings pointing to base_key.
func DeleteByBaseKey(cfg *config.CocoonConfig, baseKey string) error
```

### 7.6 Usage in Pipeline

The `Prepare()` method consults the refcache before pulling non-OCI images:

```go
// Actual usage in manager.go Prepare()
if baseKey, found, _ := refcache.ResolveBaseKey(m.cfg, ref); found {
    basePath := m.cfg.BaseImagePath(baseKey)
    if _, statErr := os.Stat(basePath); statErr == nil {
        // Manifest cache hit + base image exists -> skip pull entirely
        return identity, basePath, nil
    }
}
```

---

## 8. Error Handling

### 8.1 Error Classification

The code uses `ClassifiedError` from `types/errors.go` to distinguish transient (retriable) errors from permanent ones:

```go
// Actual implementation (types/errors.go)
type ErrorCategory string

const (
    ErrorCategoryTransient ErrorCategory = "transient"
    ErrorCategoryPermanent ErrorCategory = "permanent"
)

type ClassifiedError struct {
    Category ErrorCategory
    Err      error
}

func (e *ClassifiedError) Error() string {
    return fmt.Sprintf("[%s] %s", e.Category, e.Err.Error())
}

func (e *ClassifiedError) Unwrap() error { return e.Err }

func NewTransientError(err error) *ClassifiedError {
    return &ClassifiedError{Category: ErrorCategoryTransient, Err: err}
}

func NewPermanentError(err error) *ClassifiedError {
    return &ClassifiedError{Category: ErrorCategoryPermanent, Err: err}
}

func IsTransient(err error) bool {
    var ce *ClassifiedError
    if errors.As(err, &ce) {
        return ce.Category == ErrorCategoryTransient
    }
    return false
}
```

### 8.2 Skopeo Error Classification

```go
// Actual implementation (image/pipeline/oci_linux.go)
func classifySkopeoError(err error) error {
    msg := err.Error()
    // Permanent: unauthorized, denied, not found, manifest unknown, NAME_UNKNOWN
    if strings.Contains(msg, "unauthorized") ||
        strings.Contains(msg, "authentication required") ||
        strings.Contains(msg, "denied") ||
        strings.Contains(msg, "not found") ||
        strings.Contains(msg, "manifest unknown") ||
        strings.Contains(msg, "NAME_UNKNOWN") {
        return types.NewPermanentError(err)
    }
    // Everything else is transient (network/timeout)
    return types.NewTransientError(err)
}
```

### 8.3 Buildah Error Classification

```go
// Actual implementation (image/pipeline/oci_linux.go)
func classifyBuildahError(err error) error {
    msg := strings.ToLower(err.Error())
    // Permanent: auth failures (401, 403, unauthorized)
    if strings.Contains(msg, "unauthorized") ||
        strings.Contains(msg, "authentication required") ||
        strings.Contains(msg, "403") || strings.Contains(msg, "401") {
        return types.NewPermanentError(err)
    }
    // Transient: connection refused, timeout, temporary failure, i/o timeout
    if strings.Contains(msg, "connection refused") ||
        strings.Contains(msg, "timeout") ||
        strings.Contains(msg, "temporary failure") ||
        strings.Contains(msg, "i/o timeout") {
        return types.NewTransientError(err)
    }
    return types.NewPermanentError(err)
}
```

### 8.4 HTTP Error Classification (URL pulls)

```go
// Actual: HTTP status code classification in pullURL()
if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
    return nil, types.NewTransientError(httpErr)
}
if resp.StatusCode >= 400 {
    return nil, types.NewPermanentError(httpErr)
}
```

### 8.5 Error Classification Summary

| Error Type | Category | Recovery Strategy | User Action Required |
|------------|----------|-------------------|---------------------|
| **Network Error** | Transient | Classify as transient | Check network connectivity |
| **Image Not Found** | Permanent | Fail immediately | Verify image name and registry |
| **Authentication Failed** | Permanent | Fail with clear message | Provide credentials |
| **Not Bootable** | Permanent | Fail with validation report | Use bootable base image |
| **Tool Missing** | Permanent | Fail with install instructions | Install dependencies |

### 8.6 Retry Logic

> **Not Yet Implemented** -- Future Work

Error classification (`ClassifiedError` with transient/permanent) exists and is used throughout the pipeline, but **no automatic retry logic is implemented**. The `IsTransient()` helper is available for callers who want to implement retry, but the pipeline itself does not retry failed operations.

**Planned**: Retry with exponential backoff for transient errors (network timeouts, HTTP 429/5xx) is planned for Phase 2.

### 8.7 Cleanup on Failure

Cleanup is handled via deferred functions throughout the pipeline:

```go
// Actual patterns used in manager.go:

// Temp file cleanup
tmpPath := basePath + ".tmp"
defer func() { _ = os.Remove(tmpPath) }()

// Buildah container cleanup on failure
if identity.ContainerID != "" {
    cleanupBuildahContainer(identity.ContainerID, m.cfg)
}

// Rootfs tar cleanup in convertOCI
defer os.Remove(rootfsTarPath)
```

---

## 9. Design Principles

The conversion pipeline follows these key design principles:

1. **Idempotent Operations**: Same input always produces the same output
2. **Checksum-Based Caching**: Never convert the same image twice
3. **Fail-Fast Validation**: Verify boot contract compliance early
4. **Atomic Writes**: Use temp + rename for crash safety

---

## 10. Implementation Checklist

### 10.1 Phase 1: Core Conversion Pipeline (P0) -- Implemented

- [x] **Buildah Integration**:
  - [x] Shell-out via `runCmd()` with `--root` flag
  - [x] Image pull operation (`buildah pull`)
  - [x] Container create and mount (`buildah from`, `buildah mount`)
  - [x] Cleanup (`cleanupBuildahContainer`)

- [x] **Identification**:
  - [x] OCI manifest inspection via skopeo
  - [x] Multi-arch manifest list resolution
  - [x] Content-addressed checksum computation

- [x] **Conversion**:
  - [x] Create empty qcow2 images (`qemu-img create`)
  - [x] Create GPT partition table (ESP + root)
  - [x] Format partitions (FAT32 ESP, ext4 root)
  - [x] Copy rootfs via tar-in
  - [x] Validate GRUB config presence (fail if missing)
  - [x] Best-effort serial console injection (virt-customize)

- [x] **Caching**:
  - [x] Calculate OCI manifest checksums
  - [x] Atomic rename into cache
  - [x] Per-image conversion locks (Level 3)
  - [x] Double-check cache after lock acquisition
  - [x] Manifest refcache for fast lookups

- [x] **Bootability Verification** (on-demand):
  - [x] Basic verification (qemu-img check + format detection)
  - [x] Deep verification via guestfish (kernel, initrd, systemd, bootloader)

- [x] **Error Handling**:
  - [x] ClassifiedError (transient/permanent)
  - [x] Skopeo and buildah error classification
  - [x] HTTP error classification for URL pulls
  - [x] Automatic cleanup on failure (deferred functions)

### 10.2 Phase 2: Production Hardening (P1) -- Future Work

- [x] **Authentication** (OCI VM images):
  - [x] Explicit private registry login via `cocoon image login` (`oci/login.go`)
  - [x] Credential storage in `~/.cocoon/config.json` with Docker-compatible auth format
  - [ ] Token refresh
  - [ ] Credential helper integration (e.g., docker-credential-ecr-login)

- [ ] **Progress Tracking**:
  - [ ] OCI pull progress reporting
  - [ ] Conversion progress updates

- [ ] **Retry Logic**:
  - [ ] Automatic retry with exponential backoff for transient errors
  - [ ] Configurable retry count and backoff parameters

- [ ] **Optimization**:
  - [ ] Parallel image pulls
  - [ ] Compressed qcow2 images
  - [ ] Copy-on-write optimizations

- [ ] **Testing**:
  - [ ] Integration tests with real images

### 10.3 Phase 3: Advanced Features (P2) -- Future Work

- [ ] **Architecture Detection**:
  - [ ] Detect architecture from kernel binary in image
  - [ ] Cross-architecture support

- [ ] **Custom Bootloaders**:
  - [ ] systemd-boot support
  - [ ] Custom GRUB configurations

- [ ] **Image Optimization**:
  - [ ] Remove unnecessary files from rootfs
  - [ ] Compress images with zstd
  - [ ] Minimize qcow2 size

- [ ] **Monitoring**:
  - [ ] Conversion time metrics
  - [ ] Cache hit rate tracking
  - [ ] Disk space usage monitoring

---

## 11. Verified Images (CI Reference)

Phase 1 requires at least one **pinned reference image** per source type for full-pipeline CI verification (conversion -> boot detection -> lifecycle). These images have fixed digests and known-good checksums, ensuring deterministic CI runs.

### 11.1 Reference Cloud Image (qcow2)

**Ubuntu 22.04 Cloud Image** -- primary CI image for boot + lifecycle tests:

| Field | Value |
|-------|-------|
| **URL** | `https://cloud-images.ubuntu.com/releases/22.04/release/ubuntu-22.04-server-cloudimg-amd64.img` |
| **Pinned Release** | `20240126` (pin to a specific date release for reproducibility) |
| **Pinned URL** | `https://cloud-images.ubuntu.com/releases/22.04/release-20240126/ubuntu-22.04-server-cloudimg-amd64.img` |
| **SHA256** | Pin in `test/fixtures/verified-images.sha256` -- placeholder until Phase 1 CI setup (update on deliberate image bump only) |
| **Format** | qcow2 (direct use, no conversion) |
| **Boot Mode** | UEFI (default), direct kernel boot (Phase 2) |
| **Guest init** | Users may optionally install cloud-init for guest initialization |

**CI Usage**:
```bash
# Download and verify (cached in CI, re-downloaded on checksum mismatch)
wget -q -O test-image.img "$PINNED_URL"
sha256sum -c test/fixtures/verified-images.sha256

# Full lifecycle pipeline
cocoon create test-image.img --name ci-boot-test --cpus 1 --memory 1G --disk 5G
cocoon start ci-boot-test --boot-timeout 120
cocoon logs ci-boot-test --tail 20  # Verify boot markers
cocoon inspect ci-boot-test         # Verify state == RUNNING
cocoon stop ci-boot-test
cocoon delete ci-boot-test
```

### 11.2 Reference Bootable OCI Image

**Purpose**: Validates the complete OCI conversion pipeline (pull -> extract -> convert -> boot).

| Field | Value |
|-------|-------|
| **Registry** | `ghcr.io/CMGS/cocoon-test-images/ubuntu-bootable` |
| **Tag** | `22.04` |
| **Pinned Digest** | Pin as `ghcr.io/CMGS/cocoon-test-images/ubuntu-bootable@sha256:<digest>` in CI config |
| **Contents** | Ubuntu 22.04 + kernel + GRUB + systemd |
| **Architecture** | `linux/amd64` |

**CI Usage**:
```bash
# Pull by digest (immutable, deterministic)
cocoon image pull "ghcr.io/CMGS/cocoon-test-images/ubuntu-bootable@sha256:${PINNED_DIGEST}"

# Verify bootability (post-conversion, on-demand)
cocoon image verify "ghcr.io/CMGS/cocoon-test-images/ubuntu-bootable@sha256:${PINNED_DIGEST}"

# Full conversion + boot + lifecycle pipeline
cocoon create "ghcr.io/CMGS/cocoon-test-images/ubuntu-bootable@sha256:${PINNED_DIGEST}" \
  --name ci-oci-test --cpus 1 --memory 1G --disk 5G
cocoon start ci-oci-test --boot-timeout 180
cocoon logs ci-oci-test --tail 20     # Verify systemd boot markers
cocoon inspect ci-oci-test            # Verify state == RUNNING
cocoon stop ci-oci-test
cocoon delete ci-oci-test
```

### 11.3 CI Verification Matrix

The following pipeline stages MUST pass for every PR:

| Stage | Cloud Image (qcow2) | Bootable OCI Image |
|-------|---------------------|-------------------|
| **Image fetch** | Download + SHA256 verify | Pull by digest |
| **OCI->qcow2 conversion** | N/A (already qcow2) | Buildah extract -> guestfish convert |
| **Bootability verification** | `cocoon image verify` (post-conversion) | `cocoon image verify` (post-conversion) |
| **Direct kernel boot** | Boot with `payload.kernel` + `payload.initramfs` | Boot with `payload.kernel` + `payload.initramfs` |
| **Boot detection** | Serial log -> systemd markers | Serial log -> systemd markers |
| **Lifecycle** | create -> start -> inspect -> stop -> delete | create -> start -> inspect -> stop -> delete |
| **Crash recovery** | kill -9 CH -> `cocoon doctor --fix` | kill -9 CH -> `cocoon doctor --fix` |
| **GC** | Delete VM -> `cocoon gc --dry-run` | Delete VM -> `cocoon gc --dry-run` |

### 11.4 Maintaining Verified Images

**When to bump**:
- Kernel CVE fix in upstream cloud image
- New distro release needed for coverage

**How to bump**:
1. Update URL/digest in `test/fixtures/verified-images.sha256` (or CI config)
2. Run full CI pipeline manually against new image
3. Commit with message: `ci: bump verified image to <new-version>`
4. **Never** use floating tags (`:latest`, `:22.04`) in CI -- always pin to digest or dated release

---

## 12. References

### 12.1 Related Documents

- [01-boot-contract.md](01-boot-contract.md) - Boot requirements and VM lifecycle
- [05-storage-management.md](05-storage-management.md) - COW storage, garbage collection, and **Image Checksum Identity** (normative)
- [06-concurrency.md](06-concurrency.md) - Conversion lock keys use the same `{checksum}_{arch}` identity

### 12.2 Source Files

| File | Purpose |
|------|---------|
| `image/pipeline/manager.go` | Main pipeline: Pull, Convert, Prepare, VerifyBootability, ListCached, RemoveCached |
| `image/pipeline/oci_linux.go` | OCI-specific: identifyOCIPlatform, pullAndMountOCIPlatform, runCmd, error classification |
| `image/pipeline/convert_linux.go` | Conversion: convertOCI, ensureGRUBConfig, guestfish script |
| `image/pipeline/checksum.go` | Checksum: computeFileChecksum, goarchToOCI, defaultArch |
| `image/pipeline/cleanup_linux.go` | Cleanup: cleanupBuildahContainer |
| `image/pipeline/verify_linux.go` | Deep boot verification: deepVerifyBoot |
| `image/pipeline/format.go` | Format detection: detectImageFormat via qemu-img info |
| `image/refcache/index.go` | Manifest refcache: Upsert, ResolveBaseKey, alias generation |
| `image/types.go` | Types: ImageIdentity, BootCheckResult, CachedImage, ImageType |
| `types/errors.go` | Error types: ClassifiedError, IsTransient, ErrorType constants |
| `types/reference.go` | Reference types: ParseBaseKey, FormatBaseKey, ReferenceEntry |

### 12.3 External Tools

| Tool | Purpose | Documentation |
|------|---------|---------------|
| Buildah | OCI image operations | https://buildah.io/ |
| qemu-img | qcow2 image creation | https://www.qemu.org/docs/master/tools/qemu-img.html |
| libguestfs | Disk image manipulation | https://libguestfs.org/ |
| skopeo | OCI manifest inspection | https://github.com/containers/skopeo |

### 12.4 Installation

**Ubuntu/Debian**:
```bash
apt-get install buildah qemu-utils libguestfs-tools skopeo
```

**Fedora/RHEL**:
```bash
dnf install buildah qemu-img guestfs-tools skopeo
```

**Arch Linux**:
```bash
pacman -S buildah qemu libguestfs skopeo
```

---

## Appendix A: Example Complete Workflow

```go
// Illustrative: Shows the conceptual flow. Actual implementation uses
// the manager.Prepare() method which handles all steps internally.
package main

import (
    "context"
    "fmt"
    "log"
)

func main() {
    // 1. Create manager with config and reference counter
    mgr := pipeline.New(cfg, refCtr)

    // 2. Prepare base image (identify + cache check + pull + convert)
    // For OCI images, this runs:
    //   a. identifyOCIPlatform (skopeo inspect -> checksum)
    //   b. Cache check (os.Stat on base image path)
    //   c. Acquire conversion lock
    //   d. pullAndMountOCIPlatform (buildah --root <root> pull/from/mount)
    //   e. convertOCI (qemu-img create, tar, guestfish, ensureGRUBConfig)
    //   f. os.Rename into cache (atomic)
    //   g. cleanupBuildahContainer
    ref := "myorg/ubuntu-bootable:22.04"
    identity, basePath, err := mgr.Prepare(context.Background(), ref)
    if err != nil {
        log.Fatalf("Failed to prepare image: %v", err)
    }

    fmt.Printf("Base image ready: %s (key: %s)\n", basePath, identity.BaseKey())

    // 3. Optionally verify bootability (on-demand, post-conversion)
    result, err := mgr.VerifyBootability(context.Background(), basePath)
    if err != nil {
        log.Fatalf("Verification error: %v", err)
    }
    fmt.Printf("Bootable: %v, Modes: %v\n", result.Bootable, result.BootModes)

    // 4. Create VM overlay disk (covered in 05-storage-management.md)
    // 5. Register reference (covered in 05-storage-management.md)
}
```

---

**End of OCI Conversion Pipeline Documentation v1.1**

# OCI to qcow2 Conversion Pipeline

**Version**: 1.0
**Status**: Draft
**Priority**: P0 - Required for core functionality

## ⚠️ Root Access Requirement

**IMPORTANT**: OCI image conversion requires root privileges due to libguestfs dependency.

**Deployment Strategy**:
- **Option A (Rootless)**: Conversion is NOT available - use cloud images (qcow2) directly
- **Option B (Rootful)**: Run cocoon as root - NOT recommended for production
- **Option C (Hybrid)**: Recommended - cocoon runs as user, privileged helper for conversion only

See [00-overview.md § Deployment Strategy](./00-overview.md#deployment-strategy) and [08-dependencies.md](./08-dependencies.md) for details.

## Executive Summary

This document specifies the pipeline for converting OCI container images into bootable qcow2 disk images for Cloud Hypervisor VMs. The conversion process must produce images that satisfy the [Boot Contract](01-boot-contract.md) while maintaining efficiency through caching and deduplication.

**Key Requirements**:
1. Pull OCI images from registries using Buildah
2. Extract container rootfs to disk
3. Convert rootfs to qcow2 format with proper partitioning
4. Make the image bootable (bootloader, kernel, init)
5. Cache images based on content checksums
6. Handle rootless and rootful operation modes (conversion requires root)
7. Provide robust error handling

---

## ⚠️ Code Examples Disclaimer

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

**Example**:
```go
// MUST: Implementation-required validation
func ValidateBootability(rootfs string) error {
    // This exact check is mandatory
    if !pathExists(filepath.Join(rootfs, "/boot/vmlinuz*")) {
        return ErrKernelNotFound
    }
    return nil
}
```

### Responsibility Boundaries

**Cocoon's Role**:
- ✅ **Validate**: Check if image has required components (kernel, bootloader)
- ✅ **Warn**: Alert if cloud-init missing (VM will boot but metadata server disabled)
- ✅ **Configure**: Modify GRUB config, inject cloud-init datasource settings (if cloud-init present)
- ❌ **Install**: Does NOT install missing packages (kernel, GRUB, cloud-init)

**User's Role** (Image Provider):
- **MUST provide**: kernel, bootloader **pre-installed**
- **SHOULD provide**: cloud-init **pre-installed** (required for metadata server integration)
- Cocoon only verifies and configures existing components

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
7. [Error Handling](#7-error-handling)
8. [Rootless vs Rootful Considerations](#8-rootless-vs-rootful-considerations)
9. [Implementation Checklist](#9-implementation-checklist)
10. [Verified Images (CI Reference)](#10-verified-images-ci-reference)
11. [References](#11-references)

---

## 1. Architecture Overview

### 1.1 Conversion Pipeline

```
┌─────────────────┐
│  OCI Registry   │ (Docker Hub, GHCR, etc.)
└────────┬────────┘
         │ buildah pull
         ▼
┌─────────────────┐
│ OCI Image Store │ (~/.local/share/containers/storage)
└────────┬────────┘
         │ buildah from + mount
         ▼
┌─────────────────┐
│  Mounted Rootfs │ (/tmp/buildah-mount-XXXXX)
└────────┬────────┘
         │ Conversion Tools
         ▼
┌─────────────────┐
│  Base qcow2     │ (Bootable disk image)
└────────┬────────┘
         │ Checksum + Cache
         ▼
┌─────────────────┐
│  Image Cache    │ (/var/lib/cocoon/cache/images/)
└─────────────────┘
         │ COW Backing File
         ▼
┌─────────────────┐
│ VM Overlay Disk │ (Per-VM instance)
└─────────────────┘
```

### 1.2 Component Responsibilities

| Component | Responsibility | Tool |
|-----------|----------------|------|
| **Image Puller** | Download OCI images from registries | Buildah |
| **Rootfs Extractor** | Mount and extract container filesystem | Buildah mount |
| **qcow2 Converter** | Create disk image with partitions | qemu-img, libguestfs |
| **Bootloader Validator** | Validate bootloader exists, update grub.cfg | virt-customize, guestfish |
| **Cache Manager** | Deduplicate and cache base images | Custom Go code |
| **Checksum Calculator** | Generate stable image checksums | skopeo, Go crypto |

### 1.3 Design Principles

1. **Idempotent Operations**: Same input produces same output
2. **Checksum-Based Caching**: Never convert the same image twice
3. **Fail-Fast Validation**: Verify boot contract before VM creation
4. **Rootless-First**: Design for unprivileged operation
5. **Shell-Out Strategy**: Use external tools via exec (Buildah, qemu-img, libguestfs)

---

## 2. Buildah Integration

### 2.1 Why Buildah?

**Decision**: Use Buildah for OCI image operations instead of Docker or containerd libraries.

**Rationale**:
- ✅ **Rootless by default**: Works without root privileges
- ✅ **OCI-compliant**: Handles any OCI-compatible registry
- ✅ **Simple CLI interface**: Easy to shell out from Go
- ✅ **No daemon required**: Lightweight, no background process
- ✅ **Battle-tested**: Used in production by Podman, OpenShift

**Alternatives Considered**:

| Alternative | Pros | Cons | Decision |
|-------------|------|------|----------|
| Docker CLI | Familiar, widely available | Requires Docker daemon, rootful | ❌ Not selected |
| containerd library | Native Go, no shell-out | Complex API, daemon required | ❌ Not selected |
| Podman | Same CLI as Docker | Uses Buildah internally anyway | ❌ Redundant |
| **Buildah** | **Rootless, simple, no daemon** | **None** | ✅ **Selected** |

### 2.2 Shell-Out vs Library

**Decision**: Shell out to Buildah CLI instead of using Buildah as a Go library.

**Rationale**:
- ✅ **Stability**: CLI interface is stable, library internals change frequently
- ✅ **Error handling**: Easier to capture stderr and parse error messages
- ✅ **Process isolation**: Buildah runs in separate process, clean crash handling
- ✅ **Version flexibility**: Works with any Buildah version installed on host
- ⚠️ **Performance**: Minor overhead from process spawning (acceptable for infrequent operations)

**Implementation Pattern**:

```go
package oci

import (
    "bytes"
    "fmt"
    "os/exec"
    "strings"
)

type BuildahClient struct {
    executable string // Path to buildah binary
}

func NewBuildahClient() (*BuildahClient, error) {
    // Find buildah in PATH
    path, err := exec.LookPath("buildah")
    if err != nil {
        return nil, fmt.Errorf("buildah not found: %w", err)
    }

    return &BuildahClient{executable: path}, nil
}

func (b *BuildahClient) run(args ...string) (string, error) {
    cmd := exec.Command(b.executable, args...)

    var stdout, stderr bytes.Buffer
    cmd.Stdout = &stdout
    cmd.Stderr = &stderr

    if err := cmd.Run(); err != nil {
        return "", fmt.Errorf("buildah error: %w\nstderr: %s", err, stderr.String())
    }

    return strings.TrimSpace(stdout.String()), nil
}
```

### 2.3 Buildah Storage Configuration

Buildah stores images in a local directory (default: `~/.local/share/containers/storage`).

**Custom Storage Location**:

```go
func (b *BuildahClient) WithStorage(storageRoot string) *BuildahClient {
    // Override BUILDAH_STORAGE environment variable
    b.storageRoot = storageRoot
    return b
}

func (b *BuildahClient) run(args ...string) (string, error) {
    cmd := exec.Command(b.executable, args...)

    if b.storageRoot != "" {
        cmd.Env = append(os.Environ(),
            fmt.Sprintf("BUILDAH_STORAGE=%s", b.storageRoot))
    }

    // ... rest of execution
}
```

**Recommended Storage Layout**:

```
/var/lib/cocoon/
├── cache/
│   ├── buildah/          # Buildah storage root
│   │   ├── overlay/      # OCI image layers
│   │   ├── vfs/
│   │   └── storage.lock
│   └── images/           # Converted qcow2 base images
└── vms/                  # Per-VM overlay disks
```

---

## 3. Pull Workflow

### 3.1 Image Pull Operation

**Goal**: Download OCI image from registry to local storage.

```go
func (b *BuildahClient) Pull(image string) error {
    // Normalize image name
    // "ubuntu:22.04" -> "docker.io/library/ubuntu:22.04"
    fullImage := b.normalizeImageName(image)

    // Pull image
    _, err := b.run("pull", fullImage)
    return err
}

func (b *BuildahClient) normalizeImageName(image string) string {
    // Add default registry if not specified
    if !strings.Contains(image, "/") {
        return "docker.io/library/" + image
    }

    if !strings.Contains(image, ".") {
        return "docker.io/" + image
    }

    return image
}
```

### 3.2 Image Verification

Before pulling, verify the image exists and is accessible:

```go
func (b *BuildahClient) Inspect(image string) (*ImageInfo, error) {
    output, err := b.run("inspect", "--type", "image", image)
    if err != nil {
        return nil, fmt.Errorf("image not found or inaccessible: %w", err)
    }

    var info ImageInfo
    if err := json.Unmarshal([]byte(output), &info); err != nil {
        return nil, fmt.Errorf("failed to parse image info: %w", err)
    }

    return &info, nil
}

type ImageInfo struct {
    Digest      string            `json:"digest"`
    RepoDigests []string          `json:"repoDigests"`
    Size        int64             `json:"size"`
    Config      ImageConfig       `json:"config"`
    RootFS      RootFSInfo        `json:"rootfs"`
}

type ImageConfig struct {
    Cmd        []string          `json:"Cmd"`
    Entrypoint []string          `json:"Entrypoint"`
    Env        []string          `json:"Env"`
    WorkingDir string            `json:"WorkingDir"`
}

type RootFSInfo struct {
    Type    string   `json:"type"`
    Layers  []string `json:"layers"`
}
```

### 3.3 Authenticated Pulls

For private registries, handle authentication:

```go
func (b *BuildahClient) Login(registry, username, password string) error {
    cmd := exec.Command(b.executable, "login",
        "--username", username,
        "--password-stdin",
        registry)

    cmd.Stdin = strings.NewReader(password)

    return cmd.Run()
}

func (b *BuildahClient) PullWithAuth(image, username, password string) error {
    // Extract registry from image name
    parts := strings.Split(image, "/")
    registry := parts[0]

    // Login
    if err := b.Login(registry, username, password); err != nil {
        return fmt.Errorf("authentication failed: %w", err)
    }

    // Pull
    return b.Pull(image)
}
```

### 3.4 Pull Progress Tracking

For large images, track download progress:

```go
func (b *BuildahClient) PullWithProgress(image string, progressChan chan<- PullProgress) error {
    fullImage := b.normalizeImageName(image)

    cmd := exec.Command(b.executable, "pull", fullImage)

    stderr, err := cmd.StderrPipe()
    if err != nil {
        return err
    }

    if err := cmd.Start(); err != nil {
        return err
    }

    // Parse progress from stderr
    scanner := bufio.NewScanner(stderr)
    for scanner.Scan() {
        line := scanner.Text()
        if progress := parsePullProgress(line); progress != nil {
            progressChan <- *progress
        }
    }

    return cmd.Wait()
}

type PullProgress struct {
    Layer       string
    Current     int64
    Total       int64
    Percentage  float64
}
```

---

## 4. Extract Workflow

### 4.1 Container Creation and Mount

**Goal**: Mount the OCI image layers as a unified filesystem.

```go
func (b *BuildahClient) From(image string) (string, error) {
    // Create working container from image
    containerID, err := b.run("from", image)
    if err != nil {
        return "", fmt.Errorf("failed to create container: %w", err)
    }

    return containerID, nil
}

func (b *BuildahClient) Mount(containerID string) (string, error) {
    // Mount container filesystem
    mountPoint, err := b.run("mount", containerID)
    if err != nil {
        return "", fmt.Errorf("failed to mount container: %w", err)
    }

    return mountPoint, nil
}

func (b *BuildahClient) Umount(containerID string) error {
    _, err := b.run("umount", containerID)
    return err
}

func (b *BuildahClient) Rm(containerID string) error {
    _, err := b.run("rm", containerID)
    return err
}
```

### 4.2 Complete Extract Operation

Combine create, mount, and cleanup:

```go
type MountedContainer struct {
    client      *BuildahClient
    containerID string
    mountPoint  string
}

func (b *BuildahClient) ExtractImage(image string) (*MountedContainer, error) {
    // 1. Create container
    containerID, err := b.From(image)
    if err != nil {
        return nil, err
    }

    // 2. Mount filesystem
    mountPoint, err := b.Mount(containerID)
    if err != nil {
        b.Rm(containerID) // Cleanup on failure
        return nil, err
    }

    return &MountedContainer{
        client:      b,
        containerID: containerID,
        mountPoint:  mountPoint,
    }, nil
}

func (m *MountedContainer) Path() string {
    return m.mountPoint
}

func (m *MountedContainer) Cleanup() error {
    // Umount
    if err := m.client.Umount(m.containerID); err != nil {
        return err
    }

    // Remove container
    return m.client.Rm(m.containerID)
}

// Ensure cleanup happens even on panic
func (m *MountedContainer) Close() error {
    return m.Cleanup()
}
```

### 4.3 Rootfs Validation

Before conversion, verify the rootfs has required components:

```go
func (m *MountedContainer) ValidateBootability() error {
    // MUST have components (mandatory)
    required := []struct {
        path    string
        isFile  bool
        message string
    }{
        {"/boot/vmlinuz", false, "kernel not found (no /boot/vmlinuz*)"},
        {"/boot/initrd", false, "initrd/initramfs not found (no /boot/initrd* or /boot/initramfs*)"},
        {"/sbin/init", true, "init system not found (/sbin/init missing)"},
        {"/boot/efi/EFI", false, "EFI bootloader not found (no ESP partition)"},
        {"/etc", false, "incomplete rootfs (missing /etc)"},
        {"/usr", false, "incomplete rootfs (missing /usr)"},
    }

    for _, req := range required {
        fullPath := filepath.Join(m.mountPoint, req.path)

        // Check if path exists (glob for kernel/initrd/initramfs)
        if strings.Contains(req.path, "vmlinuz") {
            matches, _ := filepath.Glob(fullPath + "*")
            if len(matches) == 0 {
                return fmt.Errorf("bootability check failed: %s", req.message)
            }
        } else if strings.Contains(req.path, "initrd") {
            // Support both initrd* and initramfs* (Fedora, RHEL use initramfs)
            matches1, _ := filepath.Glob(filepath.Join(m.mountPoint, "/boot/initrd*"))
            matches2, _ := filepath.Glob(filepath.Join(m.mountPoint, "/boot/initramfs*"))
            if len(matches1) == 0 && len(matches2) == 0 {
                return fmt.Errorf("bootability check failed: %s", req.message)
            }
        } else {
            if _, err := os.Stat(fullPath); os.IsNotExist(err) {
                return fmt.Errorf("bootability check failed: %s", req.message)
            }
        }
    }

    // Verify init is systemd (mandatory for Cocoon)
    initPath := filepath.Join(m.mountPoint, "/sbin/init")
    initTarget, err := os.Readlink(initPath)
    if err == nil {  // init is a symlink
        if !strings.Contains(initTarget, "systemd") {
            return fmt.Errorf("bootability check failed: init must be systemd, got: %s", initTarget)
        }
    }
    // If init is not a symlink, check if it's systemd binary directly
    // (some distros have systemd as /sbin/init directly)

    // Architecture-aware bootloader validation
    if err := m.ValidateBootloaderForArch(); err != nil {
        return fmt.Errorf("bootability check failed: %w", err)
    }

    // SHOULD have components (recommended, not mandatory)
    // cloud-init: CONDITIONAL
    // - REQUIRED: For Cocoon metadata server integration (SSH/user setup)
    // - OPTIONAL: VM will boot without it (standalone use case)
    cloudInitPath := filepath.Join(m.mountPoint, "/usr/bin/cloud-init")
    if _, err := os.Stat(cloudInitPath); os.IsNotExist(err) {
        log.Warn("cloud-init not found - VM will boot but Cocoon metadata server integration disabled")
    }

    return nil
}

// ValidateBootloaderForArch validates the bootloader exists for the detected architecture
func (m *MountedContainer) ValidateBootloaderForArch() error {
    // Detect architecture
    arch, err := DetectArchitecture(m.mountPoint)
    if err != nil {
        return fmt.Errorf("failed to detect architecture: %w", err)
    }

    // Check architecture-specific bootloader
    // Path semantics: /boot/efi/EFI/BOOT/BOOTX64.EFI is the "mounted path"
    // (ESP partition mounted at /boot/efi in the rootfs)
    // The actual ESP internal path is /EFI/BOOT/BOOTX64.EFI
    var bootloaderMountedPath string
    switch arch {
    case "x86_64":
        // Mounted path: where bootloader appears after ESP is mounted to /boot/efi
        bootloaderMountedPath = filepath.Join(m.mountPoint, "/boot/efi/EFI/BOOT/BOOTX64.EFI")
    case "aarch64":
        bootloaderMountedPath = filepath.Join(m.mountPoint, "/boot/efi/EFI/BOOT/BOOTAA64.EFI")
    default:
        return fmt.Errorf("unsupported architecture: %s", arch)
    }

    if _, err := os.Stat(bootloaderMountedPath); os.IsNotExist(err) {
        return fmt.Errorf("bootloader not found for %s at %s (ESP should be mounted at /boot/efi)",
            arch, bootloaderMountedPath)
    }

    return nil
}
```

**Architecture Detection**:

```go
func DetectArchitecture(mountPoint string) (string, error) {
    // Method 1: Check kernel binary architecture
    kernelPath, _ := filepath.Glob(filepath.Join(mountPoint, "/boot/vmlinuz*"))
    if len(kernelPath) > 0 {
        out, err := exec.Command("file", kernelPath[0]).Output()
        if err == nil {
            output := string(out)
            if strings.Contains(output, "x86-64") || strings.Contains(output, "x86_64") {
                return "x86_64", nil
            }
            if strings.Contains(output, "ARM aarch64") || strings.Contains(output, "aarch64") {
                return "aarch64", nil
            }
        }
    }

    // Method 2: Check dpkg/rpm architecture (for Debian/Ubuntu/Fedora)
    if _, err := os.Stat(filepath.Join(mountPoint, "/var/lib/dpkg")); err == nil {
        // Debian/Ubuntu
        archFile := filepath.Join(mountPoint, "/var/lib/dpkg/arch")
        if data, err := os.ReadFile(archFile); err == nil {
            arch := strings.TrimSpace(string(data))
            if arch == "amd64" {
                return "x86_64", nil
            }
            if arch == "arm64" {
                return "aarch64", nil
            }
        }
    }

    return "", fmt.Errorf("unable to detect architecture")
}
```

**Architecture-Specific Bootloader Validation**:

```go
func ValidateBootloaderForArch(mountPoint, arch string) error {
    var expectedBootloader string

    switch arch {
    case "x86_64":
        expectedBootloader = "/boot/efi/EFI/BOOT/BOOTX64.EFI"
    case "aarch64":
        expectedBootloader = "/boot/efi/EFI/BOOT/BOOTAA64.EFI"
    default:
        return fmt.Errorf("unsupported architecture: %s", arch)
    }

    bootloaderPath := filepath.Join(mountPoint, expectedBootloader)
    if _, err := os.Stat(bootloaderPath); os.IsNotExist(err) {
        return fmt.Errorf("bootloader not found for %s: %s", arch, expectedBootloader)
    }

    return nil
}
```

---

## 5. Convert Workflow

### 5.1 Conversion Steps Overview

```
Mounted Rootfs
    │
    ▼
1. Create empty qcow2 image
    │
    ▼
2. Create GPT partition table + ESP + root partition
    │
    ▼
3. Format ESP (FAT32) and root (ext4)
    │
    ▼
4. Copy rootfs to root partition
    │
    ▼
5. Validate UEFI bootloader (fail if missing)
    │
    ▼
6. Verify boot contract compliance
    │
    ▼
Bootable qcow2 Image
```

### 5.2 Step 1: Create Empty qcow2

```go
func CreateEmptyQcow2(outputPath string, size string) error {
    cmd := exec.Command("qemu-img", "create",
        "-f", "qcow2",
        outputPath,
        size)

    return cmd.Run()
}
```

**Example**:
```bash
qemu-img create -f qcow2 base.qcow2 10G
```

### 5.3 Step 2: Create Partition Table

**Option A: Using libguestfs (Recommended)**

```go
func CreatePartitions(imagePath string) error {
    // Use guestfish to partition the disk
    script := `
    # Add disk
    add %s
    run

    # Create GPT partition table
    part-init /dev/sda gpt

    # Create ESP (100MB, type: EFI System)
    part-add /dev/sda primary 2048 206847
    part-set-gpt-type /dev/sda 1 C12A7328-F81F-11D2-BA4B-00A0C93EC93B

    # Create root partition (rest of disk, type: Linux filesystem)
    part-add /dev/sda primary 206848 -1
    part-set-gpt-type /dev/sda 2 0FC63DAF-8483-4772-8E79-3D69D8477DE4
    `

    cmd := exec.Command("guestfish", "-a", imagePath)
    cmd.Stdin = strings.NewReader(fmt.Sprintf(script, imagePath))

    return cmd.Run()
}
```

**Option B: Manual with virt-format (Simpler but less control)**

```go
func FormatImageWithPartitions(imagePath string) error {
    // virt-format creates partition table + filesystem automatically
    cmd := exec.Command("virt-format",
        "--partition=gpt",
        "--filesystem=ext4",
        "-a", imagePath)

    return cmd.Run()
}
```

### 5.4 Step 3: Format Partitions

```go
func FormatPartitions(imagePath string) error {
    script := `
    add %s
    run

    # Format ESP as FAT32
    mkfs fat /dev/sda1

    # Format root as ext4
    mkfs ext4 /dev/sda2

    # Set ESP flags
    part-set-bootable /dev/sda 1 true
    `

    cmd := exec.Command("guestfish", "-a", imagePath)
    cmd.Stdin = strings.NewReader(fmt.Sprintf(script, imagePath))

    return cmd.Run()
}
```

### 5.5 Step 4: Copy Rootfs

**Method A: virt-copy-in (Simple)**

```go
func CopyRootfs(imagePath, rootfsPath string) error {
    // Copy all files from rootfs to image root
    cmd := exec.Command("virt-copy-in",
        "-a", imagePath,
        fmt.Sprintf("%s/*", rootfsPath),
        "/")

    return cmd.Run()
}
```

**Method B: guestfish (More control)**

```go
func CopyRootfsAdvanced(imagePath, rootfsPath string) error {
    script := `
    add %s
    run
    mount /dev/sda2 /

    # Create ESP mount point
    mkdir /boot/efi
    mount /dev/sda1 /boot/efi

    # Copy rootfs contents
    copy-in %s/* /

    sync
    `

    cmd := exec.Command("guestfish", "-a", imagePath)
    cmd.Stdin = strings.NewReader(fmt.Sprintf(script, imagePath, rootfsPath))

    return cmd.Run()
}
```

**Method C: tar-in (Most efficient)**

```go
func CopyRootfsFromTar(imagePath, tarPath string) error {
    script := `
    add %s
    run
    mount /dev/sda2 /

    # Extract tar directly into image
    tar-in %s / compress:gzip

    sync
    `

    cmd := exec.Command("guestfish", "-a", imagePath)
    cmd.Stdin = strings.NewReader(fmt.Sprintf(script, imagePath, tarPath))

    return cmd.Run()
}
```

### 5.6 Step 5: Validate Bootloader (Fail-Fast)

**Strategy**: Cocoon does NOT install missing bootloaders or packages (see [Responsibility Boundaries](#responsibility-boundaries)). It validates that the required components exist and fails fast with a clear error if they don't.

For images that already have GRUB installed, Cocoon may **update** `grub.cfg` to ensure correct boot parameters (e.g., serial console, root device), but never installs GRUB from scratch.

```go
func ValidateBootloader(imagePath string) error {
    // Detect architecture
    arch, err := DetectImageArchitecture(imagePath)
    if err != nil {
        return fmt.Errorf("failed to detect architecture: %w", err)
    }

    // Check architecture-specific UEFI bootloader
    var bootloaderPath string
    switch arch {
    case "x86_64":
        bootloaderPath = "/boot/efi/EFI/BOOT/BOOTX64.EFI"
    case "aarch64":
        bootloaderPath = "/boot/efi/EFI/BOOT/BOOTAA64.EFI"
    default:
        return fmt.Errorf("unsupported architecture: %s", arch)
    }

    hasBootloader, err := guestfishExists(imagePath, bootloaderPath)
    if err != nil {
        return err
    }
    if !hasBootloader {
        return &BootloaderMissingError{
            Arch:         arch,
            ExpectedPath: bootloaderPath,
            Hint:         "Image must include a pre-installed UEFI bootloader (GRUB or systemd-boot). " +
                          "Cocoon does NOT install bootloaders. Build a bootable OCI image with GRUB included.",
        }
    }

    // Check GRUB config exists
    hasGRUBConfig, err := guestfishExists(imagePath, "/boot/grub/grub.cfg")
    if err != nil {
        return err
    }
    if !hasGRUBConfig {
        return fmt.Errorf("bootloader found but /boot/grub/grub.cfg missing; image may not boot correctly")
    }

    // Bootloader exists — optionally regenerate grub.cfg for correct serial/root params
    return updateGRUBConfig(imagePath)
}

// guestfishExists checks if a path exists inside a qcow2 image
func guestfishExists(imagePath, guestPath string) (bool, error) {
    script := fmt.Sprintf(`
    add %s
    run
    mount /dev/sda2 /
    mount /dev/sda1 /boot/efi
    exists %s
    `, imagePath, guestPath)

    cmd := exec.Command("guestfish")
    cmd.Stdin = strings.NewReader(script)

    output, err := cmd.Output()
    if err != nil {
        return false, err
    }

    return strings.TrimSpace(string(output)) == "true", nil
}

// updateGRUBConfig regenerates grub.cfg for correct serial console and root device
// (does NOT install GRUB — only updates config for an already-installed bootloader)
func updateGRUBConfig(imagePath string) error {
    cmd := exec.Command("virt-customize",
        "-a", imagePath,
        "--run-command", "grub-mkconfig -o /boot/grub/grub.cfg")

    return cmd.Run()
}

// BootloaderMissingError provides actionable guidance when bootloader is not found
type BootloaderMissingError struct {
    Arch         string
    ExpectedPath string
    Hint         string
}

func (e *BootloaderMissingError) Error() string {
    return fmt.Sprintf(
        "bootloader not found for %s at %s\n%s",
        e.Arch, e.ExpectedPath, e.Hint,
    )
}
```

**Bootloader Validation Cross-Reference**: This validation is consistent with [Boot Contract § 6](./01-boot-contract.md) and the `ValidateBootability()` rootfs checks in [§ 4.3](#43-rootfs-validation). The key principle: **Cocoon validates, it does not install.**

**Compatibility Scope** (what passes validation):
| Image Type | Bootloader | Passes? | Notes |
|------------|-----------|---------|-------|
| Ubuntu Cloud Image (qcow2) | GRUB pre-installed | Yes | Recommended path |
| Custom OCI with GRUB | GRUB in ESP | Yes | Must be built with bootloader |
| Custom OCI without GRUB | Missing | **No** | Fails with `BootloaderMissingError` |
| Fedora Cloud Image | GRUB pre-installed | Yes | Uses initramfs (supported) |
| Minimal OCI (alpine) | Missing | **No** | Application container, not VM image |

### 5.7 Step 6: Verify Boot Contract

Before caching, verify the image meets the [Boot Contract](01-boot-contract.md):

```go
func VerifyBootContract(imagePath string) error {
    // Detect architecture from image
    arch, err := DetectImageArchitecture(imagePath)
    if err != nil {
        return fmt.Errorf("failed to detect architecture: %w", err)
    }

    // Architecture-specific bootloader path (mounted path in rootfs)
    var bootloaderPath string
    switch arch {
    case "x86_64":
        // This is the mounted path (ESP mounted at /boot/efi)
        bootloaderPath = "/boot/efi/EFI/BOOT/BOOTX64.EFI"
    case "aarch64":
        bootloaderPath = "/boot/efi/EFI/BOOT/BOOTAA64.EFI"
    default:
        return fmt.Errorf("unsupported architecture: %s", arch)
    }

    // MUST checks (mandatory for boot)
    checks := []struct {
        name string
        cmd  string
    }{
        {"kernel", "test -f /boot/vmlinuz-*"},
        // Support both initrd* and initramfs* (Fedora uses initramfs)
        {"initrd", "sh -c 'test -f /boot/initrd* || test -f /boot/initramfs*'"},
        {"init", "test -x /sbin/init"},
        {"grub-config", "test -f /boot/grub/grub.cfg"},
        {"uefi-bootloader", fmt.Sprintf("test -f %s", bootloaderPath)},
    }

    for _, check := range checks {
        cmd := exec.Command("guestfish", "-a", imagePath, "-i", "sh", check.cmd)
        if err := cmd.Run(); err != nil {
            return fmt.Errorf("boot contract violation: %s check failed", check.name)
        }
    }

    // Verify init is systemd (mandatory for Cocoon)
    cmd := exec.Command("guestfish", "-a", imagePath, "-i",
        "sh", "readlink /sbin/init | grep -q systemd")
    if err := cmd.Run(); err != nil {
        return fmt.Errorf("boot contract violation: init system must be systemd")
    }

    // SHOULD check (recommended, not mandatory)
    // cloud-init: CONDITIONAL (warning if missing, but don't fail)
    // - REQUIRED: For metadata server integration
    // - OPTIONAL: VM will boot without it
    cloudInitCmd := exec.Command("guestfish", "-a", imagePath, "-i",
        "sh", "test -x /usr/bin/cloud-init")
    if err := cloudInitCmd.Run(); err != nil {
        log.Warn("cloud-init not found in image - VM will boot but Cocoon metadata server integration disabled")
    }

    return nil
}

// DetectImageArchitecture detects the architecture of a qcow2 image
func DetectImageArchitecture(imagePath string) (string, error) {
    // Method: Check kernel binary architecture via guestfish
    cmd := exec.Command("guestfish", "-a", imagePath, "-i",
        "sh", "file /boot/vmlinuz-* | head -1")

    output, err := cmd.Output()
    if err != nil {
        return "", fmt.Errorf("failed to detect architecture: %w", err)
    }

    outputStr := string(output)
    if strings.Contains(outputStr, "x86-64") || strings.Contains(outputStr, "x86_64") {
        return "x86_64", nil
    }
    if strings.Contains(outputStr, "ARM aarch64") || strings.Contains(outputStr, "arm64") {
        return "aarch64", nil
    }

    return "", fmt.Errorf("unknown architecture from kernel: %s", outputStr)
}
```

### 5.8 Complete Conversion Function

```go
func ConvertOCIToQcow2(
    rootfsPath string,
    outputPath string,
    size string,
) error {
    // 1. Create empty qcow2
    if err := CreateEmptyQcow2(outputPath, size); err != nil {
        return fmt.Errorf("failed to create qcow2: %w", err)
    }

    // 2. Create partitions and format
    if err := FormatImageWithPartitions(outputPath); err != nil {
        return fmt.Errorf("failed to create partitions: %w", err)
    }

    // 3. Copy rootfs
    if err := CopyRootfs(outputPath, rootfsPath); err != nil {
        return fmt.Errorf("failed to copy rootfs: %w", err)
    }

    // 4. Validate bootloader (fail-fast if missing; update grub.cfg if present)
    if err := ValidateBootloader(outputPath); err != nil {
        return fmt.Errorf("bootloader validation failed: %w", err)
    }

    // 5. Verify boot contract
    if err := VerifyBootContract(outputPath); err != nil {
        return fmt.Errorf("boot contract verification failed: %w", err)
    }

    return nil
}
```

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
[05-storage-management.md § Image Checksum Identity](./05-storage-management.md#image-checksum-identity-normative),
which is the single source of truth for filesystem paths.

**Why not image tag?** Tags are mutable (`ubuntu:22.04` can point to different content over time).

**Why manifest?** Manifest is immutable and content-addressable.

#### For OCI Images (Primary Path)

```
checksum = SHA256(
    manifest.config.digest + "\n" +
    sort(manifest.layers[*].digest).join("\n") + "\n" +
    platform_os + "/" + platform_arch       // e.g., "linux/amd64"
)
```

- Layer digests are **sorted lexicographically** before joining (ensures stability
  regardless of manifest layer ordering).
- The platform string (`linux/amd64`) is appended to distinguish identical layer
  sets built for different architectures.
- The full 64-character hex digest is computed; the first **12 hex characters**
  (48 bits) are used for filenames and keys. The full digest is stored in
  `references.json` metadata for collision verification.
- Cache filename: `{checksum_12}_{arch}.qcow2` (e.g., `a1b2c3d4e5f6_amd64.qcow2`)

**Multi-arch manifest lists**: When skopeo returns a manifest list
(`mediaType: application/vnd.oci.image.index.v1+json`), resolve to the
platform-specific manifest FIRST using `runtime.GOARCH`, then compute the
checksum above on the resolved single-platform manifest.

#### For Cloud Images (qcow2/img files)

```
checksum = SHA256(file_content)[:12]
arch     = detect from image metadata, or default to runtime.GOARCH
```

#### For URL-Based Images

```
checksum = SHA256(downloaded_file_content)[:12]
arch     = detect or default to runtime.GOARCH
```

#### Implementation

```go
import (
    "crypto/sha256"
    "encoding/hex"
    "encoding/json"
    "fmt"
    "os/exec"
    "runtime"
    "sort"
    "strings"
)

// ImageIdentity holds the content-addressed identity of a base image.
type ImageIdentity struct {
    Checksum string // 12-char hex prefix of SHA-256
    FullHash string // Full 64-char hex SHA-256 (for collision checks)
    Arch     string // "amd64", "arm64", etc.
}

// CalculateOCIIdentity computes the checksum identity for an OCI image.
// See 05-storage-management.md § "Image Checksum Identity" for the contract.
func CalculateOCIIdentity(image string) (*ImageIdentity, error) {
    // Use skopeo to get raw manifest
    cmd := exec.Command("skopeo", "inspect", "--raw",
        fmt.Sprintf("docker://%s", image))
    output, err := cmd.Output()
    if err != nil {
        return nil, fmt.Errorf("failed to fetch manifest: %w", err)
    }

    // Detect manifest list vs single manifest
    var probe struct {
        MediaType string `json:"mediaType"`
    }
    json.Unmarshal(output, &probe)

    if strings.Contains(probe.MediaType, "image.index") ||
        strings.Contains(probe.MediaType, "manifest.list") {
        // Multi-arch manifest list — resolve to platform-specific manifest
        return resolveMultiArchIdentity(image)
    }

    return calculateSingleManifestIdentity(output, goarchToOCI(runtime.GOARCH))
}

func calculateSingleManifestIdentity(rawManifest []byte, arch string) (*ImageIdentity, error) {
    var manifest struct {
        Config struct {
            Digest string `json:"digest"`
        } `json:"config"`
        Layers []struct {
            Digest string `json:"digest"`
        } `json:"layers"`
    }

    if err := json.Unmarshal(rawManifest, &manifest); err != nil {
        return nil, fmt.Errorf("failed to parse manifest: %w", err)
    }

    // Sort layer digests for stability
    layerDigests := make([]string, len(manifest.Layers))
    for i, l := range manifest.Layers {
        layerDigests[i] = l.Digest
    }
    sort.Strings(layerDigests)

    // Build canonical representation:
    //   config_digest + "\n" + sorted_layers.join("\n") + "\n" + platform
    var sb strings.Builder
    sb.WriteString(manifest.Config.Digest)
    sb.WriteString("\n")
    sb.WriteString(strings.Join(layerDigests, "\n"))
    sb.WriteString("\n")
    sb.WriteString("linux/" + arch) // e.g., "linux/amd64"

    // SHA-256
    hash := sha256.Sum256([]byte(sb.String()))
    fullHex := hex.EncodeToString(hash[:])

    return &ImageIdentity{
        Checksum: fullHex[:12],
        FullHash: fullHex,
        Arch:     arch,
    }, nil
}

func resolveMultiArchIdentity(image string) (*ImageIdentity, error) {
    // Fetch platform-specific manifest using --override-arch
    arch := goarchToOCI(runtime.GOARCH)
    cmd := exec.Command("skopeo", "inspect", "--raw",
        "--override-arch", arch,
        fmt.Sprintf("docker://%s", image))
    output, err := cmd.Output()
    if err != nil {
        return nil, fmt.Errorf("failed to resolve %s manifest: %w", arch, err)
    }
    return calculateSingleManifestIdentity(output, arch)
}

// goarchToOCI maps Go's GOARCH to OCI platform architecture strings.
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

// CacheFilename returns the content-addressed filename: {checksum}_{arch}.qcow2
func (id *ImageIdentity) CacheFilename() string {
    return fmt.Sprintf("%s_%s.qcow2", id.Checksum, id.Arch)
}

// CacheKey returns the content-addressed key: {checksum}_{arch}
// Used as the key in references.json and lock filenames.
func (id *ImageIdentity) CacheKey() string {
    return fmt.Sprintf("%s_%s", id.Checksum, id.Arch)
}
```

### 6.3 Cache Lookup

Cache filenames use the content-addressed pattern `{checksum}_{arch}.qcow2`
(see [05-storage-management.md § Canonical Filesystem Layout](./05-storage-management.md#canonical-filesystem-layout-normative)).

```go
type ImageCache struct {
    cacheDir string // e.g., /var/lib/cocoon/cache/images
}

func NewImageCache(cacheDir string) *ImageCache {
    os.MkdirAll(cacheDir, 0755)
    return &ImageCache{cacheDir: cacheDir}
}

// GetByIdentity checks the cache for a previously converted image.
// Returns the path to the cached qcow2 file, or os.ErrNotExist.
func (c *ImageCache) GetByIdentity(id *ImageIdentity) (string, error) {
    cachedPath := filepath.Join(c.cacheDir, id.CacheFilename())

    if _, err := os.Stat(cachedPath); err == nil {
        // Cache hit
        return cachedPath, nil
    }

    // Cache miss
    return "", os.ErrNotExist
}

// PutByIdentity stores a converted qcow2 image in the cache.
func (c *ImageCache) PutByIdentity(id *ImageIdentity, qcow2Path string) (string, error) {
    cachedPath := filepath.Join(c.cacheDir, id.CacheFilename())

    // Copy qcow2 to cache (use reflink if available)
    cmd := exec.Command("cp", "--reflink=auto", qcow2Path, cachedPath)
    if err := cmd.Run(); err != nil {
        return "", err
    }
    return cachedPath, nil
}
```

### 6.4 Complete Pipeline with Caching

```go
// PrepareBaseImage returns the cached qcow2 path and its content-addressed identity.
func PrepareBaseImage(image string, cache *ImageCache) (string, *ImageIdentity, error) {
    // 1. Calculate content-addressed identity (checksum + arch)
    identity, err := CalculateOCIIdentity(image)
    if err != nil {
        return "", nil, fmt.Errorf("failed to calculate image identity: %w", err)
    }

    // 2. Check cache using identity key ({checksum}_{arch}.qcow2)
    cachedPath, err := cache.GetByIdentity(identity)
    if err == nil {
        log.Printf("Cache hit: %s -> %s (key: %s)", image, cachedPath, identity.CacheKey())
        return cachedPath, identity, nil
    }

    log.Printf("Cache miss: %s (key: %s), converting from OCI...", image, identity.CacheKey())

    // 3. Pull OCI image
    buildah, _ := NewBuildahClient()
    if err := buildah.Pull(image); err != nil {
        return "", nil, fmt.Errorf("failed to pull image: %w", err)
    }

    // 4. Extract rootfs
    mounted, err := buildah.ExtractImage(image)
    if err != nil {
        return "", nil, fmt.Errorf("failed to extract image: %w", err)
    }
    defer mounted.Cleanup()

    // 5. Validate bootability
    if err := mounted.ValidateBootability(); err != nil {
        return "", nil, fmt.Errorf("image is not bootable: %w", err)
    }

    // 6. Convert to qcow2 in temp directory
    tempPath := filepath.Join(os.TempDir(), fmt.Sprintf("cocoon-%s.qcow2", uuid.New().String()))
    if err := ConvertOCIToQcow2(mounted.Path(), tempPath, "10G"); err != nil {
        os.Remove(tempPath)
        return "", nil, fmt.Errorf("conversion failed: %w", err)
    }

    // 7. Store in cache under content-addressed filename
    cachedPath, err = cache.PutByIdentity(identity, tempPath)
    if err != nil {
        os.Remove(tempPath)
        return "", nil, fmt.Errorf("failed to cache image: %w", err)
    }

    // 8. Cleanup temp file
    os.Remove(tempPath)

    return cachedPath, identity, nil
}
```

---

## 7. Error Handling

### 7.1 Error Classification

| Error Type | Recovery Strategy | User Action Required |
|------------|-------------------|---------------------|
| **Network Error** | Retry with backoff | Check network connectivity |
| **Image Not Found** | Fail immediately | Verify image name and registry |
| **Authentication Failed** | Fail with clear message | Provide credentials |
| **Not Bootable** | Fail with validation report | Use bootable base image |
| **Disk Full** | Cleanup and retry | Free disk space |
| **Tool Missing** | Fail with install instructions | Install dependencies |
| **Permission Denied** | Check rootless vs rootful | Fix permissions or use rootful |

### 7.2 Detailed Error Messages

```go
type ConversionError struct {
    Stage   string // "pull", "extract", "convert", "bootloader", "verify"
    Image   string
    Cause   error
    Details string
}

func (e *ConversionError) Error() string {
    return fmt.Sprintf(
        "OCI conversion failed at stage '%s' for image '%s': %v\nDetails: %s",
        e.Stage, e.Image, e.Cause, e.Details,
    )
}

func (e *ConversionError) UserMessage() string {
    switch e.Stage {
    case "pull":
        return fmt.Sprintf(
            "Failed to pull image '%s'.\n"+
            "Possible causes:\n"+
            "  - Image does not exist in registry\n"+
            "  - Network connectivity issues\n"+
            "  - Authentication required for private image\n"+
            "Error: %v", e.Image, e.Cause,
        )

    case "bootable-check":
        return fmt.Sprintf(
            "Image '%s' is not bootable.\n"+
            "Required components:\n"+
            "  - Linux kernel (/boot/vmlinuz-*)\n"+
            "  - Initrd (/boot/initrd.img-*)\n"+
            "  - Init system (/sbin/init)\n"+
            "  - UEFI bootloader (/boot/efi/EFI/BOOT/BOOTX64.EFI)\n"+
            "  - GRUB config (/boot/grub/grub.cfg)\n"+
            "Use a base image with these components installed.\n"+
            "Error: %v", e.Image, e.Cause,
        )

    default:
        return e.Error()
    }
}
```

### 7.3 Cleanup on Failure

```go
func ConvertWithCleanup(image, outputPath string) (err error) {
    var mounted *MountedContainer
    var tempFiles []string

    // Setup cleanup handler
    defer func() {
        // Cleanup mounted container
        if mounted != nil {
            mounted.Cleanup()
        }

        // Cleanup temp files
        for _, path := range tempFiles {
            os.Remove(path)
        }

        // On error, remove partial output
        if err != nil && outputPath != "" {
            os.Remove(outputPath)
        }
    }()

    // ... conversion logic

    return nil
}
```

### 7.4 Retry Logic

```go
func PullWithRetry(image string, maxRetries int) error {
    var lastErr error

    for attempt := 1; attempt <= maxRetries; attempt++ {
        buildah := NewBuildahClient()
        err := buildah.Pull(image)

        if err == nil {
            return nil // Success
        }

        lastErr = err

        // Check if error is retryable
        if !isRetryableError(err) {
            return err // Fail immediately for non-retryable errors
        }

        if attempt < maxRetries {
            backoff := time.Duration(attempt) * time.Second
            log.Printf("Pull failed (attempt %d/%d), retrying in %v: %v",
                attempt, maxRetries, backoff, err)
            time.Sleep(backoff)
        }
    }

    return fmt.Errorf("pull failed after %d attempts: %w", maxRetries, lastErr)
}

func isRetryableError(err error) bool {
    // Network errors are retryable
    if strings.Contains(err.Error(), "connection refused") ||
       strings.Contains(err.Error(), "timeout") ||
       strings.Contains(err.Error(), "temporary failure") {
        return true
    }

    // Authentication and not-found errors are not retryable
    return false
}
```

---

## 8. Rootless vs Rootful Considerations

### 8.1 Rootless Mode (Preferred for VM Operations, Not Conversion)

**Goal**: Run VM operations without root privileges.

**What Works Rootless**:
- ✅ VM lifecycle management (start/stop/delete)
- ✅ Cloud Hypervisor operation (with KVM access)
- ✅ Buildah image pulling and mounting
- ✅ qcow2 overlay creation (qemu-img)

**What Requires Root**:
- ❌ **libguestfs operations** (virt-format, virt-copy-in, guestfish)
  - Partitioning and formatting disk images
  - Copying files into disk images
  - Installing bootloaders
- ❌ **OCI to qcow2 conversion pipeline** (depends on libguestfs)

**Implications for Rootless Deployment**:
- **OCI image conversion is NOT available** in rootless mode
- **Workaround 1**: Use cloud images (qcow2 format) directly - recommended
- **Workaround 2**: Pre-convert OCI images in rootful environment, deploy qcow2 files
- **Workaround 3**: Use hybrid mode (privileged helper for conversion only)

**Requirements for Rootless VM Operations**:
- User namespaces enabled (`/proc/sys/kernel/unprivileged_userns_clone = 1`)
- User in `kvm` group for /dev/kvm access
- Buildah configured for rootless
- fuse-overlayfs installed

### 8.2 Rootful Mode

**When needed**:
- libguestfs operations fail in rootless mode
- Need to access system-wide image caches
- Performance-critical scenarios (overlayfs faster than fuse-overlayfs)

**Setup**:

```go
func (b *BuildahClient) EnableRootful() *BuildahClient {
    b.rootful = true
    return b
}

func (b *BuildahClient) run(args ...string) (string, error) {
    var cmd *exec.Cmd

    if b.rootful {
        // Run with sudo
        allArgs := append([]string{b.executable}, args...)
        cmd = exec.Command("sudo", allArgs...)
    } else {
        cmd = exec.Command(b.executable, args...)
    }

    // ... execute command
}
```

### 8.3 libguestfs Rootless Workaround

libguestfs can run rootless with `--backend=direct`:

```go
func GuestfishRootless(imagePath, script string) error {
    cmd := exec.Command("guestfish",
        "--backend=direct",  // Use direct backend (no libvirt)
        "-a", imagePath)

    cmd.Stdin = strings.NewReader(script)

    return cmd.Run()
}
```

**Note**: Direct backend is slower but works without root.

### 8.4 Automatic Mode Selection

```go
func NewConverter() *Converter {
    c := &Converter{}

    // Detect if running as root
    if os.Geteuid() == 0 {
        c.mode = "rootful"
        log.Println("Running in rootful mode")
    } else {
        c.mode = "rootless"
        log.Println("Running in rootless mode")

        // Verify rootless prerequisites
        if err := c.checkRootlessSupport(); err != nil {
            log.Fatalf("Rootless mode not supported: %v", err)
        }
    }

    return c
}

func (c *Converter) checkRootlessSupport() error {
    // Check user namespaces
    data, err := os.ReadFile("/proc/sys/kernel/unprivileged_userns_clone")
    if err == nil && strings.TrimSpace(string(data)) != "1" {
        return fmt.Errorf("user namespaces disabled, enable with: sysctl kernel.unprivileged_userns_clone=1")
    }

    // Check fuse-overlayfs
    if _, err := exec.LookPath("fuse-overlayfs"); err != nil {
        return fmt.Errorf("fuse-overlayfs not found, install it for rootless operation")
    }

    return nil
}
```

---

## 9. Implementation Checklist

### 9.1 Phase 1: Core Conversion Pipeline (P0)

- [ ] **Buildah Integration**:
  - [ ] Implement BuildahClient with shell-out pattern
  - [ ] Image pull operation
  - [ ] Container create and mount
  - [ ] Cleanup and error handling

- [ ] **Extraction**:
  - [ ] Mount OCI container filesystem
  - [ ] Validate bootability (kernel, init, bootloader checks)
  - [ ] Cleanup mounted containers

- [ ] **Conversion**:
  - [ ] Create empty qcow2 images
  - [ ] Create GPT partition table (ESP + root)
  - [ ] Format partitions (FAT32 ESP, ext4 root)
  - [ ] Copy rootfs to qcow2 image
  - [ ] Validate UEFI bootloader (fail-fast if missing)
  - [ ] Verify boot contract compliance

- [ ] **Caching**:
  - [ ] Calculate OCI manifest checksums
  - [ ] Implement cache lookup and storage
  - [ ] Deduplicate identical images

- [ ] **Error Handling**:
  - [ ] Network error retry with backoff
  - [ ] Detailed error messages for users
  - [ ] Automatic cleanup on failure

### 9.2 Phase 2: Production Hardening (P1)

- [ ] **Authentication**:
  - [ ] Private registry login support
  - [ ] Credential management
  - [ ] Token refresh

- [ ] **Progress Tracking**:
  - [ ] Pull progress reporting
  - [ ] Conversion progress updates
  - [ ] ETA calculation

- [ ] **Optimization**:
  - [ ] Parallel image pulls
  - [ ] Compressed qcow2 images
  - [ ] Copy-on-write optimizations

- [ ] **Testing**:
  - [ ] Unit tests for each stage
  - [ ] Integration tests with real images
  - [ ] Rootless mode tests

### 9.3 Phase 3: Advanced Features (P2)

- [ ] **Multi-Architecture**:
  - [ ] ARM64 support
  - [ ] Architecture detection and selection

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

## 10. Verified Images (CI Reference)

Phase 1 requires at least one **pinned reference image** per source type for full-pipeline CI verification (conversion → boot detection → lifecycle). These images have fixed digests and known-good checksums, ensuring deterministic CI runs.

### 10.1 Reference Cloud Image (qcow2)

**Ubuntu 22.04 Cloud Image** — primary CI image for boot + lifecycle tests:

| Field | Value |
|-------|-------|
| **URL** | `https://cloud-images.ubuntu.com/releases/22.04/release/ubuntu-22.04-server-cloudimg-amd64.img` |
| **Pinned Release** | `20240126` (pin to a specific date release for reproducibility) |
| **Pinned URL** | `https://cloud-images.ubuntu.com/releases/22.04/release-20240126/ubuntu-22.04-server-cloudimg-amd64.img` |
| **SHA256** | Pin in `test/fixtures/verified-images.sha256` (update on deliberate image bump only) |
| **Format** | qcow2 (direct use, no conversion) |
| **Boot Mode** | PVH (primary), UEFI (fallback) |
| **cloud-init** | Pre-installed, NoCloud-Net datasource |

**CI Usage**:
```bash
# Download and verify (cached in CI, re-downloaded on checksum mismatch)
wget -q -O test-image.img "$PINNED_URL"
sha256sum -c test/fixtures/verified-images.sha256

# Full lifecycle pipeline
cocoon create test-image.img --name ci-boot-test --cpus 1 --memory 1G --disk-size 5G
cocoon start ci-boot-test --boot-timeout 120s
cocoon logs ci-boot-test --tail 20  # Verify boot markers
cocoon inspect ci-boot-test         # Verify state == RUNNING
cocoon stop ci-boot-test
cocoon delete ci-boot-test
```

### 10.2 Reference Bootable OCI Image

**Purpose**: Validates the complete OCI conversion pipeline (pull → extract → validate → convert → boot).

| Field | Value |
|-------|-------|
| **Registry** | `ghcr.io/CMGS/cocoon-test-images/ubuntu-bootable` |
| **Tag** | `22.04` |
| **Pinned Digest** | Pin as `ghcr.io/CMGS/cocoon-test-images/ubuntu-bootable@sha256:<digest>` in CI config |
| **Contents** | Ubuntu 22.04 + kernel + GRUB + systemd + cloud-init |
| **Architecture** | `linux/amd64` |

**CI Usage**:
```bash
# Pull by digest (immutable, deterministic)
cocoon image pull "ghcr.io/CMGS/cocoon-test-images/ubuntu-bootable@sha256:${PINNED_DIGEST}"

# Verify bootability
cocoon image verify "ghcr.io/CMGS/cocoon-test-images/ubuntu-bootable@sha256:${PINNED_DIGEST}"

# Full conversion + boot + lifecycle pipeline
cocoon create "ghcr.io/CMGS/cocoon-test-images/ubuntu-bootable@sha256:${PINNED_DIGEST}" \
  --name ci-oci-test --cpus 1 --memory 1G --disk-size 5G
cocoon start ci-oci-test --boot-timeout 180s
cocoon logs ci-oci-test --tail 20     # Verify systemd + cloud-init markers
cocoon inspect ci-oci-test            # Verify state == RUNNING
cocoon stop ci-oci-test
cocoon delete ci-oci-test
```

### 10.3 CI Verification Matrix

The following pipeline stages MUST pass for every PR:

| Stage | Cloud Image (qcow2) | Bootable OCI Image |
|-------|---------------------|-------------------|
| **Image fetch** | Download + SHA256 verify | Pull by digest |
| **Bootability validation** | `cocoon image verify` | `cocoon image verify` (before conversion) |
| **OCI→qcow2 conversion** | N/A (already qcow2) | Buildah extract → libguestfs convert |
| **PVH boot** | Boot with `hypervisor-fw` | Boot with `hypervisor-fw` |
| **Boot detection** | Serial log → systemd markers | Serial log → systemd + cloud-init markers |
| **Lifecycle** | create → start → inspect → stop → delete | create → start → inspect → stop → delete |
| **Crash recovery** | kill -9 CH → `cocoon doctor --reconcile` | kill -9 CH → `cocoon doctor --reconcile` |
| **GC** | Delete VM → `cocoon gc --dry-run` | Delete VM → `cocoon gc --dry-run` |

### 10.4 Maintaining Verified Images

**When to bump**:
- Kernel CVE fix in upstream cloud image
- Cloud-init version incompatibility discovered
- New distro release needed for coverage

**How to bump**:
1. Update URL/digest in `test/fixtures/verified-images.sha256` (or CI config)
2. Run full CI pipeline manually against new image
3. Commit with message: `ci: bump verified image to <new-version>`
4. **Never** use floating tags (`:latest`, `:22.04`) in CI — always pin to digest or dated release

---

## 11. References

### 11.1 Related Documents

- [01-boot-contract.md](01-boot-contract.md) - Boot requirements and VM lifecycle
- [05-storage-management.md](05-storage-management.md) - COW storage, garbage collection, and **Image Checksum Identity** (normative)
- [06-concurrency.md](06-concurrency.md) - Conversion lock keys use the same `{checksum}_{arch}` identity

### 11.2 External Tools

| Tool | Purpose | Documentation |
|------|---------|---------------|
| Buildah | OCI image operations | https://buildah.io/ |
| qemu-img | qcow2 image creation | https://www.qemu.org/docs/master/tools/qemu-img.html |
| libguestfs | Disk image manipulation | https://libguestfs.org/ |
| skopeo | OCI manifest inspection | https://github.com/containers/skopeo |

### 11.3 Installation

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
package main

import (
    "fmt"
    "log"
)

func main() {
    // 1. Setup
    cache := NewImageCache("/var/lib/cocoon/cache/images")

    // 2. Prepare base image (with caching)
    image := "myorg/ubuntu-bootable:22.04"
    basePath, identity, err := PrepareBaseImage(image, cache)
    if err != nil {
        log.Fatalf("Failed to prepare image: %v", err)
    }

    fmt.Printf("Base image ready: %s (key: %s)\n", basePath, identity.CacheKey())

    // 3. Create VM overlay disk (covered in 05-storage-management.md)
    // overlayPath := createOverlay(basePath, "vm-001")

    // 4. Register reference (covered in 05-storage-management.md)
    // refCounter.AddReference(identity.Checksum, identity.Arch, "vm-001")
}
```

---

**End of OCI Conversion Pipeline Documentation v1.0**

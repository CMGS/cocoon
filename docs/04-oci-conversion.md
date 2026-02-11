# OCI to qcow2 Conversion Pipeline

**Version**: 1.0
**Status**: Draft
**Priority**: P0 - Required for core functionality

## Executive Summary

This document specifies the pipeline for converting OCI container images into bootable qcow2 disk images for Cloud Hypervisor VMs. The conversion process must produce images that satisfy the [Boot Contract](01-boot-contract.md) while maintaining efficiency through caching and deduplication.

**Key Requirements**:
1. Pull OCI images from registries using Buildah
2. Extract container rootfs to disk
3. Convert rootfs to qcow2 format with proper partitioning
4. Make the image bootable (bootloader, kernel, init)
5. Cache images based on content checksums
6. Handle rootless and rootful operation modes
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
- ✅ **Validate**: Check if image has required components (kernel, bootloader, cloud-init)
- ✅ **Configure**: Modify GRUB config, inject cloud-init datasource settings
- ❌ **Install**: Does NOT install missing packages (kernel, GRUB, cloud-init)

**User's Role** (Image Provider):
- Must provide images with kernel, bootloader, and cloud-init **pre-installed**
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
| **Bootloader Installer** | Make image bootable | virt-customize, guestfish |
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
    required := []struct {
        path    string
        isFile  bool
        message string
    }{
        {"/boot/vmlinuz", false, "kernel not found (no /boot/vmlinuz*)"},
        {"/boot/initrd", false, "initrd/initramfs not found (no /boot/initrd* or /boot/initramfs*)"},
        {"/sbin/init", true, "init system not found (/sbin/init missing)"},
        {"/usr/bin/cloud-init", true, "cloud-init not installed (required for initialization)"},
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

    // Verify init is systemd (not sysvinit or other)
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
    var bootloaderPath string
    switch arch {
    case "x86_64":
        bootloaderPath = filepath.Join(m.mountPoint, "/boot/efi/EFI/BOOT/BOOTX64.EFI")
    case "aarch64":
        bootloaderPath = filepath.Join(m.mountPoint, "/boot/efi/EFI/BOOT/BOOTAA64.EFI")
    default:
        return fmt.Errorf("unsupported architecture: %s", arch)
    }

    if _, err := os.Stat(bootloaderPath); os.IsNotExist(err) {
        return fmt.Errorf("bootloader not found for %s: %s", arch, bootloaderPath)
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
5. Install/verify UEFI bootloader
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

### 5.6 Step 5: Make Bootable

**Strategy**: Most OCI images with bootloaders already have them installed. We just need to verify.

```go
func VerifyAndFixBootloader(imagePath string) error {
    // Check if GRUB is already installed
    hasGRUB, err := hasGRUBInstalled(imagePath)
    if err != nil {
        return err
    }

    if hasGRUB {
        // Bootloader exists, just update grub.cfg
        return updateGRUBConfig(imagePath)
    }

    // No bootloader found - install it
    return installGRUB(imagePath)
}

func hasGRUBInstalled(imagePath string) (bool, error) {
    script := `
    add %s
    run
    mount /dev/sda2 /
    mount /dev/sda1 /boot/efi

    # Check for UEFI bootloader
    exists /boot/efi/EFI/BOOT/BOOTX64.EFI
    `

    cmd := exec.Command("guestfish", "-a", imagePath)
    cmd.Stdin = strings.NewReader(fmt.Sprintf(script, imagePath))

    output, err := cmd.Output()
    if err != nil {
        return false, err
    }

    return strings.TrimSpace(string(output)) == "true", nil
}

func installGRUB(imagePath string) error {
    // Use virt-customize to install and configure GRUB
    cmd := exec.Command("virt-customize",
        "-a", imagePath,
        "--run-command", "grub-install --target=x86_64-efi --efi-directory=/boot/efi --boot-directory=/boot --removable",
        "--run-command", "update-grub")

    return cmd.Run()
}

func updateGRUBConfig(imagePath string) error {
    // Regenerate grub.cfg to ensure correct boot parameters
    cmd := exec.Command("virt-customize",
        "-a", imagePath,
        "--run-command", "update-grub",
        "--run-command", "grub-mkconfig -o /boot/grub/grub.cfg")

    return cmd.Run()
}
```

### 5.7 Step 6: Verify Boot Contract

Before caching, verify the image meets the [Boot Contract](01-boot-contract.md):

```go
func VerifyBootContract(imagePath string) error {
    // Detect architecture from image
    arch, err := DetectImageArchitecture(imagePath)
    if err != nil {
        return fmt.Errorf("failed to detect architecture: %w", err)
    }

    // Architecture-specific bootloader path
    var bootloaderPath string
    switch arch {
    case "x86_64":
        bootloaderPath = "/boot/efi/EFI/BOOT/BOOTX64.EFI"
    case "aarch64":
        bootloaderPath = "/boot/efi/EFI/BOOT/BOOTAA64.EFI"
    default:
        return fmt.Errorf("unsupported architecture: %s", arch)
    }

    checks := []struct {
        name string
        cmd  string
    }{
        {"kernel", "test -f /boot/vmlinuz-*"},
        // Check for either initrd or initramfs (Fedora uses initramfs-*)
        {"initrd", "sh -c 'test -f /boot/initrd* || test -f /boot/initramfs*'"},
        {"init", "test -x /sbin/init"},
        {"cloud-init", "test -x /usr/bin/cloud-init"},
        {"grub-config", "test -f /boot/grub/grub.cfg"},
        {"uefi-bootloader", fmt.Sprintf("test -f %s", bootloaderPath)},
    }

    for _, check := range checks {
        cmd := exec.Command("guestfish", "-a", imagePath, "-i", "sh", check.cmd)
        if err := cmd.Run(); err != nil {
            return fmt.Errorf("boot contract violation: %s check failed", check.name)
        }
    }

    // Verify init is systemd (not sysvinit)
    cmd := exec.Command("guestfish", "-a", imagePath, "-i",
        "sh", "readlink /sbin/init | grep -q systemd")
    if err := cmd.Run(); err != nil {
        return fmt.Errorf("boot contract violation: init system must be systemd")
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

    // 4. Make bootable
    if err := VerifyAndFixBootloader(outputPath); err != nil {
        return fmt.Errorf("failed to install bootloader: %w", err)
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

### 6.2 Checksum Calculation Strategy

**Approach**: Calculate checksum from OCI manifest (config digest + layer digests).

**Why not image tag?** Tags are mutable (`ubuntu:22.04` can point to different content over time).

**Why manifest?** Manifest is immutable and content-addressable.

```go
func CalculateOCIChecksum(image string) (string, error) {
    // Use skopeo to get raw manifest
    cmd := exec.Command("skopeo", "inspect", "--raw", fmt.Sprintf("docker://%s", image))
    output, err := cmd.Output()
    if err != nil {
        return "", fmt.Errorf("failed to fetch manifest: %w", err)
    }

    var manifest struct {
        Config struct {
            Digest string `json:"digest"`
        } `json:"config"`
        Layers []struct {
            Digest string `json:"digest"`
        } `json:"layers"`
    }

    if err := json.Unmarshal(output, &manifest); err != nil {
        return "", fmt.Errorf("failed to parse manifest: %w", err)
    }

    // Create stable representation
    var sb strings.Builder
    sb.WriteString(manifest.Config.Digest)
    for _, layer := range manifest.Layers {
        sb.WriteString(layer.Digest)
    }

    // Calculate SHA256
    hash := sha256.Sum256([]byte(sb.String()))
    return hex.EncodeToString(hash[:]), nil
}
```

### 6.3 Cache Lookup

```go
type ImageCache struct {
    cacheDir string
}

func NewImageCache(cacheDir string) *ImageCache {
    os.MkdirAll(cacheDir, 0755)
    return &ImageCache{cacheDir: cacheDir}
}

func (c *ImageCache) Get(image string) (string, error) {
    checksum, err := CalculateOCIChecksum(image)
    if err != nil {
        return "", err
    }

    cachedPath := filepath.Join(c.cacheDir, fmt.Sprintf("%s.qcow2", checksum))

    if _, err := os.Stat(cachedPath); err == nil {
        // Cache hit
        return cachedPath, nil
    }

    // Cache miss
    return "", os.ErrNotExist
}

func (c *ImageCache) Put(image string, qcow2Path string) error {
    checksum, err := CalculateOCIChecksum(image)
    if err != nil {
        return err
    }

    cachedPath := filepath.Join(c.cacheDir, fmt.Sprintf("%s.qcow2", checksum))

    // Copy qcow2 to cache (use reflink if available)
    cmd := exec.Command("cp", "--reflink=auto", qcow2Path, cachedPath)
    return cmd.Run()
}
```

### 6.4 Complete Pipeline with Caching

```go
func PrepareBaseImage(image string, cache *ImageCache) (string, error) {
    // 1. Check cache
    cachedPath, err := cache.Get(image)
    if err == nil {
        log.Printf("Cache hit: %s -> %s", image, cachedPath)
        return cachedPath, nil
    }

    log.Printf("Cache miss: %s, converting from OCI...", image)

    // 2. Pull OCI image
    buildah := NewBuildahClient()
    if err := buildah.Pull(image); err != nil {
        return "", fmt.Errorf("failed to pull image: %w", err)
    }

    // 3. Extract rootfs
    mounted, err := buildah.ExtractImage(image)
    if err != nil {
        return "", fmt.Errorf("failed to extract image: %w", err)
    }
    defer mounted.Cleanup()

    // 4. Validate bootability
    if err := mounted.ValidateBootability(); err != nil {
        return "", fmt.Errorf("image is not bootable: %w", err)
    }

    // 5. Convert to qcow2
    tempPath := filepath.Join(os.TempDir(), fmt.Sprintf("cocoon-%s.qcow2", uuid.New().String()))
    if err := ConvertOCIToQcow2(mounted.Path(), tempPath, "10G"); err != nil {
        os.Remove(tempPath)
        return "", fmt.Errorf("conversion failed: %w", err)
    }

    // 6. Store in cache
    if err := cache.Put(image, tempPath); err != nil {
        os.Remove(tempPath)
        return "", fmt.Errorf("failed to cache image: %w", err)
    }

    // 7. Get cached path
    cachedPath, _ = cache.Get(image)

    // 8. Cleanup temp file
    os.Remove(tempPath)

    return cachedPath, nil
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

### 8.1 Rootless Mode (Preferred)

**Goal**: Run entire pipeline without root privileges.

**Advantages**:
- ✅ Better security (no privilege escalation)
- ✅ Multi-tenant friendly
- ✅ No sudo required

**Requirements**:
- User namespaces enabled (`/proc/sys/kernel/unprivileged_userns_clone = 1`)
- Buildah configured for rootless
- fuse-overlayfs installed

**Limitations**:
- Some libguestfs operations require fakeroot or root (workaround: use `--backend=direct`)
- Cannot access system-wide OCI caches

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
  - [ ] Verify/install UEFI bootloader
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

## 10. References

### 10.1 Related Documents

- [01-boot-contract.md](01-boot-contract.md) - Boot requirements and VM lifecycle
- [05-storage-management.md](05-storage-management.md) - COW storage and garbage collection

### 10.2 External Tools

| Tool | Purpose | Documentation |
|------|---------|---------------|
| Buildah | OCI image operations | https://buildah.io/ |
| qemu-img | qcow2 image creation | https://www.qemu.org/docs/master/tools/qemu-img.html |
| libguestfs | Disk image manipulation | https://libguestfs.org/ |
| skopeo | OCI manifest inspection | https://github.com/containers/skopeo |

### 10.3 Installation

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
    image := "ubuntu:22.04"
    basePath, err := PrepareBaseImage(image, cache)
    if err != nil {
        log.Fatalf("Failed to prepare image: %v", err)
    }

    fmt.Printf("Base image ready: %s\n", basePath)

    // 3. Create VM overlay disk (covered in 05-storage-management.md)
    // overlayPath := createOverlay(basePath, "vm-001")
}
```

---

**End of OCI Conversion Pipeline Documentation v1.0**

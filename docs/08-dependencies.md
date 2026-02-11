# Dependencies and Requirements

## Overview

Cocoon relies on several external tools and libraries to provide VM management with OCI image support. This guide covers all dependencies, their purposes, installation instructions, version requirements, and troubleshooting.

## Dependency Matrix

| Tool | Purpose | Min Version | Rootless Support | Installation Method |
|------|---------|-------------|------------------|---------------------|
| cloud-hypervisor | Virtual Machine Monitor (VMM) | v38.0 | N/A (requires KVM) | Binary or source |
| buildah | OCI image pull/extract | 1.35.0 | ✅ Yes | apt/dnf |
| skopeo | OCI image inspection | 1.14.0 | ✅ Yes | apt/dnf |
| qemu-img | qcow2 operations | 8.0 | ✅ Yes | apt/dnf (qemu-utils) |
| virt-format | Format disk images | libguestfs 1.50 | ❌ Needs root | apt/dnf (libguestfs-tools) |
| virt-copy-in | Copy files into disk | libguestfs 1.50 | ❌ Needs root | apt/dnf (libguestfs-tools) |
| ovmf | UEFI firmware | any stable | N/A | apt/dnf |
| /dev/kvm | KVM device access | kernel 5.6+ | N/A (user in kvm group) | Built-in (kernel module) |

## Core Dependencies

### 1. Cloud Hypervisor

**Purpose**: Production-grade Virtual Machine Monitor built in Rust for running lightweight VMs.

**Minimum Version**: v38.0

**Installation**:

```bash
# Download pre-built binary (recommended)
CH_VERSION="v38.0"
curl -LO https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/${CH_VERSION}/cloud-hypervisor-static

# Verify checksum (optional but recommended)
curl -LO https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/${CH_VERSION}/cloud-hypervisor-static.sha256
sha256sum -c cloud-hypervisor-static.sha256

# Install
chmod +x cloud-hypervisor-static
sudo mv cloud-hypervisor-static /usr/local/bin/cloud-hypervisor

# Verify
cloud-hypervisor --version
```

**From Source** (alternative):

```bash
# Install Rust toolchain
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh
source $HOME/.cargo/env

# Clone and build
git clone https://github.com/cloud-hypervisor/cloud-hypervisor.git
cd cloud-hypervisor
git checkout v38.0
cargo build --release

# Install
sudo cp target/release/cloud-hypervisor /usr/local/bin/
```

### 2. Buildah

**Purpose**: Pulls and extracts OCI container images without requiring a daemon.

**Minimum Version**: 1.35.0

**Rootless Support**: ✅ Yes (requires fuse-overlayfs or kernel overlay support)

**Installation**:

**Ubuntu 22.04/24.04**:
```bash
sudo apt-get update
sudo apt-get install -y buildah fuse-overlayfs
```

**Fedora 39/40**:
```bash
sudo dnf install -y buildah fuse-overlayfs
```

**Verification**:
```bash
buildah version
# Expected: Version: 1.35.0 or higher
```

**Rootless Configuration**:
```bash
# Enable user namespaces
echo "kernel.unprivileged_userns_clone=1" | sudo tee /etc/sysctl.d/99-rootless.conf
sudo sysctl -p /etc/sysctl.d/99-rootless.conf

# Configure subuid/subgid
grep $USER /etc/subuid || echo "$USER:100000:65536" | sudo tee -a /etc/subuid
grep $USER /etc/subgid || echo "$USER:100000:65536" | sudo tee -a /etc/subgid
```

### 3. Skopeo

**Purpose**: Inspects OCI images and manifests to calculate checksums for caching.

**Minimum Version**: 1.14.0

**Rootless Support**: ✅ Yes

**Installation**:

**Ubuntu**:
```bash
sudo apt-get install -y skopeo
```

**Fedora**:
```bash
sudo dnf install -y skopeo
```

**Verification**:
```bash
skopeo --version
# Expected: skopeo version 1.14.0 or higher
```

### 4. QEMU Image Tools (qemu-img)

**Purpose**: Creates, converts, and manages qcow2 disk images.

**Minimum Version**: 8.0

**Rootless Support**: ✅ Yes

**Installation**:

**Ubuntu**:
```bash
sudo apt-get install -y qemu-utils
```

**Fedora**:
```bash
sudo dnf install -y qemu-img
```

**Verification**:
```bash
qemu-img --version
# Expected: qemu-img version 8.0.0 or higher
```

### 5. libguestfs Tools

**Purpose**: Formats disk images and copies files into them.

**Minimum Version**: libguestfs 1.50

**Rootless Support**: ❌ No (requires root or setuid helper)

**Tools Included**:
- `virt-format`: Formats filesystem inside disk images
- `virt-copy-in`: Copies files into disk images

**Installation**:

**Ubuntu**:
```bash
sudo apt-get install -y libguestfs-tools
```

**Fedora**:
```bash
sudo dnf install -y libguestfs-tools
```

**Verification**:
```bash
virt-format --version
virt-copy-in --version
```

### 6. OVMF (UEFI Firmware)

**Purpose**: Provides UEFI firmware for booting VMs with standard Linux distributions.

**Rootless Support**: N/A (system files)

**Installation**:

**Ubuntu**:
```bash
sudo apt-get install -y ovmf
# Firmware installed to: /usr/share/OVMF/
```

**Fedora**:
```bash
sudo dnf install -y edk2-ovmf
# Firmware installed to: /usr/share/edk2/ovmf/
```

**Verification**:
```bash
# Ubuntu
ls -l /usr/share/OVMF/OVMF_CODE.fd

# Fedora
ls -l /usr/share/edk2/ovmf/OVMF_CODE.fd
```

### 7. KVM Device Access

**Purpose**: Provides hardware virtualization support via Linux kernel.

**Minimum Kernel**: 5.6+

**Prerequisites**:
- CPU with Intel VT-x or AMD-V virtualization support
- Virtualization enabled in BIOS/UEFI
- KVM kernel modules loaded

**Verification**:

```bash
# Check CPU virtualization support
# Intel (look for 'vmx'):
grep -E 'vmx' /proc/cpuinfo

# AMD (look for 'svm'):
grep -E 'svm' /proc/cpuinfo

# Check KVM modules
lsmod | grep kvm
# Expected: kvm_intel or kvm_amd, plus kvm

# Check /dev/kvm exists
ls -l /dev/kvm
```

**Setup**:
```bash
# Add user to kvm group
sudo usermod -aG kvm $USER

# Log out and back in for group membership to take effect
# Or run: newgrp kvm
```

## Startup Dependency Detection (cocoon doctor)

Cocoon includes a `doctor` command to verify all dependencies are correctly installed:

### Implementation Concept

```go
package main

type DependencyCheck struct {
    Name           string
    Command        string
    Args           []string
    VersionPattern string
    Required       bool
}

type DependencyStatus struct {
    Name      string
    Found     bool
    Version   string
    Path      string
    Error     string
    InstallCmd string
}

func checkDependencies() []DependencyStatus {
    checks := []DependencyCheck{
        {
            Name:           "cloud-hypervisor",
            Command:        "cloud-hypervisor",
            Args:           []string{"--version"},
            VersionPattern: `v\d+`,
            Required:       true,
        },
        {
            Name:           "buildah",
            Command:        "buildah",
            Args:           []string{"version"},
            VersionPattern: `Version: 1\\.3[5-9]|Version: 1\\.[4-9][0-9]|Version: [2-9]`,
            Required:       true,
        },
        {
            Name:           "skopeo",
            Command:        "skopeo",
            Args:           []string{"--version"},
            VersionPattern: `version 1\\.1[4-9]|version 1\\.[2-9][0-9]|version [2-9]`,
            Required:       true,
        },
        {
            Name:           "qemu-img",
            Command:        "qemu-img",
            Args:           []string{"--version"},
            VersionPattern: `version [8-9]|version [1-9][0-9]`,
            Required:       true,
        },
        {
            Name:           "virt-format",
            Command:        "virt-format",
            Args:           []string{"--version"},
            VersionPattern: `virt-format`,
            Required:       false, // Optional for rootless
        },
        {
            Name:           "virt-copy-in",
            Command:        "virt-copy-in",
            Args:           []string{"--version"},
            VersionPattern: `virt-copy-in`,
            Required:       false,
        },
    }

    results := []DependencyStatus{}
    for _, check := range checks {
        status := runCheck(check)
        results = append(results, status)
    }

    // Check KVM access
    kvmStatus := checkKVMAccess()
    results = append(results, kvmStatus)

    return results
}

func runCheck(check DependencyCheck) DependencyStatus {
    cmd := exec.Command(check.Command, check.Args...)
    output, err := cmd.CombinedOutput()

    status := DependencyStatus{
        Name: check.Name,
    }

    if err != nil {
        status.Found = false
        status.Error = err.Error()
        status.InstallCmd = getInstallCommand(check.Name)
        return status
    }

    status.Found = true
    status.Path, _ = exec.LookPath(check.Command)

    // Extract version using pattern
    re := regexp.MustCompile(check.VersionPattern)
    if match := re.Find(output); match != nil {
        status.Version = string(match)
    }

    return status
}

func checkKVMAccess() DependencyStatus {
    status := DependencyStatus{
        Name: "/dev/kvm",
    }

    _, err := os.Stat("/dev/kvm")
    if err != nil {
        status.Found = false
        status.Error = "KVM device not found"
        status.InstallCmd = "Ensure virtualization is enabled in BIOS and KVM modules are loaded"
        return status
    }

    // Test if readable
    file, err := os.Open("/dev/kvm")
    if err != nil {
        status.Found = false
        status.Error = "Permission denied"
        status.InstallCmd = "sudo usermod -aG kvm $USER && newgrp kvm"
        return status
    }
    defer file.Close()

    status.Found = true
    status.Version = "accessible"
    return status
}

func getInstallCommand(name string) string {
    installCmds := map[string]string{
        "cloud-hypervisor": "curl -LO https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v38.0/cloud-hypervisor-static && chmod +x cloud-hypervisor-static && sudo mv cloud-hypervisor-static /usr/local/bin/cloud-hypervisor",
        "buildah":          "sudo apt-get install buildah (Ubuntu) or sudo dnf install buildah (Fedora)",
        "skopeo":           "sudo apt-get install skopeo (Ubuntu) or sudo dnf install skopeo (Fedora)",
        "qemu-img":         "sudo apt-get install qemu-utils (Ubuntu) or sudo dnf install qemu-img (Fedora)",
        "virt-format":      "sudo apt-get install libguestfs-tools (Ubuntu) or sudo dnf install libguestfs-tools (Fedora)",
        "virt-copy-in":     "sudo apt-get install libguestfs-tools (Ubuntu) or sudo dnf install libguestfs-tools (Fedora)",
    }
    return installCmds[name]
}
```

### Example Output

```
Cocoon Dependency Check
=======================

✅ cloud-hypervisor v38.0 found at /usr/local/bin/cloud-hypervisor
✅ buildah 1.35.0 found at /usr/bin/buildah
✅ skopeo 1.14.0 found at /usr/bin/skopeo
✅ qemu-img 8.2.0 found at /usr/bin/qemu-img
❌ virt-format not found
   → Install: sudo apt-get install libguestfs-tools
   → Note: Not required for rootless mode with manual disk creation
❌ virt-copy-in not found
   → Install: sudo apt-get install libguestfs-tools
   → Note: Not required for rootless mode with manual disk creation
✅ /dev/kvm accessible

Summary: 5/7 required dependencies found
Warning: 2 optional dependencies missing (libguestfs tools)
Status: Ready to run in rootless mode
```

## Permission Models

Cocoon supports three permission models to balance security and functionality.

### Option A: Rootless (Preferred)

**Description**: Run cocoon entirely as a regular user without sudo privileges.

**Requirements**:
- User must be in `kvm` group for /dev/kvm access
- Buildah configured for rootless operation
- Kernel must support unprivileged user namespaces

**Advantages**:
- Better security (least privilege)
- No sudo required for daily operations
- Recommended for multi-tenant environments

**Limitations**:
- Cannot use libguestfs tools (virt-format, virt-copy-in)
- Must use alternative disk creation methods:
  - Pre-built base images
  - Manual qcow2 creation without filesystem formatting
  - External image preparation workflow

**Setup**:

```bash
# 1. Add user to kvm group
sudo usermod -aG kvm $USER
newgrp kvm

# 2. Enable unprivileged user namespaces
echo "kernel.unprivileged_userns_clone=1" | sudo tee /etc/sysctl.d/99-rootless.conf
sudo sysctl -p /etc/sysctl.d/99-rootless.conf

# 3. Configure subuid/subgid for buildah
grep $USER /etc/subuid || echo "$USER:100000:65536" | sudo tee -a /etc/subuid
grep $USER /etc/subgid || echo "$USER:100000:65536" | sudo tee -a /etc/subgid

# 4. Install fuse-overlayfs (for buildah storage)
sudo apt-get install -y fuse-overlayfs  # Ubuntu
# or
sudo dnf install -y fuse-overlayfs      # Fedora

# 5. Configure buildah storage
mkdir -p ~/.config/containers
cat > ~/.config/containers/storage.conf <<EOF
[storage]
driver = "overlay"
graphroot = "\$HOME/.local/share/containers/storage"

[storage.options.overlay]
mount_program = "/usr/bin/fuse-overlayfs"
EOF

# 6. Verify setup
cocoon doctor
```

### Option B: Rootful

**Description**: Run cocoon as root user (not recommended for production).

**Advantages**:
- All tools available (including libguestfs)
- No permission issues
- Simpler setup

**Disadvantages**:
- Security risk (runs with full system privileges)
- Not suitable for multi-tenant environments
- Potential for accidental system damage

**Setup**:

```bash
# Install all dependencies
sudo apt-get install -y cloud-hypervisor buildah skopeo qemu-utils libguestfs-tools ovmf

# Run cocoon as root
sudo cocoon create --image ubuntu:22.04 myvm
```

**Not Recommended**: Use Option C (Hybrid) instead.

### Option C: Hybrid (Recommended for Production)

**Description**: Run main cocoon binary as regular user, but use a privileged helper for operations requiring root.

**Architecture**:
```
cocoon (user) → cocoon-helper (setuid or sudo) → libguestfs tools
```

**Advantages**:
- Best security (minimal privilege escalation)
- All features available
- Audit trail for privileged operations

**Implementation**:

**cocoon-helper** (setuid binary or sudo wrapper):
```bash
#!/bin/bash
# /usr/local/bin/cocoon-helper
# This script runs with elevated privileges (via sudo or setuid)

case "$1" in
    format-disk)
        virt-format --filesystem=ext4 "$2"
        ;;
    copy-into-disk)
        virt-copy-in -a "$2" "$3" /
        ;;
    *)
        echo "Unknown operation: $1"
        exit 1
        ;;
esac
```

**Setup**:

```bash
# 1. Install cocoon-helper
sudo cp cocoon-helper /usr/local/bin/
sudo chmod 755 /usr/local/bin/cocoon-helper

# 2. Configure sudo (option 1: per-command)
cat > /etc/sudoers.d/cocoon-helper <<EOF
# Allow users in 'cocoon' group to run cocoon-helper
%cocoon ALL=(ALL) NOPASSWD: /usr/local/bin/cocoon-helper
EOF

# 3. Add users to cocoon group
sudo groupadd cocoon
sudo usermod -aG cocoon $USER

# 4. Verify
sudo -n /usr/local/bin/cocoon-helper format-disk /tmp/test.qcow2
```

**Alternative: setuid binary** (more secure, requires compilation):

```go
// cocoon-helper.go
package main

import (
    "fmt"
    "os"
    "os/exec"
    "path/filepath"
)

func main() {
    if len(os.Args) < 2 {
        fmt.Fprintln(os.Stderr, "Usage: cocoon-helper <operation> <args>")
        os.Exit(1)
    }

    operation := os.Args[1]

    // Validate caller is in 'cocoon' group
    if !isAuthorized() {
        fmt.Fprintln(os.Stderr, "Permission denied")
        os.Exit(1)
    }

    switch operation {
    case "format-disk":
        formatDisk(os.Args[2])
    case "copy-into-disk":
        copyIntoDisk(os.Args[2], os.Args[3])
    default:
        fmt.Fprintf(os.Stderr, "Unknown operation: %s\n", operation)
        os.Exit(1)
    }
}

func formatDisk(diskPath string) {
    // Validate path is within allowed directory
    if !isValidPath(diskPath) {
        fmt.Fprintln(os.Stderr, "Invalid path")
        os.Exit(1)
    }

    cmd := exec.Command("virt-format", "--filesystem=ext4", diskPath)
    cmd.Stdout = os.Stdout
    cmd.Stderr = os.Stderr
    if err := cmd.Run(); err != nil {
        os.Exit(1)
    }
}

func isValidPath(path string) bool {
    // Only allow paths under /var/lib/cocoon or /srv/cocoon
    allowedDirs := []string{"/var/lib/cocoon", "/srv/cocoon"}
    absPath, _ := filepath.Abs(path)

    for _, dir := range allowedDirs {
        if filepath.HasPrefix(absPath, dir) {
            return true
        }
    }
    return false
}
```

**Build and install setuid helper**:
```bash
go build -o cocoon-helper cocoon-helper.go
sudo chown root:cocoon cocoon-helper
sudo chmod 4750 cocoon-helper  # setuid root, executable by cocoon group
sudo mv cocoon-helper /usr/local/bin/
```

## Installation Guides by Distribution

### Ubuntu 22.04 LTS

```bash
#!/bin/bash
set -e

# 1. Update package index
sudo apt-get update

# 2. Install Cloud Hypervisor
CH_VERSION="v38.0"
curl -LO https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/${CH_VERSION}/cloud-hypervisor-static
chmod +x cloud-hypervisor-static
sudo mv cloud-hypervisor-static /usr/local/bin/cloud-hypervisor

# 3. Install dependencies
sudo apt-get install -y \
    buildah \
    skopeo \
    qemu-utils \
    libguestfs-tools \
    ovmf \
    fuse-overlayfs

# 4. Configure KVM access
sudo usermod -aG kvm $USER

# 5. Enable user namespaces for rootless
echo "kernel.unprivileged_userns_clone=1" | sudo tee /etc/sysctl.d/99-rootless.conf
sudo sysctl -p /etc/sysctl.d/99-rootless.conf

# 6. Configure subuid/subgid
grep $USER /etc/subuid || echo "$USER:100000:65536" | sudo tee -a /etc/subuid
grep $USER /etc/subgid || echo "$USER:100000:65536" | sudo tee -a /etc/subgid

# 7. Verify installation
echo "Installation complete. Log out and back in, then run:"
echo "  cloud-hypervisor --version"
echo "  buildah version"
echo "  cocoon doctor"
```

### Ubuntu 24.04 LTS

Same as Ubuntu 22.04, but newer package versions available:

```bash
#!/bin/bash
set -e

sudo apt-get update
sudo apt-get install -y \
    buildah \
    skopeo \
    qemu-utils \
    libguestfs-tools \
    ovmf \
    fuse-overlayfs

# Download Cloud Hypervisor (same as 22.04)
CH_VERSION="v38.0"
curl -LO https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/${CH_VERSION}/cloud-hypervisor-static
chmod +x cloud-hypervisor-static
sudo mv cloud-hypervisor-static /usr/local/bin/cloud-hypervisor

# KVM and rootless setup (same as 22.04)
sudo usermod -aG kvm $USER
echo "kernel.unprivileged_userns_clone=1" | sudo tee /etc/sysctl.d/99-rootless.conf
sudo sysctl -p /etc/sysctl.d/99-rootless.conf

grep $USER /etc/subuid || echo "$USER:100000:65536" | sudo tee -a /etc/subuid
grep $USER /etc/subgid || echo "$USER:100000:65536" | sudo tee -a /etc/subgid
```

### Fedora 39

```bash
#!/bin/bash
set -e

# 1. Install dependencies
sudo dnf install -y \
    buildah \
    skopeo \
    qemu-img \
    libguestfs-tools \
    edk2-ovmf \
    fuse-overlayfs

# 2. Install Cloud Hypervisor
CH_VERSION="v38.0"
curl -LO https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/${CH_VERSION}/cloud-hypervisor-static
chmod +x cloud-hypervisor-static
sudo mv cloud-hypervisor-static /usr/local/bin/cloud-hypervisor

# 3. Configure KVM access
sudo usermod -aG kvm $USER

# 4. Configure subuid/subgid (usually pre-configured on Fedora)
grep $USER /etc/subuid || echo "$USER:100000:65536" | sudo tee -a /etc/subuid
grep $USER /etc/subgid || echo "$USER:100000:65536" | sudo tee -a /etc/subgid

# 5. Verify
echo "Installation complete. Log out and back in, then run:"
echo "  cloud-hypervisor --version"
echo "  buildah version"
```

### Fedora 40

Same as Fedora 39 (package names unchanged).

## Failure Modes and Error Messages

### 1. buildah not found

**Error**:
```
Error: buildah: command not found
Failed to pull OCI image: executable not found
```

**Solution**:
```bash
# Ubuntu
sudo apt-get install -y buildah

# Fedora
sudo dnf install -y buildah
```

### 2. /dev/kvm not accessible

**Error**:
```
Error: Failed to open /dev/kvm: Permission denied
KVM not available. Check virtualization enabled and user in kvm group.
```

**Solution**:
```bash
# Check if KVM exists
ls -l /dev/kvm

# If missing, ensure virtualization is enabled in BIOS
# If exists but permission denied:
sudo usermod -aG kvm $USER
newgrp kvm  # Or log out and back in
```

### 3. virt-format not found

**Error**:
```
Error: virt-format: command not found
Cannot format disks. Install libguestfs-tools or use manual disk creation.
```

**Solution (Rootful)**:
```bash
# Ubuntu
sudo apt-get install -y libguestfs-tools

# Fedora
sudo dnf install -y libguestfs-tools
```

**Alternative (Rootless)**:
Skip virt-format and use pre-built images or manual qcow2 creation.

### 4. Kernel modules not loaded

**Error**:
```
Error: Could not access KVM kernel module: No such file or directory
```

**Solution**:
```bash
# Check if modules are loaded
lsmod | grep kvm

# If not loaded, try loading manually
sudo modprobe kvm
sudo modprobe kvm_intel  # or kvm_amd for AMD

# Make permanent
echo "kvm" | sudo tee -a /etc/modules
echo "kvm_intel" | sudo tee -a /etc/modules  # or kvm_amd
```

### 5. Virtualization not enabled

**Error**:
```
Error: KVM device not found at /dev/kvm
```

**Solution**:
1. Reboot and enter BIOS/UEFI setup
2. Find virtualization settings (usually under "CPU Configuration" or "Advanced")
3. Enable:
   - Intel: Intel VT-x / Intel Virtualization Technology
   - AMD: AMD-V / SVM Mode
4. Save and reboot
5. Verify: `grep -E '(vmx|svm)' /proc/cpuinfo`

### 6. User namespaces disabled

**Error**:
```
Error: cannot set up namespace using newuidmap: exit status 1
```

**Solution**:
```bash
# Enable unprivileged user namespaces
echo "kernel.unprivileged_userns_clone=1" | sudo tee /etc/sysctl.d/99-rootless.conf
sudo sysctl -p /etc/sysctl.d/99-rootless.conf

# Configure subuid/subgid
grep $USER /etc/subuid || echo "$USER:100000:65536" | sudo tee -a /etc/subuid
grep $USER /etc/subgid || echo "$USER:100000:65536" | sudo tee -a /etc/subgid
```

### 7. OVMF firmware not found

**Error**:
```
Error: Failed to load firmware: No such file or directory
```

**Solution**:
```bash
# Ubuntu
sudo apt-get install -y ovmf
ls -l /usr/share/OVMF/OVMF_CODE.fd

# Fedora
sudo dnf install -y edk2-ovmf
ls -l /usr/share/edk2/ovmf/OVMF_CODE.fd
```

## Version Pinning and Compatibility

### Tested Configurations

| OS | Kernel | cloud-hypervisor | buildah | qemu-img | libguestfs | Status |
|----|--------|------------------|---------|----------|------------|--------|
| Ubuntu 22.04 | 5.15 | v38.0 | 1.35.0 | 6.2 | 1.46 | ✅ Tested |
| Ubuntu 24.04 | 6.8 | v38.0 | 1.35.0 | 8.2 | 1.50 | ✅ Tested |
| Fedora 39 | 6.5 | v38.0 | 1.35.3 | 8.1 | 1.50 | ✅ Tested |
| Fedora 40 | 6.8 | v38.0 | 1.36.0 | 8.2 | 1.52 | ✅ Tested |
| Debian 12 | 6.1 | v38.0 | 1.28.0 | 7.2 | 1.48 | ⚠️ Partial (buildah too old) |

### Version Warnings

Cocoon will warn if dependency versions are below recommended minimums:

```
⚠️  buildah 1.28.0 detected (minimum: 1.35.0)
   Some features may not work correctly.
   Consider upgrading: sudo apt-get install -t bookworm-backports buildah

⚠️  qemu-img 7.2 detected (minimum: 8.0)
   qcow2 operations may have limited functionality.
   Consider upgrading from backports or upstream.
```

### Upgrading Dependencies

**Ubuntu (using backports)**:
```bash
# Add backports repository
sudo add-apt-repository -y ppa:projectatomic/ppa  # For buildah
sudo apt-get update

# Upgrade specific package
sudo apt-get install -y buildah
```

**Fedora (from testing repository)**:
```bash
# Enable updates-testing
sudo dnf install -y --enablerepo=updates-testing buildah
```

## Troubleshooting

### Diagnostic Commands

```bash
# Full system check
cocoon doctor

# Check specific component
cloud-hypervisor --version
buildah version
qemu-img --version
virt-format --version

# Check KVM
ls -l /dev/kvm
lsmod | grep kvm
grep -E '(vmx|svm)' /proc/cpuinfo

# Check user namespaces
sysctl kernel.unprivileged_userns_clone
cat /etc/subuid | grep $USER
cat /etc/subgid | grep $USER

# Test buildah rootless
buildah pull alpine:latest
buildah images
```

### Common Issues

**Issue: Slow image conversion**

**Symptoms**: qcow2 conversion takes minutes per image

**Solution**:
- Use faster storage (SSD instead of HDD)
- Increase available RAM for libguestfs: `export LIBGUESTFS_MEMSIZE=2048`
- Use compressed qcow2 images: `qemu-img convert -c -O qcow2`

**Issue: Disk space exhausted**

**Symptoms**: "No space left on device" during image pull/convert

**Solution**:
```bash
# Check disk usage
df -h /var/lib/cocoon

# Clean up unused images
cocoon image prune

# Run garbage collector
cocoon gc

# Increase storage allocation or move to larger partition
```

**Issue: Permission denied when creating VMs**

**Symptoms**: "Permission denied" errors despite being in kvm group

**Solution**:
```bash
# Verify group membership is active
groups | grep kvm

# If not present, re-login or use:
newgrp kvm

# Verify /dev/kvm permissions
ls -l /dev/kvm
# Should show: crw-rw---- 1 root kvm
```

## Summary

Cocoon's dependencies are designed for:
- **Production deployment**: Use Hybrid mode (Option C) for best security and features
- **Development**: Use Rootless mode (Option A) for simplicity
- **Testing**: Use pre-installed container environments with all dependencies

Run `cocoon doctor` after installation to verify all dependencies are correctly configured.

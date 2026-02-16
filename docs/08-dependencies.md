# Dependencies and Requirements

**Version**: 1.0
**Status**: Implemented
**Phase**: Phase 1
**Last Updated**: 2026-02-14

## Overview

Cocoon relies on several external tools and libraries to provide VM management with OCI image support. This guide covers all dependencies, their purposes, installation instructions, version requirements, and troubleshooting.

## Dependency Matrix

| Tool | Purpose | Min Version | Installation Method |
|------|---------|-------------|---------------------|
| cloud-hypervisor | Virtual Machine Monitor (VMM) | v38.0 | Binary or source |
| ch-remote | CH remote control (REST API client) | v38.0 | Binary (ships with CH release) |
| edk2-cloudhv | UEFI firmware for CH (default boot mode) | ch-a54f262b09 | `cocoon firmware install` or `cocoon init --with-uefi-firmware <URL>` |
| buildah | OCI image pull/extract | 1.35.0 | apt/dnf |
| skopeo | OCI image inspection | 1.14.0 | apt/dnf |
| qemu-img | qcow2 operations | 8.0 | apt/dnf (qemu-utils) |
| guestfish | OCI-to-qcow2 conversion (partition, copy rootfs) | libguestfs 1.50 | apt/dnf (libguestfs-tools) |
| swtpm | TPM 2.0 software emulator for VM TPM support | any | apt/dnf (swtpm, swtpm-tools) |
| /dev/kvm | KVM device access | kernel 5.6+ | Built-in (kernel module) |

## Core Dependencies

### 1. Cloud Hypervisor

**Purpose**: Production-grade Virtual Machine Monitor built in Rust for running lightweight VMs.

**Minimum Version**: v38.0

Phase 2 features (snapshot/restore, pause/resume) may require a higher minimum version; see individual Phase 2 design documents for version requirements.

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

### 2. CLOUDHV.fd (Cloud Hypervisor UEFI Firmware)

**Purpose**: Provides UEFI firmware for the default boot mode. Installed via `cocoon firmware install` or `cocoon init --with-uefi-firmware <URL>` (`cocoon init` alone creates directories but does not download firmware).

**Default Version**: edk2 CLOUDHV.fd from the [cloud-hypervisor/edk2 releases](https://github.com/cloud-hypervisor/edk2/releases)

**Installation**:

**Option 1: From edk2-cloudhv releases** (recommended):
```bash
# Recommended: use 'cocoon firmware install'
cocoon firmware install

# Or manually download from the edk2 Cloud Hypervisor releases:
curl -L https://github.com/cloud-hypervisor/edk2/releases/latest/download/CLOUDHV.fd \
    -o /tmp/CLOUDHV.fd

# Install to firmware directory
sudo mv /tmp/CLOUDHV.fd /var/lib/cocoon/firmware/CLOUDHV.fd
sudo chmod 644 /var/lib/cocoon/firmware/CLOUDHV.fd
```

**Option 2: From distribution packages (deprecated fallback)**:
```bash
# Ubuntu/Debian - Install standard OVMF (deprecated fallback, only used if CLOUDHV.fd missing)
sudo apt-get install -y ovmf
# Firmware location: /usr/share/OVMF/OVMF_CODE.fd

# Fedora - Install edk2-ovmf (deprecated fallback, only used if CLOUDHV.fd missing)
sudo dnf install -y edk2-ovmf
# Firmware location: /usr/share/edk2/ovmf/OVMF_CODE.fd
```

**Verification**:
```bash
# Verify CLOUDHV.fd exists (primary UEFI firmware)
ls -lh /var/lib/cocoon/firmware/CLOUDHV.fd

# Or verify system OVMF (deprecated fallback)
ls -l /usr/share/OVMF/OVMF_CODE.fd      # Ubuntu/Debian (deprecated fallback)
ls -l /usr/share/edk2/ovmf/OVMF_CODE.fd  # Fedora (deprecated fallback)
```

### 3. Buildah

**Purpose**: Pulls and extracts OCI container images without requiring a daemon.

**Minimum Version**: 1.35.0

**Installation**:

**Ubuntu 22.04/24.04**:
```bash
sudo apt-get update
sudo apt-get install -y buildah
```

**Fedora 39/40**:
```bash
sudo dnf install -y buildah
```

**Verification**:
```bash
buildah version
# Expected: Version: 1.35.0 or higher
```

### 4. Skopeo

**Purpose**: Inspects OCI images and manifests to calculate checksums for caching.

**Minimum Version**: 1.14.0

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

### 5. QEMU Image Tools (qemu-img)

**Purpose**: Creates, converts, and manages qcow2 disk images.

**Minimum Version**: 8.0

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

### 6. libguestfs Tools

**Purpose**: Formats disk images and copies files into them.

**Minimum Version**: libguestfs 1.50

**Tools Included**:
- `guestfish`: Interactive and scriptable filesystem access for disk images (used for formatting, copying files, and bootability verification)

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
guestfish --version
```

### 7. swtpm (TPM 2.0 Emulator)

**Purpose**: Provides software-based TPM 2.0 emulation for VMs that need TPM support (measured boot, guest attestation). Enabled via the `--tpm` flag on `cocoon create` and `cocoon run`.

**Packages**: `swtpm`, `swtpm-tools`

**Installation**:

**Ubuntu**:
```bash
sudo apt-get install -y swtpm swtpm-tools
```

**Fedora**:
```bash
sudo dnf install -y swtpm swtpm-tools
```

**Verification**:
```bash
swtpm --version
```

**AppArmor Note**: On Ubuntu systems with AppArmor enabled, the swtpm AppArmor profile may block socket creation under `/run/cocoon/`. The `scripts/lib.sh` setup script automatically disables this profile. If swtpm fails with "Permission denied", check the AppArmor profile status.

### 8. OVMF (UEFI Firmware) - DEPRECATED

**⚠️ Note**: This section is deprecated. Prefer using CLOUDHV.fd from Cloud Hypervisor releases (see section 3 above).

**Purpose**: Provides standard OVMF UEFI firmware as a deprecated fallback option when CLOUDHV.fd (`/var/lib/cocoon/firmware/CLOUDHV.fd`) is unavailable.

**Installation**:

**Ubuntu**:
```bash
sudo apt-get install -y ovmf
# Firmware installed to: /usr/share/OVMF/OVMF_CODE.fd
```

**Fedora**:
```bash
sudo dnf install -y edk2-ovmf
# Firmware installed to: /usr/share/edk2/ovmf/OVMF_CODE.fd
```

**Verification**:
```bash
# Ubuntu
ls -l /usr/share/OVMF/OVMF_CODE.fd

# Fedora
ls -l /usr/share/edk2/ovmf/OVMF_CODE.fd
```

**Usage Priority**:
1. **Default**: CLOUDHV.fd (Cloud Hypervisor's edk2 UEFI firmware)
2. **UEFI fallback**: OVMF_CODE.fd (standard OVMF, only if CLOUDHV.fd missing)

### 9. KVM Device Access

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
            Name:           "guestfish",
            Command:        "guestfish",
            Args:           []string{"--version"},
            VersionPattern: `guestfish`,
            Required:       true, // Required for OCI-to-qcow2 conversion and deep bootability verification; cocoon doctor exits with failure if missing
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

    // Check firmware files
    firmwareChecks := checkFirmwareFiles()
    results = append(results, firmwareChecks...)

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

func checkFirmwareFiles() []DependencyStatus {
    firmwareChecks := []struct {
        name       string
        path       string
        required   bool
        installCmd string
    }{
        {
            name:     "CLOUDHV.fd",
            path:     "/var/lib/cocoon/firmware/CLOUDHV.fd",
            required: true,
            installCmd: "curl -L https://github.com/cloud-hypervisor/edk2/releases/download/ch-a54f262b09/CLOUDHV.fd -o /tmp/CLOUDHV.fd && sudo mv /tmp/CLOUDHV.fd /var/lib/cocoon/firmware/",
        },
    }

    results := []DependencyStatus{}

    for _, check := range firmwareChecks {
        status := DependencyStatus{
            Name: check.name,
            Path: check.path,
            InstallCmd: check.installCmd,
        }

        // Check if file exists
        info, err := os.Stat(check.path)
        if err != nil {
            status.Found = false
            status.Error = "File not found"
            results = append(results, status)
            continue
        }

        status.Found = true
        status.Version = fmt.Sprintf("%d bytes", info.Size())

        results = append(results, status)
    }

    // Also check for deprecated fallback OVMF locations
    ovmfPaths := []string{
        "/usr/share/OVMF/OVMF_CODE.fd",
        "/usr/share/edk2/ovmf/OVMF_CODE.fd",
    }

    ovmfFound := false
    for _, path := range ovmfPaths {
        if _, err := os.Stat(path); err == nil {
            results = append(results, DependencyStatus{
                Name:    "OVMF (deprecated fallback)",
                Found:   true,
                Path:    path,
                Version: "available",
            })
            ovmfFound = true
            break
        }
    }

    if !ovmfFound {
        results = append(results, DependencyStatus{
            Name:       "OVMF (fallback)",
            Found:      false,
            Error:      "Not found at standard locations",
            InstallCmd: "sudo apt-get install ovmf (Ubuntu) or sudo dnf install edk2-ovmf (Fedora)",
        })
    }

    return results
}

func getInstallCommand(name string) string {
    installCmds := map[string]string{
        "cloud-hypervisor": "curl -LO https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v38.0/cloud-hypervisor-static && chmod +x cloud-hypervisor-static && sudo mv cloud-hypervisor-static /usr/local/bin/cloud-hypervisor",
        "buildah":          "sudo apt-get install buildah (Ubuntu) or sudo dnf install buildah (Fedora)",
        "skopeo":           "sudo apt-get install skopeo (Ubuntu) or sudo dnf install skopeo (Fedora)",
        "qemu-img":         "sudo apt-get install qemu-utils (Ubuntu) or sudo dnf install qemu-img (Fedora)",
        "guestfish":        "sudo apt-get install libguestfs-tools (Ubuntu) or sudo dnf install libguestfs-tools (Fedora)",
    }
    return installCmds[name]
}
```

### Example Output

```
Cocoon Dependency Check
=======================

Core Dependencies:
✅ cloud-hypervisor v38.0 found at /usr/local/bin/cloud-hypervisor
✅ buildah 1.35.0 found at /usr/bin/buildah
✅ skopeo 1.14.0 found at /usr/bin/skopeo
✅ qemu-img 8.2.0 found at /usr/bin/qemu-img
❌ guestfish not found (required)
   → Install: sudo apt-get install libguestfs-tools
   → Note: Required for OCI-to-qcow2 conversion and deep bootability verification
✅ /dev/kvm accessible

Firmware Files:
✅ CLOUDHV.fd found at /var/lib/cocoon/firmware/CLOUDHV.fd (2145728 bytes)
✅ OVMF (deprecated fallback) available at /usr/share/OVMF/OVMF_CODE.fd

swtpm:
✅ swtpm 0.9.0 found at /usr/bin/swtpm

Summary: 9/10 required dependencies found
1 required dependency missing (guestfish)
Status: Not ready (install missing dependencies)
```

## Privilege Model

Cocoon requires root privileges. All VM operations (hypervisor management, image conversion, storage) run as root.

**Setup**:

```bash
# Install all dependencies
sudo apt-get install -y cloud-hypervisor buildah skopeo qemu-utils libguestfs-tools ovmf swtpm swtpm-tools

# Run cocoon as root
sudo cocoon create ubuntu-22.04-cloudimg --name myvm
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
    swtpm \
    swtpm-tools

# 4. Configure KVM access
sudo usermod -aG kvm $USER

# 5. Verify installation
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
    swtpm \
    swtpm-tools

# Download Cloud Hypervisor (same as 22.04)
CH_VERSION="v38.0"
curl -LO https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/${CH_VERSION}/cloud-hypervisor-static
chmod +x cloud-hypervisor-static
sudo mv cloud-hypervisor-static /usr/local/bin/cloud-hypervisor

# KVM access
sudo usermod -aG kvm $USER
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
    swtpm \
    swtpm-tools

# 2. Install Cloud Hypervisor
CH_VERSION="v38.0"
curl -LO https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/${CH_VERSION}/cloud-hypervisor-static
chmod +x cloud-hypervisor-static
sudo mv cloud-hypervisor-static /usr/local/bin/cloud-hypervisor

# 3. Configure KVM access
sudo usermod -aG kvm $USER

# 4. Verify
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

### 3. guestfish not found

**Error**:
```
Error: guestfish: command not found
Deep bootability verification unavailable. Install libguestfs-tools.
```

**Solution**:
```bash
# Ubuntu
sudo apt-get install -y libguestfs-tools

# Fedora
sudo dnf install -y libguestfs-tools
```

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

### 6. UEFI firmware not found

**Error**:
```
Error: Failed to load UEFI firmware: No such file or directory
Cannot start VM with UEFI boot mode
```

**Solution (Option 1 - CLOUDHV.fd from edk2-cloudhv releases)**:
```bash
# Download CLOUDHV.fd from edk2 Cloud Hypervisor releases
curl -L https://github.com/cloud-hypervisor/edk2/releases/download/ch-a54f262b09/CLOUDHV.fd \
    -o /tmp/CLOUDHV.fd
sudo mv /tmp/CLOUDHV.fd /var/lib/cocoon/firmware/CLOUDHV.fd
sudo chmod 644 /var/lib/cocoon/firmware/CLOUDHV.fd
```

**Solution (Option 2 - System OVMF deprecated fallback)**:
```bash
# Ubuntu (deprecated fallback, only used if CLOUDHV.fd missing)
sudo apt-get install -y ovmf
ls -l /usr/share/OVMF/OVMF_CODE.fd

# Fedora (deprecated fallback, only used if CLOUDHV.fd missing)
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

### Version Failures

Cocoon will fail if dependency versions are below recommended minimums:

```
❌  buildah 1.28.0 detected (minimum: 1.35.0)
   cocoon doctor reports status: fail
   Consider upgrading: sudo apt-get install -t bookworm-backports buildah

❌  qemu-img 7.2 detected (minimum: 8.0)
   cocoon doctor reports status: fail
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
guestfish --version

# Check KVM
ls -l /dev/kvm
lsmod | grep kvm
grep -E '(vmx|svm)' /proc/cpuinfo

# Test buildah
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

Run cocoon as root. Run `cocoon doctor` after installation to verify all dependencies are correctly configured.

# Installation

**Version**: 1.0
**Status**: Implemented
**Phase**: Phase 1
**Last Updated**: 2026-02-14

## Overview

Cloud Hypervisor is a production-grade Virtual Machine Monitor (VMM) built in Rust, designed for modern cloud workloads. This guide covers installing Cloud Hypervisor for Cocoon AI Agent sandbox environments.

## Prerequisites

### Linux Kernel Requirements

Cloud Hypervisor requires a modern Linux kernel with KVM support:

- **Minimum kernel version**: 5.6 or later
- **Recommended kernel version**: 5.10 LTS or newer
- **Architecture support**: x86_64 (Intel/AMD), aarch64 (ARM64)

Verify your kernel version:

```bash
uname -r
```

### KVM Support Verification

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

### System Requirements

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

## Dependencies

For a complete list of build and runtime dependencies, see [Dependencies](08-dependencies.md).

## Installation

### Pre-built Releases (Recommended)

Using pre-built binaries is the fastest and most reliable installation method.

**1. Download the latest release:**

```bash
# Set desired version (check https://github.com/cloud-hypervisor/cloud-hypervisor/releases)
CH_VERSION="v50.0"

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

## Firmware Setup

### Firmware Requirements

Cloud Hypervisor requires firmware to boot virtual machines. Cocoon supports two boot modes per [Boot Contract v2.0](./01-boot-contract.md). For authoritative CLI behavior (flags, subcommands, exit codes), see [docs/09-cli-design.md](./09-cli-design.md).

#### UEFI Firmware (Default)

**What is UEFI?**
- UEFI (Unified Extensible Firmware Interface) provides the standard firmware environment
- **Broadest compatibility**: Works with all Linux distributions and cloud images
- **Secure boot ready**: Supports secure boot for production workloads
- CLOUDHV.fd is the Cloud Hypervisor project's own edk2 build, optimized for CH

**Installation** (via `cocoon firmware install`, or manual):

```bash
# Recommended — install firmware after cocoon init:
sudo cocoon init                  # Creates directories and config (no firmware download)
sudo cocoon firmware install      # Downloads CLOUDHV.fd from the default edk2 release URL

# Convenience shortcut — download firmware during init:
sudo cocoon init --with-uefi-firmware "https://github.com/cloud-hypervisor/edk2/releases/download/ch-a54f262b09/CLOUDHV.fd"

# Manual download from edk2-cloudhv releases:
curl -L "https://github.com/cloud-hypervisor/edk2/releases/latest/download/CLOUDHV.fd" \
    -o /tmp/CLOUDHV.fd
sudo mkdir -p /var/lib/cocoon/firmware
sudo mv /tmp/CLOUDHV.fd /var/lib/cocoon/firmware/CLOUDHV.fd
sudo chmod 644 /var/lib/cocoon/firmware/CLOUDHV.fd
```

**Deprecated fallback (system OVMF -- only used if CLOUDHV.fd missing):**

```bash
# Ubuntu/Debian - Install OVMF (deprecated fallback)
sudo apt-get install -y ovmf
# Firmware at: /usr/share/OVMF/OVMF_CODE.fd

# Fedora/RHEL - Install edk2-ovmf (deprecated fallback)
sudo dnf install -y edk2-ovmf
# Firmware at: /usr/share/edk2/ovmf/OVMF_CODE.fd
```

**UEFI Boot Behavior**:
- All VM configuration (including firmware path) goes through the REST `vm.create` payload; CLI only passes `--api-socket`
- Firmware is set as `payload.firmware` in the REST payload
- Recommended: CLOUDHV.fd at `/var/lib/cocoon/firmware/CLOUDHV.fd`
- Deprecated fallback: System OVMF (`OVMF_CODE.fd`) — only probed if CLOUDHV.fd is missing

### Firmware Selection Guide

| Boot Method | Firmware | OS Support | Phase |
|-------------|----------|------------|-------|
| **UEFI (Default)** | CLOUDHV.fd (deprecated fallback: OVMF) | All Linux distributions and cloud images | Phase 1 |
| **Direct kernel boot** | None (kernel + initramfs passed directly) | OCI VM images | Phase 2 |

**Cocoon Default Strategy** (per Boot Contract v2.0):
- UEFI boot by default for cloud images (broadest compatibility)
- Direct kernel boot automatically for OCI VM images (no firmware needed)

### Firmware Updates

**Update OVMF (UEFI firmware):**

```bash
# Ubuntu/Debian
sudo apt-get update && sudo apt-get upgrade ovmf

# Fedora
sudo dnf upgrade edk2-ovmf
```

## Directory Structure

### Recommended Structure

Recommended directory structure for Cocoon deployment:

```
/var/lib/cocoon/                   # Cocoon root directory (per Boot Contract v2.0)
├── firmware/
│   └── CLOUDHV.fd                 # UEFI firmware (default)
├── cache/
│   ├── images/                    # Base image cache (qcow2)
│   ├── manifests/                 # IMAGE_REF -> base_key alias index
│   └── locks/                     # Per-image conversion locks
├── db/                            # Runtime metadata indexes
│   ├── references.json            # base_key -> vm references
│   └── name-index.json            # vm name -> vm_id
├── vms/                           # VM instances
│   ├── vm-abc-123/
│   │   ├── overlay.qcow2          # COW overlay disk
│   │   ├── config.json            # VM configuration
│   │   └── metadata.json          # VM metadata
│   └── vm-def-456/
├── temp/                          # Temporary conversion files
└── trash/                         # Soft-deleted GC artifacts (image/overlay)

/var/log/cocoon/                   # Logs
├── vm-abc-123.log                 # Serial console logs (per-VM)
└── vm-def-456.log

/run/cocoon/                       # Runtime sockets
└── vms/
    ├── vm-abc-123/
    │   ├── api.sock               # Cloud Hypervisor API socket
    │   └── ch.pid                 # Process ID file
    └── vm-def-456/

/etc/cocoon/                       # Configuration (optional)
└── config.json                    # Global Cocoon config
```

**Setup commands:**

```bash
# Create Cocoon directories
sudo mkdir -p /var/lib/cocoon/firmware
sudo mkdir -p /var/lib/cocoon/cache/images
sudo mkdir -p /var/lib/cocoon/vms
sudo mkdir -p /var/lib/cocoon/temp

sudo mkdir -p /var/log/cocoon
sudo mkdir -p /run/cocoon/vms

# Set appropriate permissions
sudo chown -R $USER:$USER /var/lib/cocoon
sudo chown -R $USER:$USER /var/log/cocoon
sudo chown -R $USER:$USER /run/cocoon

sudo chmod -R 755 /var/lib/cocoon
sudo chmod -R 755 /var/log/cocoon
sudo chmod -R 755 /run/cocoon
```

## Verification

### Basic Functionality Test

**1. Verify Cloud Hypervisor installation:**

```bash
cloud-hypervisor --version
# Expected output: cloud-hypervisor v50.0
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

### Sample VM Creation Test

Create a minimal test VM to verify full functionality:

**1. Create a small test disk image:**

```bash
# Create a 1GB raw disk image
dd if=/dev/zero of=/tmp/test-disk.raw bs=1M count=1024

# Format as ext4
mkfs.ext4 /tmp/test-disk.raw
```

**2. Launch a test VM (requires a bootable kernel/image):**

```bash
# Example: Launch with UEFI firmware
# Note: This requires a bootable disk with kernel/bootloader
cloud-hypervisor \
    --firmware /var/lib/cocoon/firmware/CLOUDHV.fd \
    --disk path=/tmp/test-disk.raw \
    --cpus boot=1 \
    --memory size=512M \
    --console off \
    --serial tty &

# Allow VM to boot
sleep 3

# Check VM process is running
ps aux | grep cloud-hypervisor
```

**3. Clean up:**

```bash
# Kill the VM process
killall cloud-hypervisor

# Remove test disk
rm /tmp/test-disk.raw
```

### Integration Test

Verify Cloud Hypervisor can interface with standard tools:

**Test REST API:**

```bash
# Start Cloud Hypervisor with API
cloud-hypervisor --api-socket /tmp/ch-api.sock &
sleep 2

# Create VM via API
curl -X PUT 'http://localhost/api/v1/vm.create' \
    -H 'Content-Type: application/json' \
    --unix-socket /tmp/ch-api.sock

# Check VM info
curl -X GET 'http://localhost/api/v1/vm.info' \
    --unix-socket /tmp/ch-api.sock

# Cleanup
killall cloud-hypervisor
rm /tmp/ch-api.sock
```

## Security Considerations

### File Permissions

Set appropriate permissions for sensitive components:

```bash
# Firmware (read-only for users)
sudo chmod 644 /var/lib/cocoon/firmware/*

# VM overlays (read-write for owner only)
chmod 600 /var/lib/cocoon/vms/*/overlay.qcow2

# Base images (read-only after creation)
chmod 644 /var/lib/cocoon/cache/images/*.qcow2

# Logs (read-only for owner)
chmod 644 /var/log/cocoon/*.log
```

### KVM Device Access

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

### Seccomp and Landlock

Cloud Hypervisor includes built-in security features:

- **Seccomp**: Restricts system calls available to the VM process
- **Landlock**: Linux Security Module for fine-grained access control

These are enabled by default in release builds. Verify with:

```bash
# Check if Cloud Hypervisor was built with security features
strings /usr/local/bin/cloud-hypervisor | grep -i seccomp
```

## Troubleshooting

### Common Issues

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
# Check UEFI firmware installation
ls -l /var/lib/cocoon/firmware/CLOUDHV.fd

# If missing, download it (recommended: use 'cocoon firmware install')
cocoon firmware install   # Downloads from the default edk2 release URL; use --uefi-url to override
# Or manually:
curl -L "https://github.com/cloud-hypervisor/edk2/releases/latest/download/CLOUDHV.fd" \
    -o /tmp/CLOUDHV.fd
sudo mv /tmp/CLOUDHV.fd /var/lib/cocoon/firmware/CLOUDHV.fd
sudo chmod 644 /var/lib/cocoon/firmware/CLOUDHV.fd

# Or install system UEFI firmware as fallback (OVMF)
sudo apt-get install -y ovmf
```

**3. Insufficient memory:**

```
Error: Cannot allocate memory
```

**Solution:**
```bash
# Check available memory
free -h

# Reduce VM memory allocation or close other applications
```

### Debug Mode

Enable verbose logging for troubleshooting:

```bash
# Set RUST_LOG environment variable
RUST_LOG=debug cloud-hypervisor --api-socket /tmp/ch-api.sock

# Or use -v flag
cloud-hypervisor -v --api-socket /tmp/ch-api.sock
```

## Next Steps

After successful installation:

1. **Image Management**: Set up OCI image handling and qcow2 disk management
2. **CLI Development**: Create wrapper scripts or CLI tools for simplified VM management
3. **Network Configuration**: Configure TAP devices and bridge networking for VM connectivity
4. **Monitoring**: Set up logging and monitoring for VM instances
5. **Integration**: Integrate Cloud Hypervisor with your AI agent sandbox framework

## References

- Cloud Hypervisor GitHub: https://github.com/cloud-hypervisor/cloud-hypervisor
- Official Documentation: https://www.cloudhypervisor.org/
- KVM Documentation: https://www.linux-kvm.org/

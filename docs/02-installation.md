# Installation

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

## Firmware Setup

### Firmware Requirements

Cloud Hypervisor requires firmware to boot virtual machines. Cocoon supports two boot modes per [Boot Contract v2.0](./01-boot-contract.md):

#### PVH Firmware (Recommended - Phase 1 Primary)

**What is PVH?**
- PVH (Paravirtualized Hardware) is a lightweight boot protocol designed for Cloud Hypervisor
- **Fast boot**: Sub-100ms boot time (vs ~500ms for UEFI)
- **Standard cloud images**: Works with Ubuntu Cloud, Fedora Cloud, Debian Cloud images out of the box
- **Disk-based boot**: Loads kernel from GPT+ESP partition like standard VMs

**rust-hypervisor-firmware** is the PVH firmware implementation:
- Boots via PVH entry point (Xen PVH protocol)
- Discovers virtio-blk disks and parses GPT
- Mounts ESP partition and loads GRUB/kernel
- Minimal footprint (~100KB vs 2MB OVMF)

**Installation:**

```bash
# Create firmware directory
sudo mkdir -p /var/lib/cocoon/firmware

# Download rust-hypervisor-firmware (x86_64)
curl -L https://github.com/cloud-hypervisor/rust-hypervisor-firmware/releases/download/0.4.2/hypervisor-fw \
    -o /tmp/hypervisor-fw

# Install
sudo mv /tmp/hypervisor-fw /var/lib/cocoon/firmware/hypervisor-fw
sudo chmod +x /var/lib/cocoon/firmware/hypervisor-fw

# Verify download (optional but recommended)
sha256sum /var/lib/cocoon/firmware/hypervisor-fw
# Expected: (check GitHub releases page)
```

**VM Launch with PVH:**

```bash
cloud-hypervisor \
    --kernel /var/lib/cocoon/firmware/hypervisor-fw \
    --disk path=/var/lib/cocoon/vms/vm-123/overlay.qcow2 \
    --cpus boot=2 \
    --memory size=2G \
    --serial file=/var/log/cocoon/vm-123.log \
    --console off
```

**Note**: `--kernel` parameter accepts PVH firmware path. The firmware then loads the actual kernel from the disk's ESP partition.

#### UEFI Firmware (Fallback)

**What is UEFI?**
- UEFI (Unified Extensible Firmware Interface) provides traditional firmware environment
- Supports secure boot, older distributions, and specialized UEFI-only features
- Slower boot (~500ms) but broader compatibility

**When to use UEFI fallback:**
1. Image explicitly requires UEFI (metadata flag)
2. PVH boot fails (automatic retry)
3. User specifies `--boot-mode uefi` flag

**Installation:**

```bash
# Ubuntu/Debian - Install OVMF
sudo apt-get install -y ovmf

# Fedora/RHEL - Install edk2-ovmf
sudo dnf install -y edk2-ovmf

# Firmware will be installed at:
# - Ubuntu/Debian: /usr/share/OVMF/OVMF_CODE.fd
# - Fedora: /usr/share/edk2/ovmf/OVMF_CODE.fd
```

**VM Launch with UEFI:**

```bash
# Cloud Hypervisor automatically uses UEFI when --kernel is omitted
cloud-hypervisor \
    --disk path=/var/lib/cocoon/vms/vm-123/overlay.qcow2 \
    --cpus boot=2 \
    --memory size=2G \
    --serial file=/var/log/cocoon/vm-123.log \
    --console off

# Cloud Hypervisor will search for OVMF firmware at standard system paths
```

**Note**: When `--kernel` is NOT specified, Cloud Hypervisor automatically enters UEFI boot mode and searches for OVMF firmware at standard system locations.

### Firmware Selection Guide

| Boot Method | Firmware | Boot Time | OS Support | Phase |
|-------------|----------|-----------|------------|-------|
| **PVH (Primary)** | rust-hypervisor-firmware | <100ms | Ubuntu Cloud, Fedora Cloud, Debian Cloud | Phase 1 ✅ |
| **UEFI (Fallback)** | OVMF / edk2-ovmf | ~500ms | All Linux distributions | Phase 1 ✅ |

**Cocoon Default Strategy** (per Boot Contract v2.0):
1. Try PVH boot first (faster, cloud-native)
2. Automatic fallback to UEFI on failure
3. User can force UEFI with `--boot-mode uefi`

### Firmware Updates

**Update rust-hypervisor-firmware:**

```bash
# Check current version
ls -lh /var/lib/cocoon/firmware/

# Download latest release
LATEST_VERSION="0.4.2"  # Check GitHub for latest
curl -L https://github.com/cloud-hypervisor/rust-hypervisor-firmware/releases/download/${LATEST_VERSION}/hypervisor-fw \
    -o /tmp/hypervisor-fw-${LATEST_VERSION}

# Backup current firmware
sudo cp /var/lib/cocoon/firmware/hypervisor-fw /var/lib/cocoon/firmware/hypervisor-fw.backup

# Install new version
sudo mv /tmp/hypervisor-fw-${LATEST_VERSION} /var/lib/cocoon/firmware/hypervisor-fw
sudo chmod +x /var/lib/cocoon/firmware/hypervisor-fw

# Verify
sha256sum /var/lib/cocoon/firmware/hypervisor-fw
```

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
│   ├── hypervisor-fw              # PVH firmware (primary)
│   ├── hypervisor-fw-0.4.2        # Versioned backup
│   └── checksums.txt              # SHA256 verification
├── cache/
│   └── images/                    # Base image cache (qcow2)
│       ├── ubuntu-22.04-abc123.qcow2
│       └── fedora-38-def456.qcow2
├── vms/                           # VM instances
│   ├── vm-abc-123/
│   │   ├── overlay.qcow2          # COW overlay disk
│   │   ├── config.json            # VM configuration
│   │   └── metadata.json          # VM metadata
│   └── vm-def-456/
└── temp/                          # Temporary conversion files

/var/log/cocoon/                   # Logs
├── vm-abc-123.log                 # Serial console logs (per-VM)
└── vm-def-456.log

/run/cocoon/                       # Runtime sockets
└── vms/
    ├── vm-abc-123/
    │   ├── ch.sock                # Cloud Hypervisor API socket
    │   └── ch.pid                 # Process ID file
    └── vm-def-456/

/etc/cocoon/                       # Configuration (optional)
└── config.yaml                    # Global Cocoon config
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
# Example: Launch with PVH firmware
# Note: This requires a bootable disk with kernel/bootloader
cloud-hypervisor \
    --kernel /var/lib/cocoon/firmware/hypervisor-fw \
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
# Check PVH firmware installation
ls -l /var/lib/cocoon/firmware/hypervisor-fw

# If missing, download it
curl -L https://github.com/cloud-hypervisor/rust-hypervisor-firmware/releases/download/0.4.2/hypervisor-fw \
    -o /tmp/hypervisor-fw
sudo mv /tmp/hypervisor-fw /var/lib/cocoon/firmware/
sudo chmod +x /var/lib/cocoon/firmware/hypervisor-fw

# Or install UEFI firmware (OVMF)
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
- rust-hypervisor-firmware: https://github.com/cloud-hypervisor/rust-hypervisor-firmware
- KVM Documentation: https://www.linux-kvm.org/

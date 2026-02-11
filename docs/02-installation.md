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

Cloud Hypervisor requires firmware to boot virtual machines. Two firmware types are supported:

#### UEFI Firmware (Recommended)

**What is UEFI firmware?**
- UEFI (Unified Extensible Firmware Interface) provides a modern boot environment
- Supports secure boot, GPT partitions, and larger disk sizes
- Required for booting standard Linux distributions (Ubuntu, Fedora, etc.)

**Download OVMF (Open Virtual Machine Firmware):**

```bash
# Ubuntu/Debian (install via package manager)
sudo apt-get install -y ovmf

# Firmware files will be located at:
# - /usr/share/OVMF/OVMF_CODE.fd
# - /usr/share/OVMF/OVMF_VARS.fd
```

**Manual download (for other distributions):**

```bash
# Create firmware directory
sudo mkdir -p /etc/cocoon/firmware

# Download OVMF firmware from upstream
curl -L https://github.com/cloud-hypervisor/rust-hypervisor-firmware/releases/download/0.4.2/hypervisor-fw -o /tmp/hypervisor-fw
sudo mv /tmp/hypervisor-fw /etc/cocoon/firmware/

# Make executable
sudo chmod +x /etc/cocoon/firmware/hypervisor-fw
```

#### PVH Firmware (Alternative)

**What is PVH?**
- PVH (Paravirtualized Hardware) is a lighter-weight boot protocol
- Faster boot times compared to UEFI
- Limited OS support (requires kernel built with PVH support)

**Download rust-hypervisor-firmware:**

```bash
# Download latest PVH firmware
curl -L https://github.com/cloud-hypervisor/rust-hypervisor-firmware/releases/download/0.4.2/hypervisor-fw -o /tmp/hypervisor-fw

# Install to firmware directory
sudo mkdir -p /etc/cocoon/firmware
sudo mv /tmp/hypervisor-fw /etc/cocoon/firmware/
sudo chmod +x /etc/cocoon/firmware/hypervisor-fw
```

### Firmware Selection Guide

| Boot Method | Firmware Type | Use Case | Boot Time | OS Support |
|-------------|--------------|----------|-----------|------------|
| UEFI | OVMF | Standard Linux distributions | ~500ms | Excellent |
| PVH | rust-hypervisor-firmware | Custom minimal kernels | <100ms | Limited |

**Recommendation:** Use UEFI firmware (OVMF) unless you have specific requirements for ultra-fast boot times and are using a PVH-capable kernel.

### Firmware Configuration

**For UEFI boot, specify firmware in VM launch:**

```bash
cloud-hypervisor \
    --kernel /etc/cocoon/firmware/OVMF_CODE.fd \
    --disk path=/srv/cocoon/images/ubuntu.qcow2
```

**For PVH boot with custom kernel:**

```bash
cloud-hypervisor \
    --kernel /srv/cocoon/kernels/vmlinux \
    --disk path=/srv/cocoon/images/rootfs.ext4
```

### Firmware Updates

Firmware should be updated periodically for security patches and bug fixes:

```bash
# Update OVMF via package manager (Ubuntu/Debian)
sudo apt-get update && sudo apt-get upgrade ovmf

# Manual update of rust-hypervisor-firmware
curl -L https://github.com/cloud-hypervisor/rust-hypervisor-firmware/releases/latest/download/hypervisor-fw \
    -o /etc/cocoon/firmware/hypervisor-fw.new

# Backup old firmware
cp /etc/cocoon/firmware/hypervisor-fw /etc/cocoon/firmware/hypervisor-fw.backup

# Replace with new firmware
mv /etc/cocoon/firmware/hypervisor-fw.new /etc/cocoon/firmware/hypervisor-fw
chmod +x /etc/cocoon/firmware/hypervisor-fw
```

## Directory Structure

### Recommended Structure

Recommended directory structure for Cocoon deployment:

```
/etc/cocoon/                      # Configuration directory
├── firmware/
│   ├── hypervisor-fw              # PVH firmware
│   ├── OVMF_CODE.fd              # UEFI firmware (code)
│   └── OVMF_VARS.fd              # UEFI firmware (variables)
└── config.toml                    # Cocoon configuration

/srv/cocoon/                       # Runtime directory
├── images/
│   ├── base-images/              # Read-only base images
│   │   ├── ubuntu-22.04.qcow2
│   │   └── python-3.11.qcow2
│   └── active/                   # Currently running VM images
├── kernels/
│   └── vmlinux-5.15              # Custom VM kernels
└── logs/
    ├── vm-logs/                  # Individual VM logs
    └── hypervisor-logs/          # Cloud Hypervisor logs
```

**Setup commands:**

```bash
# Create configuration directory
sudo mkdir -p /etc/cocoon/firmware

# Create runtime directories
sudo mkdir -p /srv/cocoon/images/{base-images,active}
sudo mkdir -p /srv/cocoon/kernels
sudo mkdir -p /srv/cocoon/logs/{vm-logs,hypervisor-logs}

# Set appropriate permissions
sudo chown -R $USER:$USER /srv/cocoon
sudo chmod -R 755 /srv/cocoon
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
# Example: Launch with PVH firmware and minimal kernel
# Note: This requires a PVH-capable kernel image
cloud-hypervisor \
    --kernel /etc/cocoon/firmware/hypervisor-fw \
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
sudo chmod 644 /etc/cocoon/firmware/*

# VM images (read-write for owner only)
chmod 600 /srv/cocoon/images/*.qcow2

# Logs (read-only for owner)
chmod 644 /srv/cocoon/logs/*.log
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
# Ensure firmware is installed
ls -l /etc/cocoon/firmware/

# Install OVMF if missing
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

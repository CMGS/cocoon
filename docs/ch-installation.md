# Cloud Hypervisor Installation and Deployment

## Overview

Cloud Hypervisor is a production-grade Virtual Machine Monitor (VMM) built in Rust, designed for modern cloud workloads. This section provides comprehensive guidance on installing and deploying Cloud Hypervisor for AI Agent sandbox environments.

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

### Build Dependencies (Source Installation)

If building from source, install the following dependencies:

**Ubuntu/Debian:**

```bash
sudo apt-get update
sudo apt-get install -y \
    build-essential \
    git \
    curl \
    pkg-config \
    libssl-dev \
    libcap-ng-dev \
    libseccomp-dev
```

**Fedora/RHEL/CentOS:**

```bash
sudo dnf install -y \
    gcc \
    git \
    curl \
    pkg-config \
    openssl-devel \
    libcap-ng-devel \
    libseccomp-devel
```

### Rust Toolchain

Cloud Hypervisor is written in Rust. Install the latest stable Rust toolchain:

```bash
# Install rustup (Rust installer)
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh

# Configure current shell
source $HOME/.cargo/env

# Verify installation
rustc --version
cargo --version
```

**Required Rust version:** 1.70.0 or later

### Runtime Dependencies

Cloud Hypervisor requires the following runtime components:

**1. QEMU utilities (for disk image management):**

```bash
# Ubuntu/Debian
sudo apt-get install -y qemu-utils

# Fedora/RHEL
sudo dnf install -y qemu-img
```

**2. TAP networking tools (for VM network connectivity):**

```bash
# Ubuntu/Debian
sudo apt-get install -y bridge-utils iproute2

# Fedora/RHEL
sudo dnf install -y bridge-utils iproute
```

**3. Optional: virtiofsd (for shared filesystem access):**

```bash
# Ubuntu/Debian
sudo apt-get install -y virtiofsd

# Fedora/RHEL
sudo dnf install -y virtiofsd
```

## Installation Methods

### Method 1: Pre-built Releases (Recommended)

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

### Method 2: Source Compilation

Building from source provides the latest features and allows customization.

**1. Clone the repository:**

```bash
git clone https://github.com/cloud-hypervisor/cloud-hypervisor.git
cd cloud-hypervisor
```

**2. Checkout stable version (recommended):**

```bash
# List available tags
git tag -l

# Checkout specific version
git checkout v38.0
```

**3. Build the project:**

```bash
# Build release version with optimizations
cargo build --release

# Binary will be located at: target/release/cloud-hypervisor
```

**4. Optional: Build with additional features:**

```bash
# Build with TDX (Intel Trust Domain Extensions) support
cargo build --release --features tdx

# Build with SEV-SNP (AMD Secure Encrypted Virtualization) support
cargo build --release --features sev_snp

# Build with all features
cargo build --release --all-features
```

**5. Install the binary:**

```bash
sudo cp target/release/cloud-hypervisor /usr/local/bin/
sudo chmod +x /usr/local/bin/cloud-hypervisor
```

**6. Verify installation:**

```bash
cloud-hypervisor --version
```

## Runtime Dependencies

### Firmware Requirements

Cloud Hypervisor requires firmware to boot virtual machines. Two firmware types are supported:

#### 1. UEFI Firmware (Recommended)

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
sudo mkdir -p /opt/cloud-hypervisor/firmware

# Download OVMF firmware from upstream
curl -L https://github.com/cloud-hypervisor/rust-hypervisor-firmware/releases/download/0.4.2/hypervisor-fw -o /tmp/hypervisor-fw
sudo mv /tmp/hypervisor-fw /opt/cloud-hypervisor/firmware/

# Make executable
sudo chmod +x /opt/cloud-hypervisor/firmware/hypervisor-fw
```

#### 2. PVH Firmware (Alternative)

**What is PVH?**
- PVH (Paravirtualized Hardware) is a lighter-weight boot protocol
- Faster boot times compared to UEFI
- Limited OS support (requires kernel built with PVH support)

**Download rust-hypervisor-firmware:**

```bash
# Download latest PVH firmware
curl -L https://github.com/cloud-hypervisor/rust-hypervisor-firmware/releases/download/0.4.2/hypervisor-fw -o /tmp/hypervisor-fw

# Install to firmware directory
sudo mkdir -p /opt/cloud-hypervisor/firmware
sudo mv /tmp/hypervisor-fw /opt/cloud-hypervisor/firmware/
sudo chmod +x /opt/cloud-hypervisor/firmware/hypervisor-fw
```

### Firmware Selection Guide

| Boot Method | Firmware Type | Use Case | Boot Time | OS Support |
|-------------|--------------|----------|-----------|------------|
| UEFI | OVMF | Standard Linux distributions | ~500ms | Excellent |
| PVH | rust-hypervisor-firmware | Custom minimal kernels | <100ms | Limited |

**Recommendation:** Use UEFI firmware (OVMF) unless you have specific requirements for ultra-fast boot times and are using a PVH-capable kernel.

## Firmware Handling

### Firmware Storage Structure

Recommended directory structure for Cloud Hypervisor firmware and resources:

```
/opt/cloud-hypervisor/
├── firmware/
│   ├── hypervisor-fw              # PVH firmware
│   ├── OVMF_CODE.fd              # UEFI firmware (code)
│   └── OVMF_VARS.fd              # UEFI firmware (variables)
├── images/
│   ├── disk1.raw                  # VM disk images
│   └── rootfs.ext4
└── kernels/
    └── vmlinux-5.15              # Custom VM kernels
```

**Create the directory structure:**

```bash
sudo mkdir -p /opt/cloud-hypervisor/{firmware,images,kernels}
sudo chown -R $USER:$USER /opt/cloud-hypervisor
```

### Firmware Configuration

**For UEFI boot, specify firmware in VM launch:**

```bash
cloud-hypervisor \
    --kernel /opt/cloud-hypervisor/firmware/OVMF_CODE.fd \
    --disk path=/opt/cloud-hypervisor/images/ubuntu.qcow2
```

**For PVH boot with custom kernel:**

```bash
cloud-hypervisor \
    --kernel /opt/cloud-hypervisor/kernels/vmlinux \
    --disk path=/opt/cloud-hypervisor/images/rootfs.ext4
```

### Firmware Updates

Firmware should be updated periodically for security patches and bug fixes:

```bash
# Update OVMF via package manager (Ubuntu/Debian)
sudo apt-get update && sudo apt-get upgrade ovmf

# Manual update of rust-hypervisor-firmware
curl -L https://github.com/cloud-hypervisor/rust-hypervisor-firmware/releases/latest/download/hypervisor-fw \
    -o /opt/cloud-hypervisor/firmware/hypervisor-fw.new

# Backup old firmware
cp /opt/cloud-hypervisor/firmware/hypervisor-fw /opt/cloud-hypervisor/firmware/hypervisor-fw.backup

# Replace with new firmware
mv /opt/cloud-hypervisor/firmware/hypervisor-fw.new /opt/cloud-hypervisor/firmware/hypervisor-fw
chmod +x /opt/cloud-hypervisor/firmware/hypervisor-fw
```

## Verification Steps

### Basic Functionality Test

**1. Verify Cloud Hypervisor installation:**

```bash
cloud-hypervisor --version
# Expected output: cloud-hypervisor v38.0
```

**2. Check KVM access:**

```bash
cloud-hypervisor --version
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
    --kernel /opt/cloud-hypervisor/firmware/hypervisor-fw \
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

**1. Test REST API:**

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

**2. Verify performance:**

```bash
# Test VM boot time (requires full disk image)
time cloud-hypervisor \
    --kernel /boot/vmlinuz-$(uname -r) \
    --disk path=/opt/cloud-hypervisor/images/test.raw \
    --cpus boot=2 \
    --memory size=1G \
    --console off

# Expected boot time: <100ms for VM creation
```

## Directory Structure Recommendations

### System-wide Installation

For production deployments on a single system:

```
/opt/cloud-hypervisor/           # Main installation directory
├── bin/
│   └── cloud-hypervisor          # Symlink to /usr/local/bin/cloud-hypervisor
├── firmware/
│   ├── OVMF_CODE.fd
│   ├── OVMF_VARS.fd
│   └── hypervisor-fw
├── images/                       # VM disk images
│   ├── ubuntu-22.04.qcow2
│   └── minimal-rootfs.raw
├── kernels/                      # Custom kernels
│   └── vmlinuz-5.15
├── configs/                      # VM configuration files
│   ├── default.json
│   └── ai-sandbox.json
└── logs/                         # VM execution logs
    └── vm-*.log
```

**Setup commands:**

```bash
sudo mkdir -p /opt/cloud-hypervisor/{bin,firmware,images,kernels,configs,logs}
sudo ln -s /usr/local/bin/cloud-hypervisor /opt/cloud-hypervisor/bin/
sudo chown -R $USER:$USER /opt/cloud-hypervisor
```

### Multi-user/Multi-tenant Setup

For shared systems with multiple users:

```
/var/lib/cloud-hypervisor/       # System-wide resources
├── firmware/                     # Shared firmware (read-only)
└── images/                       # Shared base images (read-only)

/home/<user>/.local/cloud-hypervisor/
├── images/                       # User-specific images
├── configs/                      # User VM configs
└── logs/                         # User VM logs
```

**Setup commands:**

```bash
# System-wide (as root)
sudo mkdir -p /var/lib/cloud-hypervisor/{firmware,images}
sudo chmod 755 /var/lib/cloud-hypervisor

# User-specific
mkdir -p ~/.local/cloud-hypervisor/{images,configs,logs}
```

### AI Agent Sandbox Setup

Recommended structure for AI agent sandbox deployments:

```
/srv/ai-sandbox/
├── cloud-hypervisor/
│   ├── firmware/                 # UEFI/PVH firmware
│   ├── base-images/              # Read-only base images
│   │   ├── ubuntu-22.04.qcow2
│   │   └── python-3.11.qcow2
│   └── kernels/
├── runtime/
│   ├── active/                   # Currently running VM images
│   ├── templates/                # VM configuration templates
│   └── snapshots/                # VM snapshots (if used)
└── logs/
    ├── vm-logs/                  # Individual VM logs
    └── hypervisor-logs/          # Cloud Hypervisor logs
```

**Setup commands:**

```bash
sudo mkdir -p /srv/ai-sandbox/cloud-hypervisor/{firmware,base-images,kernels}
sudo mkdir -p /srv/ai-sandbox/runtime/{active,templates,snapshots}
sudo mkdir -p /srv/ai-sandbox/logs/{vm-logs,hypervisor-logs}

# Set appropriate permissions
sudo chown -R ai-agent:ai-agent /srv/ai-sandbox
sudo chmod -R 755 /srv/ai-sandbox
```

## Security Considerations

### File Permissions

Set appropriate permissions for sensitive components:

```bash
# Firmware (read-only for users)
sudo chmod 644 /opt/cloud-hypervisor/firmware/*

# VM images (read-write for owner only)
chmod 600 /opt/cloud-hypervisor/images/*.qcow2

# Logs (read-only for owner)
chmod 644 /opt/cloud-hypervisor/logs/*.log
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
ls -l /opt/cloud-hypervisor/firmware/

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

1. **Image Management**: Set up OCI image handling and qcow2 disk management (see next section)
2. **CLI Development**: Create wrapper scripts or CLI tools for simplified VM management
3. **Network Configuration**: Configure TAP devices and bridge networking for VM connectivity
4. **Monitoring**: Set up logging and monitoring for VM instances
5. **Integration**: Integrate Cloud Hypervisor with your AI agent sandbox framework

## References

- Cloud Hypervisor GitHub: https://github.com/cloud-hypervisor/cloud-hypervisor
- Official Documentation: https://www.cloudhypervisor.org/
- rust-hypervisor-firmware: https://github.com/cloud-hypervisor/rust-hypervisor-firmware
- KVM Documentation: https://www.linux-kvm.org/

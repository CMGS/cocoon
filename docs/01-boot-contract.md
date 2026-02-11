# Boot Contract Specification

**Version**: 2.0
**Status**: Draft
**Priority**: P0 - CRITICAL FOUNDATION DOCUMENT

## Executive Summary

This document defines the **Boot Contract** - the core specification for how Cocoon boots virtual machines using Cloud Hypervisor. The contract establishes:

1. **Dual boot mode strategy**: PVH (primary) + UEFI (fallback)
2. **Guest initialization**: systemd + cloud-init with metadata server
3. **I/O mechanisms**: Serial console, future vsock/virtiofs
4. **Lifecycle semantics**: Start, stop, delete, crash recovery
5. **Image requirements**: What constitutes a bootable image

## Table of Contents

1. [Boot Mode Strategy](#1-boot-mode-strategy)
2. [Guest Init Model](#2-guest-init-model)
3. [I/O Mechanisms](#3-io-mechanisms)
4. [Lifecycle Semantics](#4-lifecycle-semantics)
5. [VM Configuration Schema](#5-vm-configuration-schema)
6. [Image Requirements](#6-image-requirements)
7. [Implementation Checklist](#7-implementation-checklist)

---

## 1. Boot Mode Strategy

### 1.1 Primary Boot Mode: PVH + hypervisor-fw

**Selected Approach**: **PVH boot with rust-hypervisor-firmware** as the primary boot method.

**Rationale**:
- ✅ **Fast boot**: Sub-100ms boot time (vs 500ms+ for UEFI)
- ✅ **Lightweight**: Minimal firmware footprint (~100KB vs 2MB OVMF)
- ✅ **Cloud-native**: Designed specifically for Cloud Hypervisor
- ✅ **Standard cloud images**: Works with Ubuntu Cloud, Fedora Cloud, etc.
- ✅ **Disk-based boot**: Loads kernel from GPT+ESP like standard cloud VMs

**How it works**:
```bash
cloud-hypervisor \
  --firmware /var/lib/cocoon/firmware/hypervisor-fw \
  --disk path=/var/lib/cocoon/vms/vm-123/overlay.qcow2 \
  --cpus boot=2 \
  --memory size=2G \
  --serial file=/var/log/cocoon/vm-123.log \
  --console off
```

**Parameter Choice**:
- `--firmware` is the recommended way to load hypervisor-fw (architecture-specific firmware loading)
- `--kernel` also works because hypervisor-fw has a PVH entry point, but `--firmware` is semantically correct
- Cloud Hypervisor documentation: "Use --firmware for firmware loading"

**What hypervisor-fw does**:
1. Boots via PVH entry point (Xen PVH protocol)
2. Discovers virtio-blk disk devices
3. Parses GPT partition table → finds EFI System Partition (ESP)
4. Mounts ESP (FAT32 filesystem)
5. Reads Boot Loader Specification (BLS) entries
6. Loads GRUB2/shim or direct kernel from ESP
7. Transfers control to bootloader/kernel

**Firmware Management**:
```bash
# Installation downloads firmware
cocoon doctor  # Checks if firmware exists, downloads if missing

# Manual firmware management
cocoon firmware list      # Show installed firmware versions
cocoon firmware update    # Update to latest release
cocoon firmware verify    # Verify integrity
```

**Firmware Location**:
```
/var/lib/cocoon/firmware/
├── hypervisor-fw         # x86_64 PVH firmware (latest)
├── hypervisor-fw-0.4.2   # Versioned backup
└── checksums.txt         # SHA256 verification
```

**Architecture Support**:
| Arch | Firmware | Status |
|------|----------|--------|
| x86_64 | rust-hypervisor-firmware | ✅ Phase 1 |
| aarch64 | rust-hypervisor-firmware (ARM64 build) | 📋 Phase 2 |

---

### 1.2 Fallback Boot Mode: UEFI + OVMF

**When to use UEFI fallback**:
1. Image explicitly requests UEFI (metadata flag)
2. hypervisor-fw boot fails (automatic retry)
3. User specifies `--boot-mode uefi` flag

**UEFI boot command**:
```bash
cloud-hypervisor \
  # Note: Omit both --firmware and --kernel to trigger UEFI boot
  --disk path=/var/lib/cocoon/vms/vm-123/overlay.qcow2 \
  --cpus boot=2 \
  --memory size=2G \
  --serial file=/var/log/cocoon/vm-123.log \
  --console off
```

**Firmware Requirements**:
- **x86_64**: `/usr/share/OVMF/OVMF_CODE.fd` (from `ovmf` package)
- **aarch64**: `/usr/share/AAVMF/AAVMF_CODE.fd` (from `edk2-aarch64` package)
- Cloud Hypervisor automatically detects system-installed UEFI firmware at standard paths

**How UEFI fallback works**:
1. Cocoon omits both `--firmware` and `--kernel` parameters
2. Cloud Hypervisor enters UEFI boot mode
3. CH searches for OVMF/AAVMF at standard system paths
4. UEFI firmware loads GRUB from ESP → boots kernel

---

### 1.3 Boot Mode Selection Logic

```go
type BootMode string

const (
    BootModePVH  BootMode = "pvh"   // Primary: hypervisor-fw
    BootModeUEFI BootMode = "uefi"  // Fallback: OVMF
)

func SelectBootMode(image *Image, userPreference BootMode) BootMode {
    // 1. User explicitly requested UEFI
    if userPreference == BootModeUEFI {
        return BootModeUEFI
    }

    // 2. Image metadata requires UEFI (e.g., secure boot)
    if image.Metadata.RequiresUEFI {
        return BootModeUEFI
    }

    // 3. Default: PVH with hypervisor-fw
    return BootModePVH
}
```

**Automatic fallback on failure**:
```go
func BootVM(vmID string, mode BootMode) error {
    if mode == BootModePVH {
        err := bootWithPVH(vmID)
        if err != nil {
            log.Warn("PVH boot failed, falling back to UEFI: %v", err)
            return bootWithUEFI(vmID)
        }
        return nil
    }
    return bootWithUEFI(vmID)
}
```

---

## 2. Guest Init Model

### 2.1 Init System: systemd

**Required**: All bootable images MUST use systemd as init system.

**Why systemd**:
- ✅ Universal: Ubuntu, Fedora, Debian, RHEL all use systemd
- ✅ cloud-init integration: Native support via systemd units
- ✅ Service management: Easy to inject and monitor agent tasks
- ✅ Logging: journald provides structured logging

**Verification**:
```bash
# Check if image has systemd
ls -la /sbin/init  # Should be symlink to systemd
```

---

### 2.2 VM Initialization: cloud-init via Metadata Server

**Purpose**: cloud-init is used for **VM initialization only** - setting up users, SSH keys, hostname, and network configuration. It is NOT used for task orchestration or command execution.

**Architecture**:
```
┌─────────────┐
│   Cocoon    │  1. Start metadata server (HTTP)
│    Host     │  2. Launch VM with cloud-init cmdline
└──────┬──────┘
       │ :8080
       │
       ▼
┌─────────────┐
│  Guest VM   │  3. cloud-init fetches meta-data/user-data
│             │  4. Configures users, SSH, hostname, network
│ cloud-init  │  5. VM ready for external access
└─────────────┘
```

**cloud-init Datasource Configuration**:

Images MUST have cloud-init configured to use NoCloud-Net datasource:

```yaml
# /etc/cloud/cloud.cfg.d/99-cocoon.cfg
datasource_list: [ NoCloud, NoCloudNet ]
datasource:
  NoCloudNet:
    seedfrom: http://169.254.169.254/
```

**Kernel Cmdline Injection**:

During image conversion or at boot time, Cocoon configures the kernel cmdline:

```bash
# GRUB configuration (modified during conversion)
GRUB_CMDLINE_LINUX="... ds=nocloud-net;seedfrom=http://169.254.169.254/"
```

**Metadata Server Endpoints**:

Cocoon runs an EC2-compatible metadata server on the host:

```
GET http://169.254.169.254/meta-data/instance-id
→ vm-abc-123

GET http://169.254.169.254/meta-data/hostname
→ vm-abc-123.cocoon.local

GET http://169.254.169.254/user-data
→ #cloud-config
  users:
    - name: cocoon
      sudo: ALL=(ALL) NOPASSWD:ALL
      ssh_authorized_keys:
        - ssh-rsa AAAAB3...
```

**Implementation Strategy**:

**Phase 1: Stub Metadata Server (MVP)**
- Lightweight HTTP server listening on 169.254.169.254:80
- Per-VM metadata isolation (keyed by VM IP or request context)
- EC2-compatible endpoints:
  - `/meta-data/instance-id` → VM ID
  - `/meta-data/hostname` → VM hostname
  - `/meta-data/public-keys/` → SSH public keys (optional)
  - `/user-data` → cloud-config for user/SSH setup
- No authentication required (local host only)
- Starts automatically when VM starts, stops when VM stops

**NOT Included in Phase 1**:
- ❌ cloud-init ISO (no disk mounting for cloud-init)
- ❌ Advanced metadata: network config, block device mapping
- ❌ IMDSv2 authentication (AWS-style token-based auth)

**Phase 2: Full Metadata Server**
- Network configuration injection
- Block device mapping
- IMDSv2 authentication for security

---

### 2.3 VM Boot Sequence

```
1. Cocoon starts metadata server
   └─ Listens on 169.254.169.254:80

2. Cocoon launches VM with modified cmdline
   └─ ds=nocloud-net;seedfrom=http://169.254.169.254/

3. VM boots → systemd starts cloud-init.service

4. cloud-init fetches metadata from server
   └─ GET http://169.254.169.254/meta-data/*
   └─ GET http://169.254.169.254/user-data

5. cloud-init configures VM
   └─ Create users, set hostname, configure SSH keys

6. VM initialization complete
   └─ VM is ready for external access (console, SSH, API)

7. Upper layer can now orchestrate via API
   └─ External RPC/gRPC can attach, send files, run commands
```

---

## 3. I/O Mechanisms

### 3.1 Serial Console (Boot and Debug Logs)

**Purpose**:
- Boot messages capture
- Kernel/systemd logs
- cloud-init initialization logs
- Error diagnostics and debugging

**Configuration**:
```bash
cloud-hypervisor \
  --serial file=/var/log/cocoon/vm-123.log \
  --console off
```

**Serial Log Format**:
```
[    0.123456] Linux version 5.15.0-87-generic ...
[    0.234567] Command line: BOOT_IMAGE=/vmlinuz root=/dev/vda1 ds=nocloud-net;...
[    1.456789] cloud-init[234]: Cloud-init v. 23.3.1 running ...
[    2.567890] cloud-init[234]: Fetching user-data from http://169.254.169.254/
[    3.678901] cloud-init[234]: Creating user 'cocoon'
[    4.789012] cloud-init[234]: Setting hostname to 'vm-abc-123'
[    5.890123] systemd[1]: Reached target Multi-User System
[    6.901234] systemd[1]: Reached target Graphical Interface
```

**Boot Completion Detection**:
```go
func WaitForBootCompletion(vmID string, timeout time.Duration) error {
    logPath := fmt.Sprintf("/var/log/cocoon/%s.log", vmID)

    ctx, cancel := context.WithTimeout(context.Background(), timeout)
    defer cancel()

    // Tail serial log for boot completion marker
    for line := range tailFile(ctx, logPath) {
        // Look for systemd multi-user target (VM is ready)
        if strings.Contains(line, "Reached target Multi-User System") {
            return nil
        }
    }

    return fmt.Errorf("boot timeout exceeded")
}
```

---

### 3.2 Future I/O Mechanisms (Phase 2)

**vsock** (VM sockets):
- Low-latency host-guest communication
- No network configuration needed
- Ideal for streaming large outputs

**virtiofs** (Shared filesystem):
- Mount host directories into guest
- Direct file I/O without copying
- Large dataset access

---

## 4. Lifecycle Semantics

### 4.1 VM States

```
CREATING → CREATED → STARTING → RUNNING → STOPPING → STOPPED
                                     ↓
                                  ERROR
```

See `docs/07-vm-lifecycle.md` for complete state machine.

---

### 4.2 Graceful Shutdown

**Step 1: ACPI Power Button (PVH mode)**

Cloud Hypervisor's ACPI support in PVH mode:
```bash
# Send ACPI power button event via API
curl -X PUT http://localhost/api/v1/vm.power-button \
  --unix-socket /run/cocoon/vms/vm-123/ch.sock
```

systemd receives ACPI event → triggers `systemd poweroff`

**Step 2: Timeout + Force Kill**

If VM doesn't shutdown within 30 seconds:
```go
func StopVM(vmID string) error {
    // Step 1: ACPI shutdown
    client.PowerButton()

    // Step 2: Wait with timeout
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()

    select {
    case <-waitForExit(vmID):
        return nil  // Graceful shutdown succeeded
    case <-ctx.Done():
        // Step 3: Force kill
        return killCHProcess(vmID)
    }
}
```

---

### 4.3 Cleanup

**On normal shutdown**:
```bash
1. Stop metadata server for this VM
2. Send ACPI shutdown to VM
3. Wait for CH process exit
4. Clean up socket and PID files
```

**On crash**:
```bash
1. Detect crashed VM (PID not alive, state=RUNNING)
2. Clean up stale sockets
3. Mark state=ERROR in metadata
4. Preserve serial log for debugging
```

---

## 5. VM Configuration Schema

```go
type VMConfig struct {
    // Identity
    VMID        string `json:"vm_id"`
    Name        string `json:"name"`

    // Boot Configuration
    BootMode    BootMode `json:"boot_mode"`    // "pvh" or "uefi"
    Firmware    string   `json:"firmware"`     // hypervisor-fw path or ""

    // Disk
    RootDisk    string `json:"root_disk"`     // Overlay qcow2 path
    DiskSize    string `json:"disk_size"`     // e.g., "10G"

    // Resources
    CPUs        int   `json:"cpus"`
    Memory      int64 `json:"memory"`         // In bytes

    // Runtime
    VMSocket    string `json:"vm_socket"`     // /run/cocoon/vms/{vm-id}/ch.sock
    SerialLog   string `json:"serial_log"`    // /var/log/cocoon/{vm-id}.log
    PIDFile     string `json:"pid_file"`      // /run/cocoon/vms/{vm-id}/ch.pid

    // Initialization
    MetadataServer string `json:"metadata_server"` // http://169.254.169.254
    CloudInitISO   string `json:"cloud_init_iso"`  // cloud-init ISO path (Phase 2)

    // Timeouts
    BootTimeout time.Duration `json:"boot_timeout"` // Default: 60s
    StopTimeout time.Duration `json:"stop_timeout"` // Default: 30s
}
```

**Example Configuration**:
```json
{
  "vm_id": "vm-abc-123",
  "name": "ubuntu-vm-1",
  "boot_mode": "pvh",
  "firmware": "/var/lib/cocoon/firmware/hypervisor-fw",
  "root_disk": "/var/lib/cocoon/vms/vm-abc-123/overlay.qcow2",
  "disk_size": "10G",
  "cpus": 2,
  "memory": 2147483648,
  "vm_socket": "/run/cocoon/vms/vm-abc-123/ch.sock",
  "serial_log": "/var/log/cocoon/vm-abc-123.log",
  "pid_file": "/run/cocoon/vms/vm-abc-123/ch.pid",
  "metadata_server": "http://169.254.169.254",
  "cloud_init_iso": "",
  "boot_timeout": 60000000000,
  "stop_timeout": 30000000000
}
```

---

## 6. Image Requirements

### 6.1 Bootable Image Contract

An image is **bootable** if it satisfies these requirements:

**MUST Have (Mandatory)**:
1. ✅ **Kernel**: `/boot/vmlinuz*` (Linux kernel image)
2. ✅ **Initrd**: `/boot/initrd*` or `/boot/initramfs*` (initial ramdisk)
3. ✅ **Init System**: `/sbin/init` → systemd (not sysvinit)
4. ✅ **Bootloader**: GRUB2 in ESP (EFI System Partition)
   - **Path semantics**:
     - **ESP internal path**: `/EFI/BOOT/BOOTX64.EFI` (what bootloader sees)
     - **Mounted path**: `/boot/efi/EFI/BOOT/BOOTX64.EFI` (what rootfs sees after ESP mounted to /boot/efi)
5. ✅ **GPT + ESP**: EFI System Partition with FAT32 filesystem

**SHOULD Have (Recommended for VM Initialization)**:
- 🔵 **cloud-init**: `/usr/bin/cloud-init` + datasource config
  - **Purpose**: VM initialization (users, SSH keys, hostname, network)
  - **NOT mandatory**: Can boot without it, but Cocoon metadata server integration requires it
  - **For basic boot testing**: Not required

**Path Hierarchy Clarification**:
```
/dev/vda                         # Virtual disk
├── /dev/vda1 → ESP (FAT32)     # EFI System Partition
│   └── /EFI/BOOT/BOOTX64.EFI   # Bootloader (ESP internal path)
└── /dev/vda2 → / (ext4)        # Root filesystem
    └── /boot/efi/              # ESP mount point
        └── /EFI/BOOT/BOOTX64.EFI  # Same bootloader (mounted path)
```

**Validation Function** (Mandatory checks only):
```go
func ValidateBootability(rootfs string) error {
    // MUST checks
    checks := []struct {
        path    string
        message string
    }{
        {"/boot/vmlinuz*", "kernel not found"},
        {"/boot/initrd* or /boot/initramfs*", "initrd/initramfs not found"},
        {"/sbin/init", "init system not found"},
        {"/boot/efi/EFI", "EFI bootloader not found (no ESP partition)"},
    }

    for _, check := range checks {
        if !pathExists(filepath.Join(rootfs, check.path)) {
            return fmt.Errorf("bootability check failed: %s", check.message)
        }
    }

    // Verify init is systemd (mandatory for Cocoon)
    initTarget, _ := os.Readlink(filepath.Join(rootfs, "/sbin/init"))
    if !strings.Contains(initTarget, "systemd") {
        return fmt.Errorf("init system must be systemd, got: %s", initTarget)
    }

    // SHOULD check (warning, not error)
    if !pathExists(filepath.Join(rootfs, "/usr/bin/cloud-init")) {
        log.Warn("cloud-init not found - VM will boot but Cocoon metadata server integration disabled")
    }

    return nil
}
```

---

### 6.2 cloud-init Configuration Requirements

**MUST have cloud-init datasource config**:

```yaml
# /etc/cloud/cloud.cfg.d/99-cocoon.cfg
datasource_list: [ NoCloud, NoCloudNet ]
datasource:
  NoCloudNet:
    seedfrom: http://169.254.169.254/
```

**MUST have NoCloudNet support**:
```bash
# Verify cloud-init has NoCloudNet module
cloud-init query -l  # Should list NoCloudNet
```

**Conversion Process Injects**:

During OCI→qcow2 conversion, Cocoon adds:
```bash
# 1. Install cloud-init config
virt-customize -a image.qcow2 \
  --upload 99-cocoon.cfg:/etc/cloud/cloud.cfg.d/

# 2. Modify GRUB to add datasource cmdline
virt-customize -a image.qcow2 \
  --run-command "sed -i 's/GRUB_CMDLINE_LINUX=\"/GRUB_CMDLINE_LINUX=\"ds=nocloud-net;seedfrom=http:\/\/169.254.169.254\/ /' /etc/default/grub"

# 3. Regenerate GRUB config
virt-customize -a image.qcow2 \
  --run-command "grub2-mkconfig -o /boot/grub2/grub.cfg"
```

---

### 6.3 Architecture-Specific Requirements

#### x86_64

**Firmware**:
- PVH: `rust-hypervisor-firmware` (x86_64 build)
- UEFI: OVMF from `/usr/share/OVMF/OVMF_CODE.fd`

**Bootloader**:
- ESP location: `/boot/efi/EFI/BOOT/BOOTX64.EFI`
- GRUB target: `x86_64-efi`

#### aarch64 (Phase 2)

**Firmware**:
- PVH: `rust-hypervisor-firmware` (aarch64 build)
- UEFI: AAVMF from `/usr/share/AAVMF/AAVMF_CODE.fd`

**Bootloader**:
- ESP location: `/boot/efi/EFI/BOOT/BOOTAA64.EFI`
- GRUB target: `arm64-efi`

**Package Differences**:
| Component | x86_64 Package | aarch64 Package |
|-----------|----------------|-----------------|
| UEFI Firmware | `ovmf` | `edk2-aarch64` |
| GRUB | `grub2-efi-x64` | `grub2-efi-aa64` |
| Kernel | `linux-image-generic` | `linux-image-generic` |

---

## 7. Implementation Checklist

### Phase 1: Core Boot (P0)

- [ ] **Firmware Management**:
  - [ ] Download rust-hypervisor-firmware on install
  - [ ] Store in `/var/lib/cocoon/firmware/`
  - [ ] Implement `cocoon firmware` commands
  - [ ] Version management and updates

- [ ] **PVH Boot**:
  - [ ] Launch CH with `--firmware hypervisor-fw`
  - [ ] Verify firmware can boot standard cloud images
  - [ ] Test with Ubuntu Cloud, Fedora Cloud

- [ ] **UEFI Fallback**:
  - [ ] Detect OVMF firmware path at system locations
  - [ ] Launch CH without `--firmware` and `--kernel` parameters (CH auto-detects)
  - [ ] Automatic fallback on PVH failure

- [ ] **Metadata Server (Stub Implementation)**:
  - [ ] Implement lightweight HTTP server listening on 169.254.169.254:80
  - [ ] EC2-compatible endpoints:
    - [ ] `/meta-data/instance-id` → return VM ID
    - [ ] `/meta-data/hostname` → return VM hostname
    - [ ] `/meta-data/public-keys/` → return SSH keys (optional)
    - [ ] `/user-data` → return cloud-config YAML
  - [ ] Per-VM metadata isolation (keyed by request context or VM IP)
  - [ ] Start metadata server before launching VM
  - [ ] Stop metadata server when VM stops
  - [ ] Generate user-data with:
    - [ ] Default user creation
    - [ ] SSH key injection
    - [ ] Hostname configuration

- [ ] **Image Conversion**:
  - [ ] Validate cloud-init installed
  - [ ] Inject cloud-init datasource config
  - [ ] Modify GRUB cmdline for NoCloudNet
  - [ ] Regenerate GRUB config

- [ ] **VM Initialization**:
  - [ ] Generate user-data with users/SSH keys
  - [ ] Monitor serial log for boot completion
  - [ ] Detect systemd multi-user target reached

### Phase 2: Advanced Features (P1)

- [ ] **Direct Kernel Boot**:
  - [ ] Extract kernel/initrd from images
  - [ ] Use `--kernel` + `--initrd` + `--cmdline`
  - [ ] Benchmark vs hypervisor-fw boot time

- [ ] **Architecture Support**:
  - [ ] aarch64 firmware (rust-hypervisor-firmware ARM64)
  - [ ] AAVMF fallback
  - [ ] ARM64-specific GRUB config

- [ ] **Alternative I/O**:
  - [ ] vsock for task output streaming
  - [ ] virtiofs for large file access

---

## Summary

**Boot Contract v2.0** establishes:

1. ✅ **Dual boot strategy**: PVH primary + UEFI fallback
2. ✅ **Fast boot**: <100ms with hypervisor-fw
3. ✅ **VM initialization**: cloud-init + metadata server for setup (users, SSH, network)
4. ✅ **Image requirements**: kernel + bootloader + systemd + cloud-init
5. ✅ **Graceful lifecycle**: ACPI shutdown with timeout
6. ✅ **Production ready**: Works with standard cloud images

**Next Steps**:
- Read `docs/03-hypervisor-integration.md` for CH API details
- Read `docs/04-oci-conversion.md` for image conversion pipeline
- Read `docs/07-vm-lifecycle.md` for state machine specification

---

**End of Boot Contract v2.0**

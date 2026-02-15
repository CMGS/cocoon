# Boot Contract Specification

**Version**: 2.0
**Status**: Implemented
**Phase**: Phase 1
**Last Updated**: 2026-02-14

## Executive Summary

This document defines the **Boot Contract** - the core specification for how Cocoon boots virtual machines using Cloud Hypervisor. The contract establishes:

1. **Boot mode strategy**: UEFI (default for cloud images) + Direct kernel boot (for OCI VM images)
2. **Guest initialization**: systemd + cloud-init (NoCloud seed disks for initialization)
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

### 1.1 Default Boot Mode: UEFI + CLOUDHV.fd

**Selected Approach**: **UEFI boot with CLOUDHV.fd** (Cloud Hypervisor's edk2 firmware) as the default boot method.

**Rationale**:
- ✅ **Broadest compatibility**: Works with all Linux distributions and cloud images
- ✅ **CH recommended**: CLOUDHV.fd is the Cloud Hypervisor project's own edk2 build
- ✅ **Secure boot ready**: UEFI supports secure boot for production workloads
- ✅ **Standard cloud images**: Works with Ubuntu Cloud, Fedora Cloud, Debian Cloud, etc.
- ✅ **Reliable**: No kernel layout assumptions — UEFI boot path is well-tested

**How it works** (all config via REST `vm.create` payload, CLI only passes `--api-socket`):
```json
{
  "payload": {"firmware": "/var/lib/cocoon/firmware/CLOUDHV.fd"},
  "cpus": {"boot_vcpus": 2, "max_vcpus": 2},
  "memory": {"size": 2147483648},
  "disks": [{"path": "/var/lib/cocoon/vms/vm-123/overlay.qcow2"}],
  "serial": {"mode": "File", "file": "/var/log/cocoon/vm-123-serial.log"},
  "console": {"mode": "Off"}
}
```

**Firmware Management** (see [docs/09-cli-design.md](./09-cli-design.md) for authoritative CLI behavior):
- `cocoon init` creates the directory structure and configuration but does **not** automatically download firmware
- Install firmware via `cocoon firmware install` (recommended) or `cocoon init --with-uefi-firmware <URL>`
- Version controlled via `EDK2_CH_VERSION` environment variable (default: `a54f262b09`)
- URL: `https://github.com/cloud-hypervisor/edk2/releases/download/ch-{version}/CLOUDHV.fd`

### 1.2 Alternative Boot Mode: Direct Kernel Boot (OCI VM Images)

**Direct kernel boot** is used automatically when booting OCI VM images (via `--oci` flag). Instead of loading a UEFI firmware binary, Cocoon passes the kernel, initramfs, and cmdline directly to Cloud Hypervisor.

**When Direct kernel boot is used**:
- OCI VM images where the kernel and initramfs are extracted from the image
- Eliminates the need for a firmware binary entirely
- Provides fast, deterministic boot for purpose-built VM images

**How it works**:
1. Cocoon extracts the kernel and initramfs from the OCI VM image during conversion
2. Cloud Hypervisor is configured with `payload.kernel`, `payload.initramfs`, and `payload.cmdline` (no `payload.firmware`)
3. The kernel boots directly with the provided command line

**Cloud Hypervisor REST payload** (Direct kernel boot):
```json
{
  "payload": {
    "kernel": "/var/lib/cocoon/vms/vm-123/vmlinuz",
    "initramfs": "/var/lib/cocoon/vms/vm-123/initrd.img",
    "cmdline": "root=PARTUUID=<uuid> rw console=ttyS0,115200n8 console=hvc0"
  },
  "cpus": {"boot_vcpus": 2, "max_vcpus": 2},
  "memory": {"size": 2147483648},
  "disks": [{"path": "/var/lib/cocoon/vms/vm-123/overlay.qcow2"}],
  "serial": {"mode": "File", "file": "/var/log/cocoon/vm-123-serial.log"},
  "console": {"mode": "Off"}
}
```

**Kernel cmdline format**:
```
root=PARTUUID=<uuid> rw console=ttyS0,115200n8 console=hvc0
```

**Boot detection**: The same serial log pattern matching is used for both UEFI and Direct kernel boot (systemd target patterns, cloud-init patterns, fallback patterns).

**Firmware Location**:
```
/var/lib/cocoon/firmware/
├── CLOUDHV.fd            # UEFI firmware (default for cloud images)
└── (checksum metadata is optional; Cocoon Phase 1 does not read/manage it)
```

**Architecture Support**:
| Arch | Firmware | Status |
|------|----------|--------|
| x86_64 | CLOUDHV.fd (UEFI) / Direct kernel boot (OCI) | Phase 1 |
| aarch64 | CLOUDHV.fd (UEFI) | Phase 2 |

---

### 1.3 Boot Mode Selection Logic

Cocoon selects the boot mode automatically based on the image type:

- **Non-OCI images** (cloud images, local qcow2 files, URLs): **UEFI boot** with CLOUDHV.fd firmware via `payload.firmware`
- **OCI VM images** (created with `--oci` flag): **Direct kernel boot** via `payload.kernel` + `payload.initramfs` + `payload.cmdline`

The boot strategy is determined at VM creation time and stored immutably in `config.json`.

```go
// From types/boot.go:

type BootStrategy string

const (
    // BootStrategyUEFI boots with UEFI firmware (CLOUDHV.fd) via REST payload.firmware.
    // This is the default boot strategy for non-OCI images.
    BootStrategyUEFI   BootStrategy = "uefi"
    // BootStrategyDirect boots with kernel + initramfs + cmdline via REST payload.
    // Used automatically for OCI VM images.
    BootStrategyDirect BootStrategy = "direct"
)

// DefaultBootStrategy is the default boot strategy for new VMs (non-OCI images).
const DefaultBootStrategy = BootStrategyUEFI
```

**Boot mode matrix**:

| Image Type | Boot Strategy | Payload Fields | Firmware |
|------------|---------------|----------------|----------|
| Cloud images (qcow2, URL) | UEFI | `payload.firmware` | CLOUDHV.fd |
| OCI VM images (`--oci`) | Direct | `payload.kernel` + `payload.initramfs` + `payload.cmdline` | None |

**No automatic fallback**: Cocoon boots using the strategy determined by the image type. If the boot fails (e.g., firmware missing, kernel not found), the boot fails with an error.

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

### 2.2 VM Initialization: cloud-init

**Purpose**: cloud-init is used for **VM initialization only** - setting up users, SSH keys, hostname, and network configuration. It is NOT used for task orchestration or command execution.

**Architecture**:
```
┌─────────────┐
│   Cocoon    │  1. Attach NoCloud seed disk (if --network specified)
│    Host     │  2. Launch VM
└──────┬──────┘
       │
       ▼
┌─────────────┐
│  Guest VM   │  3. cloud-init reads NoCloud seed disk
│             │  4. Configures users, SSH, hostname, network
│ cloud-init  │  5. VM ready for external access
└─────────────┘
```

**cloud-init Datasource Configuration**:

**cloud-init: CONDITIONAL**
- **REQUIRED**: For VM initialization (SSH/user setup, hostname, network config via NoCloud)
- **OPTIONAL**: For standalone VMs with pre-configured credentials
- **DEFAULT**: Standard cloud images (Ubuntu Cloud, Fedora Cloud) include it by default
- **FALLBACK**: VMs without cloud-init will boot but cannot receive NoCloud configuration

---

### 2.3 VM Boot Sequence

```
1. Cocoon attaches NoCloud seed disk (if --network specified)

2. Cocoon launches VM

3. VM boots → systemd starts cloud-init.service

4. cloud-init reads NoCloud seed disk for network/meta-data

5. cloud-init configures VM
   └─ Create users, set hostname, configure SSH keys, apply network config

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
  --serial file=/var/log/cocoon/vm-123-serial.log \
  --console off
```

**Serial Log Format**:
```
[    0.123456] Linux version 5.15.0-87-generic ...
[    0.234567] Command line: BOOT_IMAGE=/vmlinuz root=/dev/vda1 ds=nocloud-net;...
[    1.456789] cloud-init[234]: Cloud-init v. 23.3.1 running ...
[    2.567890] cloud-init[234]: Reading NoCloud seed disk for user-data
[    3.678901] cloud-init[234]: Creating user 'cocoon'
[    4.789012] cloud-init[234]: Setting hostname to 'vm-abc-123'
[    5.890123] systemd[1]: Reached target Multi-User System
[    6.901234] systemd[1]: Reached target Graphical Interface
```

**Boot Completion Detection**:

Cocoon uses multi-pattern detection with fallback sequences to ensure robust boot detection across different Linux distributions and configurations.

**Detection Strategy**:
1. **Primary patterns**: Systemd target markers (multi-user or graphical)
2. **Cloud-init patterns**: Verify cloud-init completion (if enabled)
3. **Fallback patterns**: Login prompts, systemd startup finished messages
4. **Future enhancement**: cocoon-ready.service injection via user-data

**Implementation**:
```go
// BootDetectionConfig defines patterns for detecting boot completion
type BootDetectionConfig struct {
    // Systemd target patterns (any one indicates boot complete)
    SystemdTargetPatterns []string

    // Cloud-init completion patterns (optional, checked if cloud-init is enabled)
    CloudInitPatterns []string

    // Fallback patterns (used if primary patterns not found within timeout)
    FallbackPatterns []string

    // Timeout for boot detection
    Timeout time.Duration

    // Whether cloud-init is enabled for this VM
    CloudInitEnabled bool
}

// DefaultBootDetectionConfig returns the default boot detection configuration
func DefaultBootDetectionConfig() BootDetectionConfig {
    return BootDetectionConfig{
        // Systemd target patterns (ordered by priority)
        SystemdTargetPatterns: []string{
            "Reached target Multi-User System",        // Ubuntu, Debian, Fedora
            "Reached target Graphical Interface",      // Desktop images
            "multi-user.target: Startup finished",     // Alternative format
            "graphical.target: Startup finished",      // Desktop alternative
        },

        // Cloud-init completion patterns
        CloudInitPatterns: []string{
            "Cloud-init v.",                           // Generic version message
            "finished at",                             // cloud-init finish timestamp
            "cloud-init.target: Succeeded",            // systemd unit succeeded
        },

        // Fallback patterns (login prompt indicates boot complete)
        FallbackPatterns: []string{
            "login:",                                  // Login prompt
            "Welcome to",                              // Distribution welcome message
        },

        Timeout:          60 * time.Second,
        CloudInitEnabled: true,
    }
}

// BootCompletionState tracks detection state
type BootCompletionState struct {
    SystemdTargetReached bool
    CloudInitFinished    bool
    BootCompleteTime     time.Time
}

// WaitForBootCompletion waits for VM boot to complete with robust pattern detection
func WaitForBootCompletion(vmID string, config BootDetectionConfig) error {
    logPath := fmt.Sprintf("/var/log/cocoon/%s-serial.log", vmID)

    ctx, cancel := context.WithTimeout(context.Background(), config.Timeout)
    defer cancel()

    state := &BootCompletionState{
        SystemdTargetReached: false,
        CloudInitFinished:    !config.CloudInitEnabled, // Skip if disabled
    }

    // Tail serial log for boot completion markers
    for line := range tailFile(ctx, logPath) {
        // Check systemd target patterns
        if !state.SystemdTargetReached {
            for _, pattern := range config.SystemdTargetPatterns {
                if strings.Contains(line, pattern) {
                    log.Info("VM %s: Systemd target reached: %s", vmID, pattern)
                    state.SystemdTargetReached = true
                    break
                }
            }
        }

        // Check cloud-init completion patterns (if enabled)
        if config.CloudInitEnabled && !state.CloudInitFinished {
            for _, pattern := range config.CloudInitPatterns {
                if strings.Contains(line, pattern) {
                    log.Info("VM %s: cloud-init finished: %s", vmID, pattern)
                    state.CloudInitFinished = true
                    break
                }
            }
        }

        // Boot is complete when both conditions are met
        if state.SystemdTargetReached && state.CloudInitFinished {
            state.BootCompleteTime = time.Now()
            log.Info("VM %s: Boot completed successfully", vmID)
            return nil
        }

        // Fallback: check fallback patterns (only after timeout/2)
        if time.Since(ctx.Value("startTime").(time.Time)) > config.Timeout/2 {
            for _, pattern := range config.FallbackPatterns {
                if strings.Contains(line, pattern) {
                    log.Warn("VM %s: Boot detected via fallback pattern: %s", vmID, pattern)
                    state.BootCompleteTime = time.Now()
                    return nil
                }
            }
        }
    }

    // Timeout exceeded
    return fmt.Errorf("boot timeout exceeded: systemd=%v, cloud-init=%v",
        state.SystemdTargetReached, state.CloudInitFinished)
}

// tailFile tails a log file and returns lines via channel
func tailFile(ctx context.Context, path string) <-chan string {
    ch := make(chan string)

    go func() {
        defer close(ch)

        // Store start time in context for fallback timing
        ctx = context.WithValue(ctx, "startTime", time.Now())

        var file *os.File
        var err error

        // Wait for log file to be created
        for {
            file, err = os.Open(path)
            if err == nil {
                break
            }

            select {
            case <-ctx.Done():
                return
            case <-time.After(100 * time.Millisecond):
                continue
            }
        }
        defer file.Close()

        scanner := bufio.NewScanner(file)
        for scanner.Scan() {
            select {
            case <-ctx.Done():
                return
            case ch <- scanner.Text():
            }
        }
    }()

    return ch
}
```

---

### 3.2 cocoon-ready.service: Definitive Boot Signal (Phase 2)

**Purpose**: Inject a custom systemd service that provides a definitive "VM is ready" signal, independent of distribution-specific boot messages.

**Architecture**:
```
┌─────────────────────────────────────────────────────────┐
│ NoCloud seed disk (user-data)                           │
│ ↓                                                       │
│ cloud-init writes /etc/systemd/system/cocoon-ready.service│
│ ↓                                                       │
│ systemd starts cocoon-ready.service                    │
│   (after cloud-init.target + multi-user.target)       │
│ ↓                                                       │
│ Service prints "COCOON_READY" to serial console       │
└─────────────────────────────────────────────────────────┘
```

**user-data injection** (via NoCloud seed disk):
```yaml
#cloud-config
write_files:
  - path: /etc/systemd/system/cocoon-ready.service
    owner: root:root
    permissions: '0644'
    content: |
      [Unit]
      Description=Cocoon Boot Completion Marker
      After=multi-user.target cloud-init.target network-online.target
      Wants=network-online.target

      [Service]
      Type=oneshot
      ExecStart=/bin/sh -c 'echo "COCOON_READY" > /dev/ttyS0'
      RemainAfterExit=yes

      [Install]
      WantedBy=multi-user.target

runcmd:
  - systemctl daemon-reload
  - systemctl enable cocoon-ready.service
  - systemctl start cocoon-ready.service
```

**Enhanced boot detection with cocoon-ready**:
```go
// BootDetectionConfig with cocoon-ready support
type BootDetectionConfig struct {
    // ... existing patterns ...

    // Cocoon-ready service pattern (highest priority)
    CocoonReadyPattern string

    // Whether to use cocoon-ready service
    UseCocoonReady bool
}

func DefaultBootDetectionConfig() BootDetectionConfig {
    return BootDetectionConfig{
        // Cocoon-ready pattern (definitive signal)
        CocoonReadyPattern: "COCOON_READY",
        UseCocoonReady:     true, // Enable by default in Phase 2

        // ... existing patterns ...
    }
}

func WaitForBootCompletion(vmID string, config BootDetectionConfig) error {
    logPath := fmt.Sprintf("/var/log/cocoon/%s-serial.log", vmID)

    ctx, cancel := context.WithTimeout(context.Background(), config.Timeout)
    defer cancel()

    state := &BootCompletionState{}

    for line := range tailFile(ctx, logPath) {
        // Priority 1: Check for COCOON_READY marker (most reliable)
        if config.UseCocoonReady && strings.Contains(line, config.CocoonReadyPattern) {
            log.Info("VM %s: Cocoon-ready service completed", vmID)
            state.BootCompleteTime = time.Now()
            return nil
        }

        // Priority 2: Check systemd + cloud-init patterns (fallback)
        // ... existing pattern detection logic ...
    }

    return fmt.Errorf("boot timeout exceeded")
}
```

**Benefits**:
- ✅ **Distribution-agnostic**: Works across Ubuntu, Fedora, Debian, etc.
- ✅ **Reliable**: Explicit signal instead of inferring from log messages
- ✅ **Deterministic**: Runs after all critical services (cloud-init, network, multi-user)
- ✅ **No image modification**: Injected via cloud-init user-data at runtime
- ✅ **Backward compatible**: Falls back to pattern matching if service fails

**Rollout Strategy**:
- **Phase 1 (MVP)**: Use multi-pattern detection (current implementation)
- **Phase 2**: Enable cocoon-ready.service by default, keep pattern matching as fallback

---

### 3.3 Future I/O Mechanisms (Phase 3)

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

**Step 1: ACPI Power Button**

Cloud Hypervisor's ACPI support:
```bash
# Send ACPI power button event via API
curl -X PUT http://localhost/api/v1/vm.power-button \
  --unix-socket /run/cocoon/vms/vm-123/api.sock
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
1. Send ACPI shutdown to VM
2. Wait for CH process exit
3. Clean up socket and PID files
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

The canonical definition is in `types/config.go`. VMConfig is immutable after creation:

```go
type VMConfig struct {
    // Identity
    VMID string `json:"vm_id"`
    Name string `json:"name"`

    // Image provenance
    ImageRef       string `json:"image_ref"`
    BaseKey        string `json:"base_key"`         // {checksum_16}_{arch}
    BaseDigestFull string `json:"base_digest_full"` // Full SHA-256 (64 hex chars)
    Arch           string `json:"arch"`

    // Boot configuration
    BootStrategy  BootStrategy `json:"boot_strategy"`            // "uefi" (default), "direct" (OCI)
    FirmwarePath  string       `json:"firmware_path"`
    TPMSocketPath string       `json:"tpm_socket_path,omitempty"` // swtpm socket (if TPM enabled)

    // Resources
    CPUs     int    `json:"cpus"`
    MemoryMB int64  `json:"memory_mb"`   // In megabytes
    DiskSize string `json:"disk_size"`   // e.g., "10G"

    // Storage paths (derived, stored for fast lookup)
    BaseImagePath string `json:"base_image_path"`
    OverlayPath   string `json:"overlay_path"`
    SerialLog     string `json:"serial_log"`
    SocketPath    string `json:"socket_path"`

    // Timestamps
    CreatedAt string `json:"created_at"` // RFC 3339

    // Schema version for migration
    SchemaVersion int `json:"schema_version"`
}
```

**Example Configuration** (`config.json`, written once at VM creation):
```json
{
  "vm_id": "vm-01HXYZ5A3B7C8D9E0F1G2H3J4K",
  "name": "ubuntu-vm-1",
  "image_ref": "ubuntu-22.04-cloudimg",
  "base_key": "ef015678abcd1234_amd64",
  "base_digest_full": "ef015678abcd1234567890abcdef1234567890abcdef1234567890abcdef1234",
  "arch": "amd64",
  "boot_strategy": "uefi",
  "firmware_path": "/var/lib/cocoon/firmware/CLOUDHV.fd",
  "cpus": 2,
  "memory_mb": 2048,
  "disk_size": "10G",
  "base_image_path": "/var/lib/cocoon/cache/images/ef015678abcd1234_amd64.qcow2",
  "overlay_path": "/var/lib/cocoon/vms/vm-01HXYZ5A3B7C8D9E0F1G2H3J4K/overlay.qcow2",
  "serial_log": "/var/log/cocoon/vm-01HXYZ5A3B7C8D9E0F1G2H3J4K-serial.log",
  "socket_path": "/run/cocoon/vms/vm-01HXYZ5A3B7C8D9E0F1G2H3J4K/api.sock",
  "created_at": "2026-02-11T10:30:00Z",
  "schema_version": 1
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

> **Phase 1 implementation note**: `VerifyBootability()` performs a two-tier check. The basic tier (qcow2 integrity) is always available. The deep tier (guestfish) checks each component independently and sets `KernelFound`, `InitrdFound`, `SystemdFound`, `BootloaderFound` booleans. When deep verification runs, the function evaluates results: missing MUST components are added to `Errors` and `Bootable` is set to `false`. However, `VerifyBootability()` never returns a Go `error` for missing components — it returns the `BootCheckResult` struct and callers decide whether to proceed (e.g. `--skip-verify` bypasses the check entirely). Strict caller-side enforcement is deferred to Phase 2.

**SHOULD Have (Recommended for VM Initialization)**:
- 🔵 **cloud-init**: `/usr/bin/cloud-init` + datasource config
  - **Purpose**: VM initialization (users, SSH keys, hostname, network)
  - **NOT mandatory**: Can boot without it, but NoCloud configuration injection requires it
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
        log.Warn("cloud-init not found - VM will boot but NoCloud configuration injection disabled")
    }

    return nil
}
```

---

### 6.2 cloud-init Configuration Requirements

**cloud-init: CONDITIONAL** (see § 2.2 for details)

**IF using cloud-init with NoCloud**, images MUST have cloud-init datasource config:

```yaml
# /etc/cloud/cloud.cfg.d/99-cocoon.cfg
datasource_list: [ NoCloud ]
```

**MUST have NoCloud support**:
```bash
# Verify cloud-init has NoCloud module
cloud-init query -l  # Should list NoCloud
```

---

### 6.3 Architecture-Specific Requirements

#### x86_64

**Firmware**:
- UEFI: CLOUDHV.fd from `/var/lib/cocoon/firmware/CLOUDHV.fd` (deprecated fallback: `/usr/share/OVMF/OVMF_CODE.fd`)
- Direct kernel boot: No firmware needed (kernel + initramfs passed directly)

**Bootloader**:
- ESP location: `/boot/efi/EFI/BOOT/BOOTX64.EFI`
- GRUB target: `x86_64-efi`

#### aarch64 (Phase 2)

**Firmware**:
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
  - [ ] Download CLOUDHV.fd on install
  - [ ] Store in `/var/lib/cocoon/firmware/`
  - [ ] Implement `cocoon firmware` commands
  - [ ] Version management and updates

- [ ] **UEFI Boot**:
  - [ ] Locate UEFI firmware: primary `/var/lib/cocoon/firmware/CLOUDHV.fd`, deprecated fallback `/usr/share/OVMF/OVMF_CODE.fd`
  - [ ] Launch CH with UEFI firmware via REST `payload.firmware` (default boot strategy for non-OCI images)

- [ ] **Direct Kernel Boot** (OCI VM images):
  - [ ] Extract kernel and initramfs from OCI VM images
  - [ ] Launch CH with `payload.kernel` + `payload.initramfs` + `payload.cmdline`
  - [ ] Build kernel cmdline with `root=PARTUUID=<uuid> rw console=ttyS0,115200n8 console=hvc0`

- [ ] **Image Conversion**:
  - [ ] Validate cloud-init installed
  - [ ] Inject cloud-init datasource config
  - [ ] Modify GRUB cmdline for NoCloudNet
  - [ ] Regenerate GRUB config

- [ ] **VM Initialization**:
  - [ ] Generate user-data with users/SSH keys
  - [ ] Monitor serial log for boot completion
  - [ ] Implement multi-pattern boot detection:
    - [ ] Systemd target patterns (multi-user, graphical)
    - [ ] Cloud-init completion patterns
    - [ ] Fallback patterns (login prompt, welcome message)
  - [ ] Handle cloud-init enabled/disabled scenarios
  - [ ] Timeout handling with detailed error reporting

### Phase 2: Advanced Features (P1)

- [ ] **cocoon-ready.service Boot Marker**:
  - [ ] Add cocoon-ready.service to user-data generation
  - [ ] Update WaitForBootCompletion to check for COCOON_READY pattern
  - [ ] Add priority-based pattern matching (cocoon-ready → systemd+cloud-init → fallback)
  - [ ] Test across Ubuntu, Fedora, Debian distributions
  - [ ] Maintain backward compatibility with pattern-only detection

- [ ] **Architecture Support**:
  - [ ] aarch64 UEFI firmware (AAVMF)
  - [ ] ARM64-specific GRUB config

- [ ] **Alternative I/O**:
  - [ ] vsock for task output streaming
  - [ ] virtiofs for large file access

---

## Summary

**Boot Contract v2.0** establishes:

1. **Boot strategy**: UEFI (cloud images) + Direct kernel boot (OCI VM images)
2. **VM initialization**: cloud-init + NoCloud seed disks for initialization (users, SSH, network)
3. **Image requirements**: kernel + bootloader + systemd + cloud-init
4. **Graceful lifecycle**: ACPI shutdown with timeout
5. **Production ready**: Works with standard cloud images and OCI VM images

**Next Steps**:
- Read `docs/03-hypervisor-integration.md` for CH API details
- Read `docs/04-oci-conversion.md` for image conversion pipeline
- Read `docs/07-vm-lifecycle.md` for state machine specification

---

**End of Boot Contract v2.0**

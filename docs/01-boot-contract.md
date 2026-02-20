# Boot Contract & VM Lifecycle

**Version**: 1.1
**Status**: Implemented
**Phase**: Phase 1
**Last Updated**: 2026-02-20

## Executive Summary

Cocoon defines a strict **Boot Contract** that all VM images must satisfy. This contract ensures that VMs can be booted, configured, and managed uniformly regardless of their origin (cloud image or OCI conversion).

---

## 1. Boot Requirements

### 1.1 Boot Mode
- **Phase 1**: Only **UEFI** boot is supported.
- **Legacy BIOS**: Not supported.
- **Direct Kernel Boot**: Not supported in Phase 1 (Planned for Phase 2).

### 1.2 Disk Layout
- **Partition Table**: GPT (GUID Partition Table) is required.
- **ESP**: A standard EFI System Partition (ESP) must exist.
- **Root Filesystem**: Must be on a partition (not raw disk) discoverable by the bootloader.

### 1.3 Bootloader
- **Requirement**: A valid UEFI bootloader (e.g., GRUB2, systemd-boot) must be installed in the ESP.
- **Config**: A valid configuration file (e.g., `grub.cfg`) must exist.

---

## 2. Image Verification

Cocoon provides tools to verify if an image meets the boot contract.

### 2.1 Verification Tiers

1.  **Basic (Always Run)**:
    - Checks file format (`qcow2`) and integrity (`qemu-img check`).
    - **Fatal**: Failures here prevent VM creation.

2.  **Deep (Optional)**:
    - Inspects internal partitions for Kernel, Initrd, Systemd, and Bootloader.
    - **Tooling**: Requires `guestfish` (`libguestfs-tools`).
    - **Behavior**:
        - If `guestfish` is installed: Runs verification. Failures are reported as errors/warnings.
        - If `guestfish` is missing: **Skips** verification silently (logs warning).

### 2.2 Conversion vs. Verification Dependency

It is critical to distinguish between the **Conversion** pipeline and the **Verification** step:

| Feature | Scenario | Guestfish Requirement | Failure Behavior |
| :--- | :--- | :--- | :--- |
| **OCI Conversion** | `cocoon image pull <oci>` | **Mandatory** | **Fails** if missing. Cannot convert OCI to qcow2 without guestfish. |
| **Verification** | `cocoon image verify` | **Optional** | Skips if missing. Optimistically assumes valid. |
| **VM Creation** | `cocoon create` | **Optional** | Skips if missing. |

> **Note**: Users on macOS (Darwin) cannot run OCI conversion because `guestfish` is not available. They can, however, use `cocoon create` with pre-converted cloud images (qcow2).

---

## 3. Kernel Command Line

Cocoon manages the kernel command line to inject configuration.

### 3.1 Serial Console
- Cocoon expects console output on `ttyS0`.
- **OCI Conversion**: The conversion pipeline attempts to inject `console=ttyS0` into `grub.cfg` using `virt-customize` (if available).
- **Cloud Images**: Users should ensure their images are configured for serial console access.

---

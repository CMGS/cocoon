# Building Bootable OCI Images

**Version**: 1.1
**Status**: Planned
**Phase**: Phase 2
**Last Updated**: 2026-02-16

**Related Documents**: [01-boot-contract.md](./01-boot-contract.md), [04-oci-conversion.md](./04-oci-conversion.md), [04.1-oci-vm-images.md](./04.1-oci-vm-images.md), [09-cli-design.md](./09-cli-design.md)

## Table of Contents

1. [Overview](#1-overview)
2. [Minimum Requirements by Distro](#2-minimum-requirements-by-distro)
3. [Dockerfile Examples](#3-dockerfile-examples)
4. [Build Pipeline](#4-build-pipeline)
5. [Verification](#5-verification)
6. [Phase 1 vs Phase 2 Differences](#6-phase-1-vs-phase-2-differences)
7. [cocoon-ready.service](#7-cocoon-readyservice)
8. [Current Workaround (Phase 1)](#8-current-workaround-phase-1)
9. [References](#9-references)

---

## 1. Overview

A **bootable OCI image** in Cocoon's context is an OCI-compliant container image whose rootfs contains all the components required by the [Boot Contract](./01-boot-contract.md) to boot a Linux virtual machine under Cloud Hypervisor. At minimum, this means:

- A **Linux kernel** (`vmlinuz`) and **initramfs** (`initrd.img` / `initramfs-*.img`)
- **systemd** as PID 1 (the Boot Contract requires systemd; sysvinit and OpenRC are not supported)
- A **bootloader** (GRUB2 with EFI support) for the UEFI boot path

The image is published to any OCI-compliant registry (Docker Hub, GHCR, private registries) and consumed by Cocoon via `cocoon image pull`.

### 1.1 Two Consumption Paths

A bootable OCI image can be consumed in two ways, depending on the Cocoon phase:

| Path | Phase | Boot Mode | Rootfs Delivery | Requires Bootloader |
|------|-------|-----------|-----------------|---------------------|
| OCI-to-qcow2 conversion | Phase 1 | UEFI (`payload.firmware`) | qcow2 block device | Yes (GRUB2 + ESP) |
| OCI VM image (OverlayFS + virtiofs) | Phase 2 | Direct kernel boot (`payload.kernel`) | virtiofs from OverlayFS-merged layers | No |

**Phase 1**: The OCI image is pulled, its rootfs extracted, and converted to a qcow2 disk image with a GPT partition table and ESP. Cloud Hypervisor boots via UEFI firmware (CLOUDHV.fd), which loads GRUB, which loads the kernel from disk. This path requires the image to contain a bootloader.

**Phase 2**: The OCI image is decomposed into discrete layers (kernel, rootfs, customization) as defined in [docs/04.1-oci-vm-images.md](./04.1-oci-vm-images.md). The kernel and initramfs are extracted and passed directly to Cloud Hypervisor via `payload.kernel` and `payload.initramfs`. The rootfs layers are composed via OverlayFS and served to the guest via virtiofs. No bootloader is required on disk. Phase 2 also provides `cocoon image build-bootable`, a helper command that automates the image build process.

### 1.2 Relationship to docs/04.1

[docs/04.1-oci-vm-images.md](./04.1-oci-vm-images.md) defines the **OCI VM Image Format** -- a purpose-built format with custom media types and a Cocoonfile-based customization model. That document's `cocoon image build` command takes an existing cloud image (qcow2) as input and decomposes it into OCI VM layers.

This document covers a complementary concern: **how to build the base bootable image itself** -- the image that contains the kernel, systemd, and userspace packages that make it bootable. Users who start from scratch (no existing cloud image) need this guidance. Users who start from an existing cloud image (Ubuntu Cloud, Fedora Cloud) can skip directly to the Cocoonfile workflow in docs/04.1.

---

## 2. Minimum Requirements by Distro

All bootable OCI images must satisfy the [Boot Contract](./01-boot-contract.md) requirements. The following tables list the minimum packages needed per distribution family.

### 2.1 Ubuntu / Debian

| Component | Package(s) | Purpose |
|-----------|-----------|---------|
| Kernel | `linux-image-generic` | Linux kernel (`/boot/vmlinuz-*`) |
| Initramfs | `initramfs-tools` | Generates initrd (`/boot/initrd.img-*`) |
| Init system | `systemd` | PID 1, service management, journald |
| Bootloader | `grub-efi-amd64` | UEFI bootloader (Phase 1 only) |
| Serial console | `systemd` (built-in) | `serial-getty@ttyS0.service` for console access |
| Filesystem tools | `e2fsprogs`, `dosfstools` | ext4 and FAT32 support |

**Kernel path**: `/boot/vmlinuz-{version}` (e.g., `/boot/vmlinuz-5.15.0-100-generic`)
**Initramfs path**: `/boot/initrd.img-{version}` (e.g., `/boot/initrd.img-5.15.0-100-generic`)

### 2.2 Fedora / CentOS / RHEL

| Component | Package(s) | Purpose |
|-----------|-----------|---------|
| Kernel | `kernel` | Linux kernel (`/boot/vmlinuz-*`) |
| Initramfs | `dracut` | Generates initramfs (`/boot/initramfs-*.img`) |
| Init system | `systemd` | PID 1, service management, journald |
| Bootloader | `grub2-efi-x64` | UEFI bootloader (Phase 1 only) |
| Serial console | `systemd` (built-in) | `serial-getty@ttyS0.service` for console access |
| Filesystem tools | `e2fsprogs`, `dosfstools` | ext4 and FAT32 support |

**Kernel path**: `/boot/vmlinuz-{version}` (e.g., `/boot/vmlinuz-6.5.6-300.fc39.x86_64`)
**Initramfs path**: `/boot/initramfs-{version}.img` (e.g., `/boot/initramfs-6.5.6-300.fc39.x86_64.img`)

### 2.3 Alpine Linux

> **Note**: Alpine's default init system is OpenRC, not systemd. Cocoon's Boot Contract **requires systemd**, so Alpine-based bootable images must install `systemd` from the community repository or use a custom build. This adds complexity and is not recommended for most users.

| Component | Package(s) | Purpose |
|-----------|-----------|---------|
| Kernel | `linux-lts` | Linux kernel |
| Initramfs | `mkinitfs` | Generates initramfs |
| Init system | `systemd` (from community) | Requires replacing OpenRC with systemd |
| Bootloader | `grub-efi` | UEFI bootloader (Phase 1 only) |

Alpine support is **best-effort**. Ubuntu and Fedora are the recommended base distributions.

---

## 3. Dockerfile Examples

### 3.1 Ubuntu-Based Bootable OCI Image (Complete)

This multi-stage Dockerfile produces an Ubuntu-based bootable OCI image suitable for both Phase 1 (qcow2 conversion) and Phase 2 (direct kernel boot).

```dockerfile
# =============================================================================
# Stage 1: Install kernel, systemd, and bootloader
# =============================================================================
FROM ubuntu:22.04 AS base

# Prevent interactive prompts during package installation
ENV DEBIAN_FRONTEND=noninteractive

# Install the minimum Boot Contract packages
RUN apt-get update && apt-get install -y --no-install-recommends \
    linux-image-generic \
    initramfs-tools \
    systemd \
    systemd-sysv \
    grub-efi-amd64-bin \
    grub-efi-amd64-signed \
    shim-signed \
    e2fsprogs \
    dosfstools \
    iproute2 \
    isc-dhcp-client \
    openssh-server \
    sudo \
    && rm -rf /var/lib/apt/lists/*

# =============================================================================
# Stage 2: Configure the rootfs for VM boot
# =============================================================================
FROM base AS configured

# Configure /etc/fstab for standard VM disk layout
# Phase 1 (qcow2): root is a block device, ESP at /boot/efi
# Phase 2 (virtiofs): root is cocoonfs, no fstab needed (kernel cmdline handles it)
RUN echo '# Cocoon VM fstab' > /etc/fstab && \
    echo '# Phase 1: Root filesystem (overridden by kernel cmdline in Phase 2)' >> /etc/fstab && \
    echo 'LABEL=cloudimg-rootfs  /        ext4  defaults,discard  0 1' >> /etc/fstab && \
    echo 'LABEL=UEFI             /boot/efi vfat  umask=0077        0 1' >> /etc/fstab

# Enable serial console for Cloud Hypervisor
# Both ttyS0 (serial) and hvc0 (virtio console) are enabled
RUN systemctl enable serial-getty@ttyS0.service && \
    systemctl enable serial-getty@hvc0.service

# Configure GRUB for serial console output (Phase 1 / UEFI path)
RUN mkdir -p /etc/default/grub.d && \
    echo 'GRUB_CMDLINE_LINUX_DEFAULT="console=ttyS0,115200n8 console=hvc0"' \
    > /etc/default/grub.d/cocoon-serial.cfg && \
    echo 'GRUB_TERMINAL="serial console"' >> /etc/default/grub.d/cocoon-serial.cfg && \
    echo 'GRUB_SERIAL_COMMAND="serial --speed=115200 --unit=0 --word=8 --parity=no --stop=1"' \
    >> /etc/default/grub.d/cocoon-serial.cfg

# Set a default root password (users should override this)
RUN echo 'root:cocoon' | chpasswd

# Allow root login on serial console
RUN echo 'ttyS0' >> /etc/securetty 2>/dev/null || true

# Clean up
RUN apt-get clean && rm -rf /tmp/* /var/tmp/*

# =============================================================================
# Final: Export as bootable OCI image
# =============================================================================
FROM configured

# Metadata labels
LABEL org.opencontainers.image.title="Ubuntu 22.04 Bootable VM"
LABEL org.opencontainers.image.description="Bootable OCI image for Cocoon VM (satisfies Boot Contract)"
LABEL io.cocoon.bootable="true"
LABEL io.cocoon.init="systemd"
LABEL io.cocoon.bootloader="grub-efi-amd64"

CMD ["/sbin/init"]
```

### 3.2 Fedora-Based Bootable OCI Image (Minimal)

```dockerfile
FROM fedora:39 AS base

# Install Boot Contract packages
RUN dnf install -y \
    kernel \
    dracut \
    systemd \
    grub2-efi-x64 \
    shim-x64 \
    e2fsprogs \
    dosfstools \
    iproute \
    dhcp-client \
    openssh-server \
    sudo \
    && dnf clean all

# Enable serial console
RUN systemctl enable serial-getty@ttyS0.service && \
    systemctl enable serial-getty@hvc0.service

# Configure GRUB for serial console
RUN echo 'GRUB_CMDLINE_LINUX="console=ttyS0,115200n8 console=hvc0"' >> /etc/default/grub && \
    echo 'GRUB_TERMINAL="serial console"' >> /etc/default/grub

# Set default root password (override in production)
RUN echo 'root:cocoon' | chpasswd

LABEL io.cocoon.bootable="true"
LABEL io.cocoon.init="systemd"

CMD ["/sbin/init"]
```

### 3.3 Key Dockerfile Considerations

**Why multi-stage builds are optional for this use case**: Unlike application containers, bootable VM images do not benefit from separating build tools from runtime -- the kernel and initramfs *are* the runtime. Multi-stage builds are shown for organizational clarity, not size reduction.

**initramfs generation**: On Debian/Ubuntu, installing `linux-image-generic` triggers `initramfs-tools` to generate the initrd automatically via a kernel postinst hook. On Fedora, `dracut` runs as part of the `kernel` package installation. No manual `mkinitramfs` or `dracut` invocation is typically needed.

**Bootloader installation within a container**: Running `grub-install` inside a Docker build is not straightforward because there is no real block device. For Phase 1 (qcow2 conversion), the conversion step does **not** run `grub-install` either -- it only creates partitions, writes the rootfs via `tar-in`, and verifies that `grub.cfg` exists. The UEFI bootloader binary (`BOOTX64.EFI`) and GRUB modules must be pre-installed in the OCI image's `/boot/efi/EFI/BOOT/` directory. The Dockerfile must install the GRUB packages (e.g., `grub-efi-amd64-bin`, `grub-efi-amd64-signed`, `shim-signed`), which place the EFI binaries in the correct locations during package installation.

---

## 4. Build Pipeline

The end-to-end pipeline from Dockerfile to running VM:

```
Dockerfile
    |
    v
1. docker build -t myregistry.io/myvm:v1 .
    |
    v
2. docker push myregistry.io/myvm:v1
    |
    v
3. cocoon image pull myregistry.io/myvm:v1        (Phase 1: pull standard OCI container image)
   cocoon image pull --oci myregistry.io/myvm:v1  (Phase 2: pull OCI VM image)
    |
    v
4. cocoon image verify myregistry.io/myvm:v1
    |
    v
5. cocoon create myregistry.io/myvm:v1 --name myvm       (Phase 1: OCI->qcow2->UEFI)
   cocoon create --oci myregistry.io/myvm:v1 --name myvm  (Phase 2: direct kernel boot)
```

### 4.1 Step Details

**Step 1: `docker build`**

Build the bootable OCI image using a standard Docker (or Buildah/Podman) build:

```bash
docker build -t myregistry.io/ubuntu-vm:22.04 -f Dockerfile.ubuntu .
```

The image is a standard OCI container image at this point. It contains a full Linux rootfs with kernel, initramfs, and systemd.

**Step 2: `docker push`**

Push to any OCI-compliant registry:

```bash
docker push myregistry.io/ubuntu-vm:22.04
```

**Step 3: `cocoon image pull`**

Pull the image into Cocoon's local image cache:

```bash
# Phase 1: pull standard OCI container image
cocoon image pull myregistry.io/ubuntu-vm:22.04

# Phase 2: pull OCI VM image (uses --oci flag)
cocoon image pull --oci myregistry.io/ubuntu-vm:22.04
```

For Phase 1, this downloads the OCI container layers and extracts the rootfs into Cocoon's local storage. For Phase 2, the `--oci` flag signals that the image uses the OCI VM Image format ([docs/04.1](./04.1-oci-vm-images.md)) with separate kernel, rootfs, and customization layers. The `--oci` flag convention is defined in [docs/09-cli-design.md](./09-cli-design.md).

**Step 4: `cocoon image verify`**

Verify the image satisfies the Boot Contract:

```bash
cocoon image verify myregistry.io/ubuntu-vm:22.04
```

See [section 5](#5-verification) for the verification checks performed.

**Step 5: `cocoon create`**

Create and optionally start a VM from the image. The boot path depends on the phase and flags used. See [section 6](#6-phase-1-vs-phase-2-differences) for the distinction.

### 4.2 Phase 2 Helper: `cocoon image build-bootable`

Phase 2 introduces a convenience command that wraps the Dockerfile build:

```bash
cocoon image build-bootable --base ubuntu:22.04 --tag myregistry.io/myvm:v1
```

This command:
1. Generates a Dockerfile from a known-good template for the specified base distribution
2. Runs `docker build` (or Buildah) to produce the OCI image
3. Runs `cocoon image verify` on the result
4. Optionally pushes to a registry with `--push`

The generated Dockerfile includes all Boot Contract requirements and serial console configuration. Users who need custom packages or configuration should use their own Dockerfile instead.

---

## 5. Verification

### 5.1 `cocoon image verify`

The `cocoon image verify` command checks that an OCI image satisfies the Boot Contract. It performs the following checks against the extracted rootfs:

| Check | What It Looks For | Required |
|-------|-------------------|----------|
| **Kernel** | `/boot/vmlinuz*` exists | Yes |
| **Initramfs** | `/boot/initrd*` or `/boot/initramfs*` exists | Yes |
| **systemd** | `/sbin/init` is a symlink to systemd (or systemd binary exists) | Yes |
| **Bootloader** | GRUB EFI binary in `/usr/lib/grub/x86_64-efi/` (packages installed) | Phase 1 only |
| **Serial console** | `serial-getty@ttyS0.service` enabled | Warning only |
| **cocoon-ready.service** | `/etc/systemd/system/cocoon-ready.service` exists | Optional |

### 5.2 Verification Output

```
$ cocoon image verify myregistry.io/ubuntu-vm:22.04

Boot Contract Verification: myregistry.io/ubuntu-vm:22.04
=========================================================
[PASS] Kernel found: /boot/vmlinuz-5.15.0-100-generic
[PASS] Initramfs found: /boot/initrd.img-5.15.0-100-generic
[PASS] Init system: systemd (via /sbin/init -> /lib/systemd/systemd)
[PASS] Bootloader: grub-efi-amd64 packages installed
[PASS] Serial console: serial-getty@ttyS0.service enabled
[INFO] cocoon-ready.service: not found (optional, recommended for Phase 2)

Result: BOOTABLE (5/5 required checks passed)
```

### 5.3 Verification Failure Examples

```
$ cocoon image verify myregistry.io/broken-image:latest

Boot Contract Verification: myregistry.io/broken-image:latest
=============================================================
[PASS] Kernel found: /boot/vmlinuz-5.15.0-100-generic
[FAIL] Initramfs not found: no /boot/initrd* or /boot/initramfs* detected
[PASS] Init system: systemd (via /sbin/init -> /lib/systemd/systemd)
[FAIL] Bootloader: no GRUB EFI modules found
[WARN] Serial console: serial-getty@ttyS0.service not enabled

Result: NOT BOOTABLE (2 checks failed)
  - Install initramfs-tools and run: update-initramfs -c -k all
  - Install grub-efi-amd64 for UEFI boot support
```

### 5.4 Programmatic Verification

The verification logic mirrors the `VerifyBootability()` function defined in the Boot Contract. It returns a `BootCheckResult` struct with per-component booleans:

```go
type BootCheckResult struct {
    Bootable        bool     // Overall result
    BootModes       []string // Supported boot modes ("uefi", "direct")
    KernelFound     bool     // /boot/vmlinuz* present
    InitrdFound     bool     // /boot/initrd* or /boot/initramfs* present
    SystemdFound    bool     // /sbin/init -> systemd
    BootloaderFound bool     // GRUB EFI packages/modules present
    Errors          []string // List of failed checks
    Warnings        []string // Non-fatal issues
}
```

---

## 6. Phase 1 vs Phase 2 Differences

### 6.1 Phase 1: OCI-to-qcow2 Conversion (Current)

In Phase 1, bootable OCI images are consumed through the [OCI Conversion Pipeline](./04-oci-conversion.md):

```
OCI Image (rootfs) -> Buildah extraction -> guestfish partitioning -> qcow2
                                              |
                                              +-- Create GPT partition table
                                              +-- Create ESP (FAT32)
                                              +-- Create root partition (ext4)
                                              +-- Mount ESP at /boot/efi
                                              +-- tar-in rootfs (including pre-installed bootloader)
                                              +-- Verify grub.cfg exists
                                              +-- (Optional) Regenerate grub.cfg for serial console
```

**Important**: The conversion step does **not** run `grub-install`. The UEFI bootloader (`BOOTX64.EFI` in `/boot/efi/EFI/BOOT/`) and GRUB modules must already be present in the OCI image's rootfs. This is a **build-time** responsibility: the Dockerfile must install the GRUB packages (see [section 3](#3-dockerfile-examples)). The conversion step only partitions the disk, writes the rootfs, and verifies that a grub.cfg exists. If `virt-customize` is available, it optionally injects `console=ttyS0,115200n8` into the GRUB configuration for serial console support.

**Boot mode**: UEFI via `payload.firmware` (CLOUDHV.fd)
**Requires**: Kernel, initramfs, systemd, GRUB packages in the OCI image
**Rootfs delivery**: qcow2 block device attached to VM
**Boot chain**: UEFI firmware -> GRUB -> kernel -> systemd

### 6.2 Phase 2: Direct Kernel Boot + OverlayFS + virtiofs

In Phase 2, bootable OCI images can also be consumed via the [OCI VM Image Format](./04.1-oci-vm-images.md):

```
OCI Image -> cocoon image build -> OCI VM Image (kernel layer + rootfs layer)
                                        |
                                        +-- Layer 1: kernel + initrd (extracted from /boot)
                                        +-- Layer 2: rootfs (everything else)
                                        +-- Layer 3..N: customization (Cocoonfile)
```

**Boot mode**: Direct kernel boot via `payload.kernel` + `payload.initramfs` + `payload.cmdline`
**Requires**: Kernel, initramfs, systemd (no bootloader needed)
**Rootfs delivery**: virtiofs from OverlayFS-merged layers on the host
**Boot chain**: kernel (direct) -> initramfs -> systemd

### 6.3 Feature Comparison

| Feature | Phase 1 | Phase 2 |
|---------|---------|---------|
| Boot mode | UEFI | Direct kernel boot |
| Firmware | CLOUDHV.fd required | None |
| Bootloader in image | Required (GRUB2) | Not required |
| Rootfs delivery | qcow2 block device | virtiofs (OverlayFS) |
| Boot speed | Standard (firmware + GRUB chain) | Faster (no firmware init) |
| Registry deduplication | Per-image (no layer sharing) | Per-layer (kernel/rootfs shared) |
| Kernel upgrades | Guest can upgrade in-place | Rebuild OCI image (kernel pinned) |
| Build helper | Manual Dockerfile | `cocoon image build-bootable` |
| Image customization | Rebuild entire image | Cocoonfile (adds overlay layer) |
| CLI flag | `cocoon create <image>` | `cocoon create --oci <ref>` |

### 6.4 Migration Path

Users building bootable OCI images for Phase 1 do **not** need to rebuild for Phase 2. The same OCI image works with both paths:

- Phase 1 consumes the full rootfs and creates a qcow2 disk.
- Phase 2's `cocoon image build` can take the same OCI image, extract the kernel and rootfs into separate layers, and produce an OCI VM image.

The extra GRUB packages in a Phase 1 image are harmless in Phase 2 -- they simply occupy space in the rootfs layer but are never used (the bootloader is bypassed by direct kernel boot).

---

## 7. cocoon-ready.service

### 7.1 Purpose

`cocoon-ready.service` is an **optional** systemd service that provides a definitive "VM is ready" signal to Cocoon's boot detection logic. Instead of relying on heuristic pattern matching against serial log output (login prompts, systemd target messages), this service emits a well-known marker string `COCOON_READY` to the serial console after all critical services have started.

### 7.2 Service Definition

Users who want deterministic boot detection should bake this service into their bootable OCI images:

```ini
# /etc/systemd/system/cocoon-ready.service
[Unit]
Description=Cocoon Boot Completion Marker
After=multi-user.target network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/bin/sh -c 'echo "COCOON_READY" > /dev/ttyS0'
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
```

### 7.3 Adding to a Dockerfile

```dockerfile
# Install cocoon-ready.service for deterministic boot detection
COPY cocoon-ready.service /etc/systemd/system/cocoon-ready.service
RUN systemctl enable cocoon-ready.service
```

Or inline without a separate file:

```dockerfile
RUN printf '[Unit]\n\
Description=Cocoon Boot Completion Marker\n\
After=multi-user.target network-online.target\n\
Wants=network-online.target\n\
\n\
[Service]\n\
Type=oneshot\n\
ExecStart=/bin/sh -c '\''echo "COCOON_READY" > /dev/ttyS0'\''\n\
RemainAfterExit=yes\n\
\n\
[Install]\n\
WantedBy=multi-user.target\n' > /etc/systemd/system/cocoon-ready.service && \
    systemctl enable cocoon-ready.service
```

### 7.4 Detection Priority

When `cocoon-ready.service` is present in the image, Cocoon's boot detection uses the following priority order:

1. **COCOON_READY** marker on serial console (highest priority, definitive)
2. Systemd target patterns (`login:`, `Reached target.*Login`, `systemd .* running`)
3. Fallback patterns (`Welcome to`, `Startup finished`)

If the service is not present, boot detection falls back to patterns 2 and 3 automatically. There is no configuration change needed on the Cocoon side -- the detection logic always checks for all patterns.

### 7.5 Benefits

- **Distribution-agnostic**: The `COCOON_READY` marker is identical across Ubuntu, Fedora, Debian, and any other systemd-based distribution.
- **Deterministic**: Runs after `multi-user.target` and `network-online.target`, so the VM is fully operational when the signal is emitted.
- **No false positives**: Unlike login prompts (which can appear in error messages or logs), `COCOON_READY` is an unambiguous signal.
- **Backward compatible**: Images without the service work fine -- Cocoon falls back to heuristic detection.

---

## 8. Current Workaround (Phase 1)

**For Phase 1**, we recommend using **pre-built cloud images** instead of building custom bootable OCI images. Cloud images from major distributions already satisfy all Boot Contract requirements.

### 8.1 Recommended Cloud Images

**Ubuntu Cloud Images**:
```bash
# Download Ubuntu 22.04 Cloud Image
wget https://cloud-images.ubuntu.com/releases/22.04/release/ubuntu-22.04-server-cloudimg-amd64.img

# Convert to qcow2 if needed
qemu-img convert -O qcow2 ubuntu-22.04-server-cloudimg-amd64.img ubuntu-22.04-cloudimg.qcow2

# Use with Cocoon -- IMAGE is a positional parameter, --disk sets overlay size
cocoon create ubuntu-22.04-cloudimg.qcow2 --name myvm --cpus 2 --memory 2G --disk 20G
```

**Fedora Cloud Images**:
```bash
# Download Fedora 39 Cloud Image
wget https://download.fedoraproject.org/pub/fedora/linux/releases/39/Cloud/x86_64/images/Fedora-Cloud-Base-39-1.5.x86_64.qcow2

# Use with Cocoon -- IMAGE is positional, not a flag
cocoon create Fedora-Cloud-Base-39-1.5.x86_64.qcow2 --name fedora-vm
```

**Why Cloud Images Work**:
- Pre-installed kernel, initrd, systemd
- GRUB bootloader configured
- Users may optionally install cloud-init for guest initialization
- GPT + ESP partition layout
- Optimized for Cloud Hypervisor/KVM

### 8.2 Why Building Custom Images is Deferred

Building bootable OCI images from scratch is complex because it requires:
- Kernel and initramfs generation inside a container build environment
- GRUB package installation in the Dockerfile (the EFI bootloader binary must be present in the rootfs; the conversion step does not run `grub-install`)
- Partition table and filesystem setup during conversion
- Thorough testing across distributions

This complexity is orthogonal to Cocoon's core VM management functionality. Phase 2 introduces `cocoon image build-bootable` and the Cocoonfile model to simplify this process significantly.

---

## 9. References

- [Boot Contract Specification](./01-boot-contract.md) -- Required components for bootable images
- [OCI Conversion Guide](./04-oci-conversion.md) -- Phase 1 OCI-to-qcow2 pipeline and verification
- [OCI VM Image Format](./04.1-oci-vm-images.md) -- Phase 2 VM-native OCI format with layer deduplication
- [CLI Design](./09-cli-design.md) -- CLI command reference

---

**End of Building Bootable OCI Images v1.1**

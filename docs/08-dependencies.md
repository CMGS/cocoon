# Dependencies and Requirements

**Version**: 2.0
**Status**: Implemented
**Phase**: OCI native pull + direct runtime
**Last Updated**: 2026-02-20

## Overview

Cocoon uses a mixed toolchain:
- Native Go OCI registry integration for image probe/pull/push (`go-containerregistry`)
- External Linux tooling for qcow2 conversion and VM runtime

No external OCI CLI is required for registry pull/inspect in the current implementation.

## Dependency Matrix

| Component | Purpose | Minimum Version | Install Source |
| --- | --- | --- | --- |
| `cloud-hypervisor` | VM monitor | `v38.0.0` | upstream release binary/source |
| `ch-remote` | Cloud Hypervisor API CLI | `v38.0.0` | ships with cloud-hypervisor release |
| `virtiofsd` | virtio-fs daemon for OCI direct runtime | `1.7.0` | distro package |
| `qemu-img` | qcow2 create/convert/info | `8.0.0` | distro package |
| `guestfish` | image conversion + deep verification | `1.50.0` | `libguestfs-tools` |
| `virt-customize` | optional Cocoonfile RUN/COPY support | any | `libguestfs-tools` |
| `swtpm` | optional TPM 2.0 emulator | any | distro package |
| `CLOUDHV.fd` | UEFI firmware | recommended | `cocoon firmware install` |
| `/dev/kvm` | hardware virtualization | kernel support | host kernel/device |
| `overlayfs` | OCI runtime overlay mount | kernel support | host kernel |

## Go Library Dependencies

| Library | Purpose | Used By |
| --- | --- | --- |
| `github.com/google/go-containerregistry` | OCI registry auth/probe/pull/push | `oci/`, `image/pipeline/identify.go` |
| `github.com/urfave/cli/v2` | CLI framework | `cmd/cocoon/` |
| `github.com/oklog/ulid/v2` | VM ID generation | `utils/id.go` |
| `github.com/docker/go-units` | size/memory parsing | CLI vm/image commands |
| `golang.org/x/term` | hidden password input | `cocoon image login` |

## Install (Linux)

### Ubuntu 22.04 / 24.04

```bash
sudo apt-get update
sudo apt-get install -y \
  qemu-utils \
  libguestfs-tools \
  swtpm \
  swtpm-tools \
  virtiofsd \
  ovmf
```

Install cloud-hypervisor separately (static binary recommended):

```bash
CH_VERSION="v50.0.0"
curl -LO "https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/${CH_VERSION}/cloud-hypervisor-static"
chmod +x cloud-hypervisor-static
sudo mv cloud-hypervisor-static /usr/local/bin/cloud-hypervisor
```

### Fedora 39 / 40+

```bash
sudo dnf install -y \
  qemu-img \
  libguestfs-tools \
  swtpm \
  swtpm-tools \
  virtiofsd \
  edk2-ovmf
```

Install cloud-hypervisor separately (static binary recommended):

```bash
CH_VERSION="v50.0.0"
curl -LO "https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/${CH_VERSION}/cloud-hypervisor-static"
chmod +x cloud-hypervisor-static
sudo mv cloud-hypervisor-static /usr/local/bin/cloud-hypervisor
```

## Initialize Cocoon

```bash
sudo cocoon init --with-uefi-firmware \
  https://github.com/cloud-hypervisor/edk2/releases/latest/download/CLOUDHV.fd
sudo cocoon doctor
```

## What `cocoon doctor` Checks

Current dependency checks include:
- `cloud-hypervisor` version
- `ch-remote` version
- `virtiofsd` presence/version/path
- `qemu-img` version
- `guestfish` version
- `virt-customize` optional presence
- `swtpm` presence/version
- `/dev/kvm`
- `overlayfs` availability
- UEFI firmware path
- required directory tree
- OCI blob-ref cleanup health

### `--fix` Scope

`cocoon doctor --fix` currently auto-remediates:
- `virtiofsd` install/path mismatch (apt/dnf hosts)

Other checks are diagnostic only and require manual remediation.

## Common Failure Modes

### 1. `virtiofsd` Not Found

Symptoms:
- OCI direct runtime VM create/run fails preflight
- doctor shows configured binary missing but fallback path detected

Fix:
```bash
sudo cocoon doctor --fix
# or install manually and ensure command is in PATH
```

### 2. `guestfish` Not Found

Symptoms:
- cloud image / non-direct conversion paths fail
- deep image verify unavailable

Fix:
```bash
# Ubuntu/Debian
sudo apt-get install -y libguestfs-tools

# Fedora
sudo dnf install -y libguestfs-tools
```

### 3. `/dev/kvm` Missing or Inaccessible

Symptoms:
- VM boot fails early
- doctor reports kvm failure

Fix:
```bash
ls -l /dev/kvm
grep -E '(vmx|svm)' /proc/cpuinfo
sudo usermod -aG kvm "$USER"
```

### 4. Firmware Missing

Symptoms:
- UEFI boot mode fails

Fix:
```bash
cocoon firmware install
# or: cocoon init --with-uefi-firmware <URL>
```

## Validation Commands

```bash
cocoon doctor
cloud-hypervisor --version
ch-remote --version
qemu-img --version
guestfish --version
virtiofsd --version
```

## Tested Baselines (Reference)

| OS | Kernel | cloud-hypervisor | qemu-img | libguestfs | Status |
| --- | --- | --- | --- | --- | --- |
| Ubuntu 22.04 | 5.15 | v50.0.0 | 6.2 | 1.46 | supported |
| Ubuntu 24.04 | 6.8 | v50.0.0 | 8.2 | 1.50 | supported |
| Fedora 39 | 6.5 | v50.0.0 | 8.1 | 1.50 | supported |
| Fedora 40 | 6.8 | v50.0.0 | 8.2 | 1.52 | supported |

## Notes

- For OCI VM direct runtime, `virtiofsd` and `overlayfs` are hard requirements.
- For cloud image conversion and deep verify, `guestfish` remains required.
- Keep `cloud-hypervisor` and `ch-remote` from the same release line.

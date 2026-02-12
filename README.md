# Cocoon

[![test](https://github.com/projecteru2/cocoon/actions/workflows/test.yml/badge.svg)](https://github.com/projecteru2/cocoon/actions/workflows/test.yml)
[![golangci-lint](https://github.com/projecteru2/cocoon/actions/workflows/lint.yml/badge.svg)](https://github.com/projecteru2/cocoon/actions/workflows/lint.yml)
[![build](https://github.com/projecteru2/cocoon/actions/workflows/build.yml/badge.svg)](https://github.com/projecteru2/cocoon/actions/workflows/build.yml)

Lightweight VM manager built on [Cloud Hypervisor](https://www.cloudhypervisor.org/) for managing microVMs with fast boot times and minimal resource overhead. Part of the [Eru](https://github.com/projecteru2) ecosystem.

## Features

- **Fast boot**: PVH direct kernel boot (<100ms) with UEFI fallback
- **Content-addressed caching**: Deduplicated base images via SHA-256 checksum
- **COW overlays**: qcow2 copy-on-write for instant VM disk creation
- **Cloud image support**: Ubuntu Cloud, Fedora Cloud, Debian Cloud (qcow2)
- **Docker-like CLI**: `cocoon run`, `cocoon ps`, `cocoon stop`, `cocoon rm`
- **Reconciliation**: Automatic detection and repair of state inconsistencies
- **Zero-daemon**: Per-VM Cloud Hypervisor processes, no long-running daemon

## Requirements

- Go 1.22+
- Linux with KVM support (for running VMs)
- macOS supported for development (build + test only)

## Quick Start

```bash
# Clone
git clone https://github.com/projecteru2/cocoon.git
cd cocoon

# Build
make build

# Set up Cloud Hypervisor and firmware (Linux only, requires sudo)
sudo make setup-ch

# Run a VM from a cloud image
./cocoon run https://cloud-images.ubuntu.com/releases/22.04/release/ubuntu-22.04-server-cloudimg-amd64.img

# List VMs
./cocoon ps

# Stop a VM
./cocoon stop <vm-id>

# Delete a VM
./cocoon rm <vm-id>
```

## Supported Images

Cocoon requires **bootable VM images**, not regular container images.

| Type | Example | Status |
|------|---------|--------|
| Cloud images (qcow2) | Ubuntu Cloud, Fedora Cloud | Recommended |
| Bootable OCI images | Custom-built with kernel+initrd | Supported |
| Container images | `ubuntu:latest`, `python:3.11` | NOT supported |

```bash
# Download and run a cloud image (recommended)
./cocoon run https://cloud-images.ubuntu.com/releases/22.04/release/ubuntu-22.04-server-cloudimg-amd64.img

# Run from a local qcow2 file
./cocoon run ./ubuntu-22.04-server-cloudimg-amd64.img

# Custom options
./cocoon run --name my-vm --cpus 4 --memory 4096 --disk 20G ./image.qcow2
```

## Development Setup

### Prerequisites

| Tool | Purpose | Install |
|------|---------|---------|
| Go 1.22+ | Build & test | [go.dev/dl](https://go.dev/dl/) |
| golangci-lint | Linting | `go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest` |
| gofumpt | Formatting | `go install mvdan.cc/gofumpt@latest` |
| goimports | Import sorting | `go install golang.org/x/tools/cmd/goimports@latest` |
| mockery | Mock generation | `go install github.com/vektra/mockery/v2@latest` |

### Automated Setup

```bash
# Linux: install all dev tools + runtime dependencies (CH, firmware, qemu-img)
sudo bash scripts/setup-dev.sh

# Check what's installed / missing (no root needed)
bash scripts/setup-dev.sh --check-only

# Linux: install Cloud Hypervisor + firmware only
sudo make setup-ch
```

### Build & Test

```bash
make deps          # Tidy modules
make build         # Build binary
make test          # Run tests with race detector + coverage
make lint          # Run golangci-lint (24 linters)
make fmt           # Format code (gofumpt + goimports)
make fmt-check     # Check formatting without modifying
make mock          # Regenerate mock implementations
make ci            # Full CI pipeline (fmt-check + vet + lint + test + build)
make help          # Show all available targets
```

### Cross-Compilation

```bash
make build-linux   # linux/amd64 binary
```

## CLI Reference

```
cocoon run IMAGE         Create and start a VM
cocoon start VM          Start a stopped VM
cocoon stop VM           Stop a running VM
cocoon rm VM             Delete a VM
cocoon ps                List VMs
cocoon inspect VM        Show VM details (JSON)
cocoon logs VM           View serial console output
cocoon images            List cached base images
cocoon doctor            Reconcile VM state
cocoon version           Show version info
```

### Global Flags

```
--config PATH      Config file (default: /etc/cocoon/config.json, env: COCOON_CONFIG_PATH)
--root-dir PATH    Data directory (default: /var/lib/cocoon, env: COCOON_ROOT_DIR)
--log-level LEVEL  Log verbosity: debug, info, warn, error (default: info)
```

## Architecture

```
cocoon run IMAGE
    |
    v
+------------+     +----------------+     +---------------+
| image.Pull | --> | image.Convert  | --> | storage.COW   |
| (URL/local)|     | (raw -> qcow2) |     | CreateOverlay |
+------------+     +----------------+     +---------------+
                                                |
                                                v
                                    +--------------------+
                                    | hypervisor.Launch   |
                                    | (cloud-hypervisor)  |
                                    +--------------------+
                                                |
                                                v
                                         +-----------+
                                         | Running VM |
                                         +-----------+
```

### Package Structure

```
cmd/cocoon/       CLI entry point and command handlers
config/           Configuration loading and path helpers
hypervisor/       Cloud Hypervisor process management + REST API
image/            Image pulling, conversion, bootability verification
lock/             Cross-process flock(2) mutual exclusion
storage/          Reference counting, COW overlays, garbage collection
types/            Shared types, constants, state machine
utils/            Process management, atomic I/O, ID generation
vm/               VM lifecycle (create/start/stop/delete), reconciliation
version/          Build version info
```

### Key Design Decisions

- **Content-addressed caching**: `base_key = SHA256[:16]_ARCH` (e.g., `a1b2c3d4e5f67890_amd64`)
- **Dual JSON schema**: `config.json` (immutable) + `metadata.json` (mutable) per VM
- **flock(2) hierarchy**: GC (L1) > References (L2) > Conversion (L3) > VM Metadata (L4)
- **Atomic writes**: temp + fsync + rename for all JSON mutations
- **Boot strategy**: `pvh_then_uefi` (default), `uefi_only`, `pvh_only`

## Directory Layout

```
/var/lib/cocoon/
  cache/
    images/           Base images ({checksum}_{arch}.qcow2)
    locks/            Per-image conversion locks
  vms/
    {vm-id}/
      config.json     Immutable VM configuration
      metadata.json   Mutable runtime state
      overlay.qcow2   COW disk
  firmware/
    hypervisor-fw     PVH firmware (rust-hypervisor-firmware)
    CLOUDHV.fd        UEFI firmware
  temp/               Download staging
  trash/              GC soft-delete staging
```

## Documentation

Detailed design documents are in the [`docs/`](./docs/) directory:

| Doc | Topic |
|-----|-------|
| [00-overview](docs/00-overview.md) | Project overview and architecture |
| [01-boot-contract](docs/01-boot-contract.md) | Boot modes, firmware, guest init |
| [02-installation](docs/02-installation.md) | Prerequisites and setup |
| [03-hypervisor-integration](docs/03-hypervisor-integration.md) | Cloud Hypervisor API |
| [04-oci-conversion](docs/04-oci-conversion.md) | Image conversion pipeline |
| [05-storage-management](docs/05-storage-management.md) | COW, caching, GC |
| [06-concurrency](docs/06-concurrency.md) | Locking and consistency |
| [07-vm-lifecycle](docs/07-vm-lifecycle.md) | State machine and metadata |
| [08-dependencies](docs/08-dependencies.md) | Runtime dependencies |
| [09-cli-design](docs/09-cli-design.md) | CLI conventions |
| [10-implementation-roadmap](docs/10-implementation-roadmap.md) | Phase 1/2 roadmap |
| [11-bootable-oci-build](docs/11-bootable-oci-build.md) | Building bootable OCI images |

## Contributing

```bash
# Development workflow
make deps          # Tidy dependencies
# ... make changes ...
make fmt           # Format
make lint          # Lint
make test          # Test
make build         # Build

# Before committing
make ci            # Full CI check
```

## License

Apache 2.0 - see [LICENSE](LICENSE) for details.

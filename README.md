# Cocoon

Lightweight VM manager built on Cloud Hypervisor.

## Features

- **UEFI boot** -- CLOUDHV.fd UEFI firmware by default (direct kernel boot for OCI VM images planned for Phase 2)
- **TPM 2.0 emulation** -- optional swtpm integration via `--tpm` flag for measured boot and guest attestation
- **Content-addressed image cache** -- base images deduplicated by SHA-256 checksum
- **COW overlays** -- qcow2 copy-on-write disks backed by shared base images
- **Cloud image support** -- pull from HTTP/HTTPS URLs or use local qcow2/raw files
- **Interactive console** -- `cocoon console` for bidirectional PTY access to running VMs, SSH-style escape sequences
- **Docker-like CLI** -- `cocoon run`, `cocoon list`, `cocoon stop`, `cocoon delete`
- **State reconciliation** -- `cocoon doctor` detects and repairs metadata/process inconsistencies
- **Zero-daemon architecture** -- one Cloud Hypervisor process per VM, no long-running daemon
- **Garbage collection** -- automatic tracking of base image references with GC of unreferenced images, orphaned overlays, and temp files

## Requirements

- Linux with KVM (x86_64 or aarch64)
- Root access (sudo)
- [Cloud Hypervisor](https://github.com/cloud-hypervisor/cloud-hypervisor) v38.0+
- `qemu-img` (from qemu-utils package)
- UEFI firmware (`CLOUDHV.fd`)
- Go 1.25+ (build only)

## Installation

### go install

```bash
go install github.com/CMGS/cocoon/cmd/cocoon@latest
```

### Build from source

```bash
git clone https://github.com/CMGS/cocoon.git
cd cocoon
make build
```

This produces a `cocoon` binary in the project root. Use `make install` to install it into `$GOPATH/bin`.

## Quick Start

```bash
# Initialize directories and write a default config
cocoon init

# Run a VM from a cloud image URL
cocoon run https://cloud-images.ubuntu.com/releases/22.04/release/ubuntu-22.04-server-cloudimg-amd64.img

# List running VMs
cocoon list

# Stop a VM
cocoon stop <vm>

# Delete a VM and its storage
cocoon delete <vm>
```

## CLI Commands

| Command | Description |
|---------|-------------|
| `cocoon init` | Initialize cocoon directories and default config |
| `cocoon run IMAGE` | Create and start a VM from an image |
| `cocoon create IMAGE` | Create a VM from an image without starting it |
| `cocoon start VM` | Start a stopped VM |
| `cocoon stop VM` | Stop a running VM (graceful ACPI shutdown) |
| `cocoon kill VM` | Force-terminate a VM immediately (SIGKILL) |
| `cocoon delete VM` | Remove a VM and clean up storage (alias: `rm`) |
| `cocoon list` | List VMs (aliases: `ps`, `ls`) |
| `cocoon inspect VM` | Display detailed VM information as JSON |
| `cocoon logs VM` | View VM serial console logs |
| `cocoon console VM` | Attach an interactive console to a running VM |
| `cocoon image list` | List cached base images (alias: `ls`) |
| `cocoon image pull IMAGE_REF` | Pull and cache an image without creating a VM |
| `cocoon image inspect IMAGE_REF` | Show details of a cached image |
| `cocoon image remove IMAGE_REF` | Remove a cached image if unreferenced (alias: `rm`) |
| `cocoon image verify IMAGE_REF` | Check if an image (local path or cached ref) is bootable |
| `cocoon gc` | Run garbage collection on unreferenced images and orphaned resources |
| `cocoon firmware list` | List installed firmware files (alias: `ls`) |
| `cocoon firmware verify` | Check firmware files exist and are accessible |
| `cocoon doctor` | Check system health, dependencies, and VM state consistency |
| `cocoon version` | Show version, git revision, and build timestamp |

## Global Flags

| Flag | Env Variable | Default | Description |
|------|-------------|---------|-------------|
| `--config` | `COCOON_CONFIG_PATH` | `/etc/cocoon/config.json` | Config file path |
| `--root-dir` | `COCOON_ROOT_DIR` | (from config) | Root directory for persistent data |
| `--runtime-dir` | `COCOON_RUNTIME_DIR` | (from config) | Runtime directory for sockets and PIDs |
| `--log-dir` | `COCOON_LOG_DIR` | (from config) | Log directory for VM serial logs |
| `--log-level` | `COCOON_LOG_LEVEL` | `info` | Log level: debug, info, warn, error |

## Development on macOS

Cocoon targets Linux with KVM, so macOS cannot run VMs. However, you can build, lint, and run tests on macOS by pointing cocoon at a local dev directory:

```bash
# Set up local dev environment (no sudo needed)
export COCOON_CONFIG_PATH=./dev/config.json
export COCOON_ROOT_DIR=./dev/lib
export COCOON_RUNTIME_DIR=./dev/run
export COCOON_LOG_DIR=./dev/log

# Initialize
cocoon init

# Or equivalently:
cocoon --config ./dev/config.json --root-dir ./dev/lib --runtime-dir ./dev/run --log-dir ./dev/log init
```

The `dev/` directory is gitignored.

## Development

```bash
make build    # Build cocoon binary (CGO_ENABLED=0)
make test     # Run tests with race detector and coverage
make lint     # Run golangci-lint (auto-downloads v2.9.0)
make fmt      # Format code with gofumpt + goimports
make ci       # Full CI pipeline: fmt-check + vet + lint + test + build
```

See `make help` for all available targets.

## Documentation

Design documents and RFCs are in the [`docs/`](./docs/) directory.

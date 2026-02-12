# Cocoon

Lightweight VM manager built on [Cloud Hypervisor](https://www.cloudhypervisor.org/) for managing microVMs with fast boot times and minimal resource overhead.

## Features

- **Fast boot** -- PVH direct kernel boot with UEFI fallback via configurable boot strategies
- **Content-addressed image cache** -- base images deduplicated by SHA-256 checksum (`{checksum_16}_{arch}`)
- **COW overlays** -- qcow2 copy-on-write disks backed by shared base images for instant VM disk creation
- **Cloud image support** -- pull from HTTP/HTTPS URLs or use local qcow2/raw files
- **Docker-like CLI** -- `cocoon run`, `cocoon ps`, `cocoon stop`, `cocoon rm`
- **State reconciliation** -- `cocoon doctor` detects and repairs metadata/process inconsistencies
- **Zero-daemon architecture** -- one Cloud Hypervisor process per VM, no long-running daemon
- **Reference counting and GC** -- automatic tracking of base image references with garbage collection of unreferenced images, orphaned overlays, and temp files

## Requirements

- **Go 1.22+** (build and test)
- **Linux with KVM** (running VMs) -- x86_64 or aarch64
- **macOS** supported for development only (build and test, no VM execution)
- **Runtime dependencies** (Linux):
  - [Cloud Hypervisor](https://github.com/cloud-hypervisor/cloud-hypervisor) v41.0+
  - `qemu-img` (from qemu-utils/qemu-img package) -- overlay creation and image conversion
  - PVH firmware (`hypervisor-fw`) and/or UEFI firmware (`CLOUDHV.fd`)

## Quick Start

```bash
# Clone and build
git clone https://github.com/CMGS/cocoon.git
cd cocoon
make build

# Set up Cloud Hypervisor, firmware, and qemu-img (Linux only, requires sudo)
sudo make setup-ch

# Run a VM from a cloud image URL
./cocoon run https://cloud-images.ubuntu.com/releases/22.04/release/ubuntu-22.04-server-cloudimg-amd64.img

# List VMs
./cocoon ps

# View serial console output
./cocoon logs <vm-id>

# Stop a VM
./cocoon stop <vm-id>

# Delete a VM
./cocoon rm <vm-id>
```

## Installation

### Build from source

```bash
make build              # Build ./cocoon binary
make build-linux        # Cross-compile linux/amd64 binary (cocoon-linux-amd64)
make install            # Install to $GOPATH/bin
```

### Runtime setup (Linux)

The `setup-ch` script installs Cloud Hypervisor v41.0, PVH firmware (rust-hypervisor-firmware 0.4.2), UEFI firmware (CLOUDHV.fd), qemu-img, and creates the directory structure under `/var/lib/cocoon/`.

```bash
# Install Cloud Hypervisor + firmware + qemu-img
sudo make setup-ch

# Or run the script directly
sudo bash scripts/init-cloud-hypervisor.sh

# Check installation status without installing anything
bash scripts/init-cloud-hypervisor.sh --check-only
```

### Full development environment (Linux)

```bash
# Install everything: Cloud Hypervisor, firmware, qemu-img, buildah, Go dev tools
sudo bash scripts/setup-dev.sh

# Check what is installed / missing (no root needed)
bash scripts/setup-dev.sh --check-only
```

## CLI Reference

### Global Flags

```
--config PATH      Config file path (default: /etc/cocoon/config.json, env: COCOON_CONFIG_PATH)
--root-dir PATH    Root data directory (default: /var/lib/cocoon, env: COCOON_ROOT_DIR)
--log-level LEVEL  Log verbosity: debug, info, warn, error (default: info, env: COCOON_LOG_LEVEL)
```

### Commands

#### `cocoon run IMAGE` (alias: `create`)

Create and start a VM from an image.

```
Flags:
  --name, -n NAME           VM name (globally unique; auto-generated if omitted)
  --cpus, -c N              Number of vCPUs (default: 1)
  --memory, -m MB           Memory in MB (default: 512)
  --disk SIZE               Root disk overlay size, e.g. 10G, 20G (default: 10G)
  --boot-strategy STRATEGY  Boot strategy: pvh_then_uefi, uefi_only, pvh_only (default: pvh_then_uefi)
  --detach, -d              Create VM without starting it
```

The IMAGE argument accepts:
- HTTP/HTTPS URLs to cloud images (e.g., `https://cloud-images.ubuntu.com/.../ubuntu-22.04-server-cloudimg-amd64.img`)
- Local file paths to qcow2 or raw images (e.g., `./ubuntu.qcow2`, `/path/to/image.img`)
- OCI registry references (planned, not yet implemented)

#### `cocoon start VM_REF`

Start a stopped or newly created VM. Idempotent: starting a running VM is a no-op.

#### `cocoon stop VM_REF`

Stop a running VM via ACPI shutdown, falling back to SIGKILL on timeout.

```
Flags:
  --timeout DURATION    Graceful shutdown timeout (default: 30s)
```

#### `cocoon rm VM_REF` (alias: `delete`)

Remove a VM and clean up all associated storage (overlay, references, name index entry).

```
Flags:
  --force, -f    Force delete even if VM is running (stops it first)
```

#### `cocoon ps` (aliases: `list`, `ls`)

List VMs.

```
Flags:
  --all, -a        Show all VMs including stopped and errored
  --format FORMAT  Output format: table, json (default: table)
  --quiet, -q      Only display VM IDs
```

#### `cocoon inspect VM_REF`

Display detailed VM information as JSON (merged view of config.json and metadata.json).

```
Flags:
  --format FORMAT  Output format: json, yaml (default: json; yaml outputs JSON)
```

#### `cocoon logs VM_REF`

View VM serial console logs.

```
Flags:
  --follow, -f    Follow log output (poll every 500ms)
  --tail N        Number of lines to show from the end (default: 100)
```

#### `cocoon images` (alias: `image`)

List cached base images in the image cache.

```
Flags:
  --format FORMAT  Output format: table, json (default: table)
```

#### `cocoon doctor`

Check system health, detect VM state inconsistencies, and optionally repair them. Rebuilds the name index on every run.

```
Flags:
  --fix      Attempt to fix issues automatically
  --force    Force re-check; required for killing zombie processes
  --format FORMAT  Output format: table, json (default: table)
```

Detected inconsistency types: `state_mismatch`, `metadata_corrupted`, `stale_pid_file`, `zombie_socket`, `zombie_process`, `missing_overlay`.

#### `cocoon version`

Show version, git revision, and build timestamp.

### VM Reference Resolution

All commands accepting `VM_REF` resolve the reference as follows:
- If the ref starts with `vm-`, it is treated as a direct VM ID (validated against config.json on disk)
- Otherwise, the name index (`name-index.json`) is consulted to map the name to a VM ID

## Configuration

Cocoon loads configuration from a JSON file (default: `/etc/cocoon/config.json`). If the file does not exist, built-in defaults are used. The `--root-dir` CLI flag overrides `root_dir` from the config file.

```json
{
  "root_dir": "/var/lib/cocoon",
  "runtime_dir": "/run/cocoon",
  "log_dir": "/var/log/cocoon",
  "ch_binary": "cloud-hypervisor",
  "pvh_firmware_path": "/var/lib/cocoon/firmware/hypervisor-fw",
  "uefi_firmware_path": "/var/lib/cocoon/firmware/CLOUDHV.fd",
  "buildah_root": "/var/lib/cocoon/cache/buildah",
  "default_cpus": 1,
  "default_memory_mb": 512,
  "default_disk_size": "10G",
  "gc_grace_period_hours": 24,
  "gc_trash_retention_days": 7,
  "boot_timeout_seconds": 60,
  "stop_timeout_seconds": 30
}
```

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `COCOON_CONFIG_PATH` | Config file path | `/etc/cocoon/config.json` |
| `COCOON_ROOT_DIR` | Root data directory | `/var/lib/cocoon` |
| `COCOON_LOG_LEVEL` | Log level (debug, info, warn, error) | `info` |

## Supported Images

Cocoon requires **bootable VM images**, not container images.

| Type | Example | Status |
|------|---------|--------|
| Cloud images (qcow2/raw) | Ubuntu Cloud, Fedora Cloud, Debian Cloud | Supported |
| Local qcow2 files | `./my-image.qcow2` | Supported |
| Local raw disk images | `./disk.raw` | Supported (converted to qcow2) |
| OCI registry references | `docker.io/...` | Not yet implemented |
| Container images | `ubuntu:latest`, `python:3.11` | NOT supported |

Image formats are auto-detected via `qemu-img info`. Raw images are converted to qcow2 during the caching step. Images already in qcow2 format are atomically copied into the cache.

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

| Package | Purpose |
|---------|---------|
| `cmd/cocoon/` | CLI entry point, command handlers, output formatting |
| `config/` | Configuration loading, default values, derived path helpers |
| `hypervisor/` | Cloud Hypervisor process management (launch/kill/monitor) and REST API client |
| `image/` | Image pulling (URL/local/OCI), format detection, raw-to-qcow2 conversion, bootability verification, cache listing |
| `lock/` | Cross-process mutual exclusion via `flock(2)` |
| `storage/` | Reference counting, COW overlay creation (qemu-img), garbage collection |
| `types/` | Shared types: VM config, metadata, state machine, boot strategies, error types |
| `utils/` | Process management, atomic JSON I/O (temp + fsync + rename), ULID-based ID generation |
| `vm/` | VM lifecycle (create/start/stop/delete), state transitions, name index, reconciliation |
| `version/` | Build-time version, revision, and timestamp |

### Key Design Decisions

- **Content-addressed caching**: base_key = `SHA256[:16]_ARCH` (e.g., `a1b2c3d4e5f67890_amd64`). Full SHA-256 stored in `references.json` for collision detection.
- **Dual JSON schema per VM**: `config.json` (immutable, written once at creation) + `metadata.json` (mutable, updated on every state transition)
- **flock(2) lock hierarchy**: GC lock (Level 1) > References lock (Level 2) > Conversion lock (Level 3) > VM Metadata lock (Level 4)
- **Atomic writes**: all JSON mutations use temp file + fsync + rename
- **Boot strategies**: `pvh_then_uefi` (default), `uefi_only`, `pvh_only`
- **VM state machine**: CREATING -> CREATED -> STARTING -> RUNNING -> STOPPING -> STOPPED (with ERROR reachable from most states, DELETED as terminal)
- **Name resolution**: VM references resolve by name index first; `vm-` prefix bypasses the index for direct ID lookup

### Directory Layout

```
/var/lib/cocoon/
  cache/
    images/           Base images ({checksum}_{arch}.qcow2)
    locks/            Per-image conversion locks
    manifests/        OCI manifest cache (future)
    buildah/          Buildah storage root (future)
  vms/
    {vm-id}/
      config.json     Immutable VM configuration
      metadata.json   Mutable runtime state
      overlay.qcow2   COW disk backed by base image
  firmware/
    hypervisor-fw     PVH firmware (rust-hypervisor-firmware)
    CLOUDHV.fd        UEFI firmware
  temp/               Download staging area
  trash/              GC soft-delete staging
  references.json     Base image reference counts
  name-index.json     VM name -> vm_id mapping

/run/cocoon/
  vms/
    {vm-id}/
      api.sock        Cloud Hypervisor API socket
      ch.pid          Cloud Hypervisor process ID

/var/log/cocoon/
  {vm-id}-serial.log  Serial console output
```

## Development

### Prerequisites

| Tool | Purpose | Install |
|------|---------|---------|
| Go 1.22+ | Build and test | [go.dev/dl](https://go.dev/dl/) |
| golangci-lint v2.9.0 | Linting | Auto-installed by `make lint` into `./bin/` |
| gofumpt | Formatting | Auto-installed by `make fmt` into `./bin/` |
| goimports | Import sorting | Auto-installed by `make fmt` into `./bin/` |
| mockery | Mock generation | `go install github.com/vektra/mockery/v2@latest` |

Development tools (golangci-lint, gofumpt, goimports) are automatically downloaded into the project-local `./bin/` directory by Makefile targets. No global installation is required.

### Makefile Targets

```bash
make build         # Build cocoon binary (CGO_ENABLED=0)
make build-linux   # Cross-compile for linux/amd64
make install       # Install to $GOPATH/bin
make test          # Run tests with race detector and coverage
make test-race     # Run tests with race detector only
make test-short    # Run short tests (skip long-running)
make coverage      # Generate and display coverage report
make vet           # Run go vet
make lint          # Run golangci-lint (auto-downloads v2.9.0)
make fmt           # Format code with gofumpt + goimports
make fmt-check     # Check formatting without modifying files
make mock          # Regenerate mock implementations
make deps          # Tidy Go modules
make clean         # Remove build artifacts, coverage files, test cache
make setup-dev     # Run scripts/setup-dev.sh
make setup-ch      # Run scripts/init-cloud-hypervisor.sh
make ci            # Full CI pipeline: fmt-check + vet + lint + test + build
make verify        # Lint + fmt-check + git diff check
make cloc          # Count lines of code (requires cloc)
make help          # Show all targets with descriptions
```

### CI Workflows

| Workflow | Trigger | What it does |
|----------|---------|-------------|
| `test.yml` | Push/PR on all branches | go vet, test with race/coverage, build binary |
| `lint.yml` | Push/PR on all branches | golangci-lint v2.9.0 |
| `build.yml` | Push to main or tags | Cross-compile linux/amd64 and linux/arm64, upload artifacts |
| `goreleaser.yml` | Tags matching `v*` | GoReleaser release |

### Running Tests

```bash
make test          # Full test suite with race detector + coverage
make test-short    # Skip long-running tests
make coverage      # Test + coverage report
```

### Code Quality

```bash
# Before committing
make ci            # fmt-check + vet + lint + test + build

# Or individually
make fmt           # Auto-format
make lint          # Lint
make vet           # Vet
```

## Documentation

Detailed design documents are in the [`docs/`](./docs/) directory:

| Document | Topic |
|----------|-------|
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

## License

MIT

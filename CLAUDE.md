# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Test Commands

```bash
make build          # Build cocoon binary (CGO_ENABLED=0)
make lint           # Lint for BOTH linux and darwin (golangci-lint v2, auto-downloaded)
make test           # Tests with -race -count=1 -cover (includes go vet)
make fmt            # Format with gofumpt + goimports
make ci             # Full CI: fmt-check + vet + lint + test + build

# Run a single test
go test -run TestFunctionName ./path/to/package/

# Run tests for a single package
go test -race -count=1 ./oci/
```

**Critical**: Always run `make lint` (not `golangci-lint` directly). The Makefile runs lint for both `GOOS=linux` and `GOOS=darwin` — the project has platform-specific files that must pass on both.

## Project Overview

Cocoon is a zero-daemon VM manager built on Cloud Hypervisor. One CH process per VM, Docker-like CLI (`cocoon run/create/start/stop/delete`), two boot modes: UEFI (cloud images) and direct kernel boot (OCI VM images with virtiofs rootfs).

## Architecture

### Package Dependency Flow

```
cmd/cocoon/        CLI commands + app wiring (app.go composes all managers)
  ├── vm/engine/   VM lifecycle (Manager interface in vm/vm.go)
  │     ├── hypervisor/cloudhypervisor/   CH process + REST API (Client interface)
  │     ├── storage/local/                COW overlays + reference counting + GC
  │     └── image/pipeline/               Cloud image pull + convert
  ├── oci/         OCI VM image build/push/pull/store (Builder interface in image/image.go)
  ├── config/      CocoonConfig (JSON config + derived path helpers)
  ├── lock/flock/  File-based flock(2) locks (Locker interface)
  └── types/       Shared types (VMConfig, VMMetadataFile, etc.)
```

### Key Interfaces (all have mock implementations in `*/mocks/`)

| Interface | Package | Implementation |
|-----------|---------|----------------|
| `hypervisor.Client` | `hypervisor/` | `cloudhypervisor/client.go` |
| `vm.Manager` | `vm/` | `vm/engine/manager.go` |
| `image.Manager` | `image/` | `image/pipeline/manager.go` |
| `image.Builder` | `image/` | `oci/builder.go` |
| `storage.ReferenceCounter` | `storage/` | `storage/local/refcount.go` |
| `storage.COWManager` | `storage/` | `storage/local/cow.go` |
| `storage.GarbageCollector` | `storage/` | `storage/local/gc.go` |
| `lock.Locker` | `lock/` | `lock/flock/flock.go` |

### Platform-Specific Files

The project targets Linux+KVM for VM execution but supports macOS for build/lint/test. Platform-specific code uses `_linux.go` / `_darwin.go` suffixes (no build tags):

- `oci/build_linux.go` / `build_darwin.go` (OCI build — darwin is stub)
- `oci/delta_linux.go` (layer diffing — linux only)
- `image/pipeline/convert_linux.go` / `convert_darwin.go`
- `image/pipeline/oci_linux.go` / `oci_darwin.go`
- `image/pipeline/verify_linux.go` / `verify_darwin.go`
- `vm/engine/overlay_runtime_linux.go` / `overlay_runtime_other.go`
- `utils/process_linux.go` / `process_darwin.go`
- `cmd/cocoon/console_linux.go` / `console_darwin.go`

### Two Boot Paths

1. **UEFI boot** (cloud images): firmware CLOUDHV.fd → qcow2 base + COW overlay → serial log on ttyS0
2. **Direct kernel boot** (OCI VM images): kernel + initramfs + virtiofs rootfs → virtiofsd daemon → overlay mount → serial log on ttyS0 + console on hvc0

Boot detection (`vm/engine/boot_detect.go`) polls the serial log file for success/failure regex patterns.

### Storage Model

- **Base images**: content-addressed by SHA-256 (`{checksum_16}_{arch}.qcow2`), immutable, shared
- **Overlays**: per-VM qcow2 COW backed by base image
- **OCI store**: shared blob store + per-tag OCI layouts with hardlinks
- **Reference counting**: tracks VM→base image references, enables safe GC

### Lock Hierarchy (strict ordering, flock-based, NOT reentrant)

```
L1: gc.lock           (GarbageCollector)
L2: references.lock   (ReferenceCounter) / name-index.lock
L3: {baseKey}.lock    (Image conversion)
L4: {vmID}-meta.lock  (VM metadata)
```

### Persistence Pattern

All mutable state uses atomic writes: write to temp file → fsync → rename. JSON files protected by flock. Config (`config.json`) is immutable after VM creation; metadata (`metadata.json`) is mutable with atomic updates.

## Code Conventions

### From AGENTS.md (authoritative)

- **Context**: flows through parameters, never created inside business logic. Tests use `t.Context()`, not `context.Background()`.
- **Error scope**: prefer `if err := fn(); err != nil` over separate declaration when the error is the only needed return value.
- **Pre-commit**: `make lint && make test` before every commit, no exceptions.
- **Git workflow**: rebase merge for PRs (`gh pr merge --rebase`).
- **Issues**: must have Summary, Scope, Out of Scope, Acceptance Criteria (checkboxes), Checklist in the description body — this is the single source of truth.

### Linter Config

golangci-lint v2 (`.golangci.yml`): `gocyclo` max 30, `nestif` max 5, `nakedret` max 30 lines, `goconst` min 3 occurrences. Mocks and test files have relaxed rules. Formatter: `gofumpt` + `goimports` with local prefix `github.com/CMGS/cocoon`.

### Dependency Injection

Major components use function-type fields for testability (e.g., `virtiofsdRuntimeManager` has `launchFn`, `waitReadyFn`, `stopFn` fields). Constructors set production defaults; tests override with fakes.

### Error Classification

`types.NewPermanentError()` marks non-retryable errors (bad format, missing file). Transient errors (network, locked files) are retryable. See `oci/push.go` and `oci/pull.go` for `classifyRemoteError` patterns.

## Key File Locations

- **App wiring**: `cmd/cocoon/app.go` — creates all managers, the composition root
- **VM engine** (largest file): `vm/engine/manager.go` — VM lifecycle, state machine, boot flow
- **CH VM config builder**: `vm/engine/manager.go:buildCHVMConfig` — translates VMConfig → CHVMConfig
- **Image resolver**: `vm/engine/image_resolver.go` — classifies image refs (local tag, cache, registry)
- **OCI runtime**: `vm/engine/oci_runtime_prepare.go` — materializes kernel/rootfs/initrd for direct boot
- **Config paths**: `config/config.go` — all derived path helpers (VMDir, BaseImagePath, OCILayoutDir, etc.)
- **OCI media types**: `oci/types.go` — Cocoon VM artifact types and media types

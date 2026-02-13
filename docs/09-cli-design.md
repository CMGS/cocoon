# CLI Design and Commands

**Version**: 1.0
**Status**: Draft
**Last Updated**: 2026-02-11

## ⚠️ Supported Image Contract

**CRITICAL**: Cocoon requires **bootable VM images**, not regular container images.

**Supported Image Types**:
1. **Cloud Hypervisor Native Cloud Images** (Recommended):
   - Ubuntu Cloud, Fedora Cloud, Debian Cloud (qcow2 format)
   - Pre-configured for cloud-init and PVH/UEFI boot
   - Direct usage without OCI conversion

2. **Bootable OCI Images** (Custom-built):
   - **Phase 1**: OCI→qcow2 conversion with **strict bootability validation**
   - Must contain: kernel, initrd, init system (systemd), bootloader
   - **Non-bootable OCI images will fail with clear error**
   - Requires build tooling (see docs/11-bootable-oci-build.md)

**NOT Supported** (Phase 1):
- Regular container images (`ubuntu:latest`, `python:3.11`, etc.)
- These lack kernel/bootloader and will **fail bootability validation**
- Phase 2 will add auto-conversion capabilities

See [00-overview.md § Supported Image Contract](./00-overview.md#️-supported-image-contract) for details.

---

## Executive Summary

This document defines the command-line interface for Cocoon, a lightweight VM management tool built on Cloud Hypervisor. The CLI follows Docker-like patterns for familiarity while exposing VM-specific capabilities like PVH/UEFI boot modes, resource allocation, and lifecycle management.

The design integrates the [Boot Contract](./01-boot-contract.md) decisions, including flexible boot modes (PVH preferred, UEFI optional), cloud-init task injection, serial console I/O, and graceful shutdown semantics. It also leverages the [storage management](./05-storage-management.md) system for efficient copy-on-write disk handling.

## Table of Contents

1. [Project Structure](#1-project-structure)
2. [Core Interfaces](#2-core-interfaces)
3. [VM Identifier Resolution](#3-vm-identifier-resolution)
4. [CLI Commands](#4-cli-commands)
5. [Configuration](#5-configuration)
6. [Implementation Flow](#6-implementation-flow)
7. [Examples](#7-examples)
8. [Cross-References](#8-cross-references)

---

## 1. Project Structure

Cocoon uses an interface-per-package pattern with concrete implementations in sub-packages. CLI commands live under `cmd/cocoon/`, each in its own file. Mock implementations live alongside their interfaces in `mocks/` sub-packages.

```
cocoon/
├── cmd/cocoon/
│   ├── main.go               # CLI entry point (urfave/cli/v2, signal handling)
│   ├── app.go                # appContext: manager initialization, root check
│   ├── output.go             # Table/JSON output helpers (printTable, printJSON)
│   ├── init.go               # cocoon init (dirs + config + firmware download)
│   ├── create.go             # cocoon create + shared vmCreateFlags()
│   ├── run.go                # cocoon run (create + start)
│   ├── start.go              # cocoon start
│   ├── stop.go               # cocoon stop
│   ├── kill.go               # cocoon kill
│   ├── rm.go                 # cocoon delete/rm
│   ├── ps.go                 # cocoon list/ps/ls
│   ├── inspect.go            # cocoon inspect
│   ├── logs.go               # cocoon logs
│   ├── images.go             # cocoon image (list/pull/inspect/remove/verify)
│   ├── gc.go                 # cocoon gc
│   ├── firmware.go           # cocoon firmware (list/verify/install/update)
│   ├── doctor.go             # cocoon doctor (deps + reconciliation)
│   └── version.go            # cocoon version
├── config/
│   ├── config.go             # CocoonConfig, LoadConfig, RebaseRootDir, EnsureDirs, path helpers
│   └── config_test.go
├── hypervisor/
│   ├── hypervisor.go         # Client interface (process mgmt + CH REST API)
│   ├── types.go              # CHVMConfig, CHVMInfo, CHDiskConfig, etc.
│   ├── cloudhypervisor/
│   │   ├── client.go         # Client implementation (launch, retry, buildLaunchArgs)
│   │   ├── client_test.go
│   │   └── procattr.go       # Platform-specific SysProcAttr
│   └── mocks/
│       └── mock.go
├── image/
│   ├── image.go              # Manager interface (Pull, Convert, Prepare, Verify, List, Remove)
│   ├── types.go              # ImageIdentity, ImageType, BootCheckResult, CachedImage
│   ├── pipeline/
│   │   ├── manager.go        # Pipeline implementation (pull → convert → cache)
│   │   ├── checksum.go       # Content-addressed checksum generation
│   │   ├── format.go         # Image format detection (qcow2/URL/OCI)
│   │   ├── oci_linux.go      # OCI pull via skopeo + buildah (Linux only)
│   │   ├── oci_darwin.go     # Stub: returns "Linux only" error
│   │   ├── convert_linux.go  # OCI→qcow2 via qemu-img + guestfish (Linux only)
│   │   ├── convert_darwin.go # Stub: returns "Linux only" error
│   │   ├── verify_linux.go   # Bootability verification via guestfish
│   │   ├── verify_darwin.go  # Stub: returns "Linux only" error
│   │   ├── cleanup_linux.go  # buildah umount + rm
│   │   └── cleanup_darwin.go # No-op
│   └── mocks/
│       └── mock.go
├── storage/
│   ├── storage.go            # ReferenceCounter, COWManager, GarbageCollector interfaces
│   ├── types.go              # OverlayInfo, GCResult
│   ├── local/
│   │   ├── refcount.go       # ReferenceCounter impl (references.json + flock)
│   │   ├── cow.go            # COWManager impl (qemu-img create/resize)
│   │   └── gc.go             # GarbageCollector impl (sweep + trash)
│   └── mocks/
│       └── mock.go
├── vm/
│   ├── vm.go                 # Manager interface (CRUD, state, reconcile)
│   ├── types.go              # CreateOptions, Inconsistency, InconsistencyType
│   ├── engine/
│   │   ├── manager.go        # Manager impl (lifecycle, boot fallback, state machine)
│   │   ├── manager_test.go
│   │   ├── boot_detect.go    # waitForBoot: serial log polling for boot patterns
│   │   ├── boot_detect_test.go
│   │   ├── name_index.go     # Name ↔ vm_id resolution
│   │   └── reconcile.go      # VM state reconciliation logic
│   └── mocks/
│       └── mock.go
├── types/
│   ├── boot.go               # BootStrategy, BootConfig, DefaultBootStrategy
│   ├── config.go             # VMConfig (immutable, written at create)
│   ├── errors.go             # ErrorType (14 constants), ClassifiedError
│   ├── inspect.go            # VMInspect (merged config + metadata view)
│   ├── metadata.go           # VMMetadataFile (mutable runtime state)
│   ├── reference.go          # NameIndex, Reference types
│   ├── state.go              # VMState (8 states), ValidateTransition
│   └── state_test.go
├── lock/
│   ├── interface.go          # Locker interface (Lock, TryLock, Unlock, Path)
│   ├── flock/
│   │   ├── flock.go          # flock(2) implementation
│   │   └── flock_test.go
│   └── mocks/
│       └── mock.go
├── utils/
│   ├── atomic.go             # AtomicWriteJSON, AtomicReadJSON (temp + fsync + rename)
│   ├── atomic_test.go
│   ├── id.go                 # ULID-based VM ID generation
│   ├── process.go            # Process liveness helpers
│   ├── process_linux.go      # Linux-specific SysProcAttr
│   └── process_darwin.go     # Darwin stub
├── version/
│   └── version.go            # NAME, VERSION, REVISION, BUILTAT (ldflags)
├── go.mod                    # Go 1.25.0
├── Makefile
├── scripts/                  # Setup, teardown, update scripts
└── docs/                     # Design specifications
```

---

## 2. Core Interfaces

### 2.1 Hypervisor Client Interface

The `hypervisor.Client` interface covers two concerns: CH process management (launch, kill, liveness) and the CH REST API over Unix sockets. One CH process per VM for strong isolation.

```go
package hypervisor

import (
    "context"
    "time"

    "github.com/CMGS/cocoon/types"
)

// Client is the interface for managing a Cloud Hypervisor instance.
type Client interface {
    // --- Process management ---
    Launch(ctx context.Context, vmID string, cfg *types.VMConfig) (pid int, err error)
    Shutdown(ctx context.Context, vmID string, timeout time.Duration) error
    ForceKill(vmID string) error
    IsAlive(vmID string) bool

    // --- CH REST API over Unix socket ---
    CreateVM(ctx context.Context, socketPath string, vmCfg *CHVMConfig) error
    BootVM(ctx context.Context, socketPath string) error
    ShutdownVM(ctx context.Context, socketPath string) error
    PowerButton(ctx context.Context, socketPath string) error
    DeleteVM(ctx context.Context, socketPath string) error
    GetVMInfo(ctx context.Context, socketPath string) (*CHVMInfo, error)

    // --- Utilities ---
    WaitForSocket(ctx context.Context, socketPath string, timeout time.Duration) error
    CheckSocketConnectivity(socketPath string) error
}
```

The concrete implementation lives in `hypervisor/cloudhypervisor/client.go`. Key implementation details:
- `Launch()` starts the CH binary with `--api-socket` + firmware flag only; all VM config goes via REST API.
- `buildLaunchArgs()` selects `--kernel` for UEFI or `--firmware` for PVH based on `BootStrategy`.
- REST API calls use `doWithRetry()` with exponential backoff (100ms/200ms/400ms + jitter).
- `isRetryable()`: retry on 500/503/429/connection-refused; no retry on 4xx/context.Canceled.

### 2.2 Image Manager Interface

The `image.Manager` handles multi-source image references (local qcow2 files, cloud image URLs, OCI registry references). Images are identified by content-addressed keys (`{checksum_16}_{arch}`).

```go
package image

import "context"

type Manager interface {
    // Pull downloads an image (OCI ref, HTTP URL, or local file path).
    // Returns the ImageIdentity describing the content-addressed identity.
    Pull(ctx context.Context, ref string) (*ImageIdentity, error)

    // Convert transforms a pulled image into a qcow2 base image.
    // Uses per-image conversion lock (Level 3) to prevent duplicate work.
    Convert(ctx context.Context, identity *ImageIdentity) (baseImagePath string, err error)

    // Prepare is the combined pull+convert+cache pipeline.
    // Skips pull/convert if cached base image already exists.
    Prepare(ctx context.Context, ref string) (*ImageIdentity, string, error)

    // VerifyBootability checks if an image meets the boot contract
    // (kernel + initrd/bootloader + systemd).
    VerifyBootability(ctx context.Context, imagePath string) (*BootCheckResult, error)

    // ListCached returns all cached base images.
    ListCached(ctx context.Context) ([]*CachedImage, error)

    // RemoveCached removes a cached base image by base_key.
    RemoveCached(ctx context.Context, baseKey string) error
}
```

Key types: `ImageIdentity` (checksum, arch, full digest, source ref, image type), `ImageType` (OCI/URL/LocalFile), `BootCheckResult`, `CachedImage`. The concrete implementation is `image/pipeline/manager.go`.

### 2.3 Storage Interfaces

Storage is split into three focused interfaces rather than one monolithic `StorageManager`:

```go
package storage

import "time"

// ReferenceCounter manages base image reference counts.
// All mutations hold references.lock (flock, Level 2).
type ReferenceCounter interface {
    AddReference(baseKey, vmID, digestFull, sourceRef string) error
    RemoveReference(baseKey, vmID string) error
    GetReferences(baseKey string) ([]string, error)
    IsReferenced(baseKey string) (bool, error)
    GetUnreferencedImages() ([]string, error)
}

// COWManager manages copy-on-write overlay images backed by qcow2.
type COWManager interface {
    CreateBaseImage(srcPath, baseKey string) error
    CreateOverlay(baseKey, vmID, diskSize string) (overlayPath string, err error)
    RemoveOverlay(vmID string) error
    GetOverlayInfo(vmID string) (*OverlayInfo, error)
}

// GarbageCollector reclaims unreferenced storage resources.
// Locking order: gc.lock (Level 1) → references.lock (Level 2).
type GarbageCollector interface {
    CollectUnreferencedImages(gracePeriod time.Duration) ([]string, error)
    CollectOrphanedOverlays() ([]string, error)
    CollectTempFiles(maxAge time.Duration) ([]string, error)
    EmptyTrash(maxAge time.Duration) error
    FullGC() error
}
```

Concrete implementations live in `storage/local/` (refcount.go, cow.go, gc.go).

### 2.4 VM Manager Interface

The `vm.Manager` interface handles the full VM lifecycle including creation, state transitions, name resolution, and reconciliation:

```go
package vm

import (
    "context"
    "time"

    "github.com/CMGS/cocoon/types"
)

type Manager interface {
    // CRUD operations.
    Create(ctx context.Context, opts *CreateOptions) (*types.VMConfig, error)
    Start(ctx context.Context, vmID string) error
    Stop(ctx context.Context, vmID string, timeout time.Duration) error
    Delete(ctx context.Context, vmID string, force bool) error
    Inspect(ctx context.Context, vmID string) (*types.VMInspect, error)
    List(ctx context.Context) ([]*types.VMInspect, error)

    // Name resolution: "vm-" prefix → vm_id lookup, otherwise → name-index.json.
    ResolveVMRef(ref string) (string, error)

    // State management.
    TransitionState(vmID string, to types.VMState, reason string) error
    LoadConfig(vmID string) (*types.VMConfig, error)
    LoadMetadata(vmID string) (*types.VMMetadataFile, error)
    SaveMetadata(meta *types.VMMetadataFile) error

    // Reconciliation (used by cocoon doctor).
    Reconcile(ctx context.Context, fix bool, force bool) ([]Inconsistency, error)
}
```

The concrete implementation is `vm/engine/manager.go`. `Start()` implements the boot fallback strategy: `pvh_then_uefi` attempts PVH first, logs a warning, then falls back to UEFI only for firmware-related errors.

### 2.5 Manager Initialization

Cocoon does not use a factory pattern. All managers are constructed directly in `cmd/cocoon/app.go`:

```go
// appContext holds initialized managers for CLI commands.
type appContext struct {
    cfg    *config.CocoonConfig
    vmMgr  vm.Manager
    imgMgr image.Manager
    hyper  hypervisor.Client
    refCtr storage.ReferenceCounter
    cowMgr storage.COWManager
    gc     storage.GarbageCollector
}

func initApp(_ *cli.Context) (*appContext, error) {
    // Phase 1: require root on Linux.
    if runtime.GOOS == "linux" && os.Geteuid() != 0 {
        return nil, fmt.Errorf("cocoon requires root privileges (Phase 1 rootful mode)")
    }

    cfg, err := config.LoadConfig(configPath)
    // ... apply --root-dir, --runtime-dir, --log-dir overrides ...

    hyper := cloudhypervisor.New(cfg)
    refCtr := local.NewReferenceCounter(cfg)
    cowMgr := local.NewCOWManager(cfg)
    gc := local.NewGarbageCollector(cfg)
    imgMgr := pipeline.New(cfg, refCtr)
    vmMgr := engine.New(cfg, hyper, refCtr, cowMgr, imgMgr)

    return &appContext{cfg, vmMgr, imgMgr, hyper, refCtr, cowMgr, gc}, nil
}
```

The `initApp()` function is called by every CLI command that needs manager access (all commands except `init` and `version`).

---

## 3. VM Identifier Resolution

All CLI commands that operate on a specific VM accept a `<vm-ref>` positional argument. A `<vm-ref>` can be either a `vm_id` or a `name`. Resolution follows a deterministic algorithm with no ambiguity.

For the full identifier rules (format, uniqueness, mutability), see [07-vm-lifecycle.md § 1.4](./07-vm-lifecycle.md#14-vm-identifier-rules).

### 3.1 Resolution Algorithm

1. If `<vm-ref>` starts with `vm-`: treat as exact `vm_id` lookup in `/var/lib/cocoon/vms/{vm_id}/config.json`.
2. Otherwise: look up `<vm-ref>` in the name index (`/var/lib/cocoon/db/name-index.json`).
3. If no match: exit with error `"VM not found: <vm-ref>"`.

No prefix-matching, substring matching, or fuzzy matching is supported.

### 3.2 Identifier Summary

| Identifier | Format | Example | Mutable? | Used In |
|------------|--------|---------|----------|---------|
| `vm_id` | `vm-{ulid}` | `vm-01HXYZ5A3B7C8D9E0F1G2H3J4K` | Never | Directories, logs, sockets, locks |
| `name` | User-chosen or auto-generated | `myvm`, `cocoon-a3f7b2c1` | Immutable after create | CLI commands, display, name index |

### 3.3 Resolution Examples

```bash
# By name (most common)
cocoon start myvm
cocoon stop myvm
cocoon inspect myvm

# By vm_id (for automation / scripting)
cocoon start vm-01HXYZ5A3B7C8D9E0F1G2H3J4K
cocoon inspect vm-01HXYZ5A3B7C8D9E0F1G2H3J4K

# Error: VM not found
$ cocoon inspect nonexistent
Error: VM not found: nonexistent

# Error: ambiguity is impossible — names are globally unique
# If "myvm" exists in the name index, it resolves to exactly one vm_id.
```

---

## 4. CLI Commands

Using `urfave/cli/v2` (same as core project):

### 4.1 Main Application Structure

```go
package main

import (
    "context"
    "fmt"
    "os"
    "os/signal"
    "syscall"

    cli "github.com/urfave/cli/v2"

    "github.com/CMGS/cocoon/version"
)

var (
    configPath string
    rootDir    string
    runtimeDir string
    logDir     string
    logLevel   string
)

func main() {
    cli.VersionPrinter = func(_ *cli.Context) {
        fmt.Print(version.String())
    }

    app := cli.NewApp()
    app.Name = version.NAME
    app.Usage = "Lightweight VM manager built on Cloud Hypervisor"
    app.Version = version.VERSION
    app.Flags = []cli.Flag{
        &cli.StringFlag{
            Name:        "config",
            Value:       "/etc/cocoon/config.json",
            Usage:       "config file path for cocoon",
            Destination: &configPath,
            EnvVars:     []string{"COCOON_CONFIG_PATH"},
        },
        &cli.StringFlag{
            Name:        "root-dir",
            Usage:       "root directory for cocoon persistent data (overrides config)",
            Destination: &rootDir,
            EnvVars:     []string{"COCOON_ROOT_DIR"},
        },
        &cli.StringFlag{
            Name:        "runtime-dir",
            Usage:       "runtime directory for sockets and PIDs (overrides config)",
            Destination: &runtimeDir,
            EnvVars:     []string{"COCOON_RUNTIME_DIR"},
        },
        &cli.StringFlag{
            Name:        "log-dir",
            Usage:       "log directory for VM serial logs (overrides config)",
            Destination: &logDir,
            EnvVars:     []string{"COCOON_LOG_DIR"},
        },
        &cli.StringFlag{
            Name:        "log-level",
            Value:       "info",
            Usage:       "log level (debug, info, warn, error)",
            Destination: &logLevel,
            EnvVars:     []string{"COCOON_LOG_LEVEL"},
        },
    }

    app.Commands = []*cli.Command{
        initCommand(),
        createCommand(),
        runCommand(),
        startCommand(),
        stopCommand(),
        killCommand(),
        rmCommand(),
        psCommand(),
        inspectCommand(),
        logsCommand(),
        imagesCommand(),
        gcCommand(),
        firmwareCommand(),
        doctorCommand(),
        versionCommand(),
    }

    os.Exit(run(app))
}

func run(app *cli.App) int {
    ctx, stop := signal.NotifyContext(context.TODO(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
    defer stop()

    if err := app.RunContext(ctx, os.Args); err != nil {
        fmt.Fprintf(os.Stderr, "Error: %v\n", err)
        return 1
    }
    return 0
}
```

### 4.2 cocoon init (Initialize Environment)

**Command**: `cocoon init [FLAGS]`

**Purpose**: Initialize the Cocoon directory tree, write a default config file, and optionally download firmware. This is typically the first command run after installing Cocoon.

**Note**: `cocoon init` does NOT call `initApp()` — it builds a config from `DefaultConfig()` and applies CLI overrides directly. This means it works without an existing config file or directories.

```go
func initCommand() *cli.Command {
    return &cli.Command{
        Name:  "init",
        Usage: "Initialize cocoon directories and default config",
        Flags: []cli.Flag{
            &cli.BoolFlag{
                Name:  "force",
                Usage: "overwrite existing config file and re-download firmware",
            },
            &cli.StringFlag{
                Name:  "with-pvh-firmware",
                Usage: "download PVH firmware from `URL`",
            },
            &cli.StringFlag{
                Name:  "with-uefi-firmware",
                Usage: "download UEFI firmware from `URL`",
            },
        },
        Action: initAction,
    }
}
```

**Behavior**:

1. Build config from `config.DefaultConfig()`, apply `--root-dir`, `--runtime-dir`, `--log-dir` overrides
2. Create all directories via `cfg.EnsureDirs()` (db/, cache/images/, cache/manifests/, cache/locks/, vms/, temp/, trash/, firmware/, buildah/, runtime/vms/, log/)
3. If `--with-pvh-firmware URL`: download to `firmware/hypervisor-fw` (0755), atomic rename
4. If `--with-uefi-firmware URL`: download to `firmware/CLOUDHV.fd` (0644), atomic rename
5. Write `config.json` to `--config` path (default `/etc/cocoon/config.json`); skip if exists unless `--force`
6. Print "Done. Run 'cocoon doctor' to verify system dependencies."

**Example Usage**:

```bash
# Basic initialization
sudo cocoon init

# Initialize with firmware download
sudo cocoon init \
  --with-pvh-firmware https://github.com/cloud-hypervisor/rust-hypervisor-firmware/releases/download/0.5.0/hypervisor-fw \
  --with-uefi-firmware https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v50.0/CLOUDHV.fd

# Force re-initialization
sudo cocoon init --force

# Development setup with custom paths
COCOON_CONFIG_PATH=./dev/config.json \
COCOON_ROOT_DIR=./dev/lib \
COCOON_RUNTIME_DIR=./dev/run \
COCOON_LOG_DIR=./dev/log \
cocoon init
```

### 4.3 cocoon run (Create and Start)

**Command**: `cocoon run IMAGE [FLAGS]`

**Purpose**: Create and start a VM in one operation

**Implementation**: Based on [Boot Contract §4.2](./01-boot-contract.md#42-cocoon-run-create-and-start)

```go
// vmCreateFlags returns the common flags shared by both create and run commands.
func vmCreateFlags() []cli.Flag {
    return []cli.Flag{
        &cli.StringFlag{
            Name:    "name",
            Aliases: []string{"n"},
            Usage:   "VM name (globally unique; auto-generated if omitted)",
        },
        &cli.IntFlag{
            Name:    "cpus",
            Aliases: []string{"c"},
            Value:   types.DefaultCPUs,
            Usage:   "number of vCPUs",
        },
        &cli.StringFlag{
            Name:    "memory",
            Aliases: []string{"m"},
            Value:   fmt.Sprintf("%dM", types.DefaultMemoryMB), // "2048M"
            Usage:   "memory size (e.g., 512M, 1G, 2G, 2048)",
        },
        &cli.StringFlag{
            Name:  "disk",
            Value: types.DefaultDiskSize,
            Usage: "root disk overlay size (e.g., 10G, 20G)",
        },
        &cli.StringFlag{
            Name:  "boot-strategy",
            Value: string(types.DefaultBootStrategy),
            Usage: "boot strategy: pvh_then_uefi, uefi_only, pvh_only",
        },
        &cli.BoolFlag{
            Name:  "skip-verify",
            Usage: "skip bootability verification of the image",
        },
    }
}

func runCommand() *cli.Command {
    flags := vmCreateFlags()
    flags = append(flags,
        &cli.BoolFlag{
            Name:    "detach",
            Aliases: []string{"d"},
            Usage:   "run VM in background",
        },
        &cli.BoolFlag{
            Name:  "rm",
            Usage: "automatically delete the VM when it stops",
        },
    )
    return &cli.Command{
        Name:      "run",
        Usage:     "Create and start a VM from an image",
        ArgsUsage: "IMAGE",
        Flags:     flags,
        Action:    runAction,
    }
}
```

**Behavior**:

1. **Create VM Configuration**: Generate VM ID, resolve image, prepare COW overlay
2. **Start Cloud Hypervisor**: Launch CH process, configure via REST, boot VM
3. **Wait for Boot**: Poll serial log for boot completion (timeout: config default)
4. **Print VM ID**: Output the created VM ID
5. **Background behavior**: VM runs as a background CH process. Serial log is written to disk; use `cocoon logs --follow` to stream.
6. **Auto-remove** (if `--rm`): The `AutoRemove` flag is recorded in metadata. When the VM is stopped via `cocoon stop`, the delete flow is triggered automatically. Note: if the VM crashes or is killed externally, auto-remove does not fire. Use `cocoon doctor --fix` for state reconciliation; automatic deletion of crashed `auto_remove` VMs is a future enhancement.

**Example Usage**:

```bash
# Run Ubuntu VM with 4 CPUs and 4GB memory
cocoon run ubuntu-22.04-cloudimg --name myvm --cpus 4 --memory 4G

# Run VM with auto-remove on stop
cocoon run --rm ubuntu-22.04-cloudimg --name temp-vm

# Run with PVH-only boot strategy (no UEFI fallback)
cocoon run --boot-strategy pvh_only ubuntu-22.04-cloudimg
```

### 4.4 cocoon create (Prepare VM)

**Command**: `cocoon create IMAGE [FLAGS]`

**Purpose**: Create a VM without starting it

**IMAGE Parameter**:
- **Positional argument** (required): Image path, URL, or OCI reference
- **Phase 1 Support** (Current):
  - Cloud image qcow2: `/path/to/ubuntu-22.04-cloudimg.qcow2`
  - Cloud image URL: `https://cloud-images.ubuntu.com/.../ubuntu-22.04.img`
  - **OCI→qcow2 conversion**: Supported with strict bootability validation
    - Bootable OCI images: Converted and used successfully
    - Non-bootable OCI images: **Fail with clear error** (missing kernel/bootloader)
- **Phase 2 Support** (Planned):
  - Auto-fix non-bootable OCI images during conversion

```go
func createCommand() *cli.Command {
    return &cli.Command{
        Name:      "create",
        Usage:     "Create a VM from an image without starting it",
        ArgsUsage: "IMAGE",
        Flags:     vmCreateFlags(),
        Action:    createAction,
    }
}

// createAction implementation (illustrative)
func createAction(c *cli.Context) error {
    opts, err := parseCreateOptions(c, "create")
    if err != nil {
        return err
    }

    app, err := initApp(c)
    if err != nil {
        return err
    }

    vmCfg, err := app.vmMgr.Create(c.Context, opts)
    if err != nil {
        return fmt.Errorf("create VM: %w", err)
    }

    fmt.Printf("%s\n", vmCfg.VMID)
    return nil
}
```

The `createCommand` uses the same `vmCreateFlags()` as `runCommand`, which includes `--name`, `--cpus`, `--memory` (default "2048M"), `--disk`, `--boot-strategy`, and `--skip-verify`.

**Example Usage**:

```bash
# Create from local cloud image (qcow2) with explicit name
cocoon create /var/lib/cocoon/cache/images/ubuntu-22.04-cloudimg.qcow2 --name myvm --cpus 4 --memory 8G

# Create without --name (auto-generates name like "cocoon-a3f7b2c1")
cocoon create ubuntu-22.04-cloudimg --cpus 2 --memory 2G

# Create from cloud image URL (downloads and caches)
cocoon create https://cloud-images.ubuntu.com/releases/22.04/release/ubuntu-22.04-server-cloudimg-amd64.img \
  --name myvm --cpus 4 --memory 8G

# Create from OCI image (Phase 1: requires bootable OCI, else error)
cocoon create myorg/ubuntu-bootable:22.04 --name myvm
# Error if not bootable: "Image is not bootable - missing kernel/bootloader"

# Error: name already taken
$ cocoon create ubuntu-22.04-cloudimg --name myvm
Error: VM name 'myvm' already exists (used by vm-01HXYZ5A3B7C8D9E0F1G2H3J4K)
```

### 4.5 cocoon start (Boot VM)

**Command**: `cocoon start <vm-ref> [FLAGS]`

**Purpose**: Start a previously created VM. `<vm-ref>` is resolved via [Section 3](#3-vm-identifier-resolution).

**Implementation**: Based on [Boot Contract §4.2](./01-boot-contract.md#42-cocoon-run-create-and-start)

```go
func startCommand() *cli.Command {
    return &cli.Command{
        Name:      "start",
        Usage:     "Start a stopped VM",
        ArgsUsage: "VM_REF",
        Flags: []cli.Flag{
            &cli.IntFlag{
                Name:  "boot-timeout",
                Usage: "seconds to wait for VM to boot (0 = use config default)",
            },
        },
        Action: startAction,
    }
}
```

**Example Usage**:

```bash
# Start VM
cocoon start myvm

# Start with custom boot timeout (seconds)
cocoon start myvm --boot-timeout 120
```

### 4.6 cocoon stop (Graceful Shutdown)

**Command**: `cocoon stop <vm-ref> [FLAGS]`

**Purpose**: Gracefully stop a running VM. `<vm-ref>` is resolved via [Section 3](#3-vm-identifier-resolution).

**Implementation**: Based on [Boot Contract §4.3](./01-boot-contract.md#43-cocoon-stop-graceful-shutdown)

```go
func stopCommand() *cli.Command {
    return &cli.Command{
        Name:      "stop",
        Usage:     "Stop a running VM",
        ArgsUsage: "VM_REF",
        Flags: []cli.Flag{
            &cli.DurationFlag{
                Name:  "timeout",
                Usage: "graceful shutdown timeout",
                Value: 30 * time.Second,
            },
        },
        Action: stopAction,
    }
}
```

**Behavior**:

1. **Check VM State**: Verify VM is running
2. **Send ACPI Shutdown**: Trigger graceful shutdown via Cloud Hypervisor API
3. **Wait for Graceful Shutdown**: Wait up to `--timeout` duration
4. **Force Kill on Timeout**: If timeout reached, send SIGKILL
5. **Verify Shutdown**: Confirm Cloud Hypervisor process exited

For immediate force termination (SIGKILL without waiting), use `cocoon kill` instead.

**Example Usage**:

```bash
# Graceful stop with 30s timeout (default)
cocoon stop myvm

# Custom timeout
cocoon stop myvm --timeout 60s

# Immediate force kill (no graceful period) — use kill command
cocoon kill myvm
```

### 4.7 cocoon delete (Remove VM)

**Command**: `cocoon delete <vm-ref> [FLAGS]`

**Purpose**: Delete VM and cleanup storage. `<vm-ref>` is resolved via [Section 3](#3-vm-identifier-resolution).

**Implementation**: Based on [Boot Contract §4.4](./01-boot-contract.md#44-cocoon-delete-remove-resources)

```go
func rmCommand() *cli.Command {
    return &cli.Command{
        Name:      "delete",
        Aliases:   []string{"rm"},
        Usage:     "Remove a VM and cleanup storage",
        ArgsUsage: "VM_REF",
        Flags: []cli.Flag{
            &cli.BoolFlag{
                Name:    "force",
                Aliases: []string{"f"},
                Usage:   "force delete even if VM is running",
            },
        },
        Action: rmAction,
    }
}
```

**Behavior**:

1. **Check VM State**: If running and no `--force`, error
2. **Stop VM** (if `--force`): Call stop with 10s timeout
3. **Remove References**: Update reference counter ([05-storage-management.md](./05-storage-management.md))
4. **Delete Resources**: Remove overlay, serial log, config files, VM directory
5. **Remove from Name Index**: Remove `name → vm_id` mapping

Delete always removes all VM resources (overlay, config, metadata, serial log). There is no separate "volumes" concept in Phase 1 — each VM has exactly one overlay disk that is always cleaned up.

**Example Usage**:

```bash
# Delete stopped VM
cocoon delete myvm

# Force delete running VM
cocoon delete myvm --force
```

### 4.8 cocoon kill (Force Terminate)

**Command**: `cocoon kill <vm-ref>`

**Purpose**: Force terminate a VM (SIGKILL). `<vm-ref>` is resolved via [Section 3](#3-vm-identifier-resolution).

**Implementation**: Based on [Boot Contract §4.5](./01-boot-contract.md#45-cocoon-kill-force-terminate)

```go
func killCommand() *cli.Command {
    return &cli.Command{
        Name:      "kill",
        Usage:     "Force-terminate a VM immediately (SIGKILL)",
        ArgsUsage: "VM_REF",
        Action:    killAction,
    }
}
```

**Example Usage**:

```bash
# Force kill hung VM
cocoon kill myvm
```

### 4.9 cocoon list (List VMs)

**Command**: `cocoon list [FLAGS]`

**Purpose**: List all VMs

```go
func psCommand() *cli.Command {
    return &cli.Command{
        Name:    "list",
        Aliases: []string{"ps", "ls"},
        Usage:   "List VMs",
        Flags: []cli.Flag{
            &cli.BoolFlag{
                Name:    "all",
                Aliases: []string{"a"},
                Usage:   "show all VMs (including stopped)",
            },
            &cli.StringFlag{
                Name:  "format",
                Value: "table",
                Usage: "output format (table, json)",
            },
            &cli.BoolFlag{
                Name:    "quiet",
                Aliases: []string{"q"},
                Usage:   "only display VM IDs",
            },
            &cli.StringFlag{
                Name:  "filter",
                Usage: "filter by field (e.g., state=running)",
            },
        },
        Action: psAction,
    }
}
```

**Example Usage**:

```bash
# List running VMs
cocoon list

# List all VMs
cocoon list --all

# List in JSON format
cocoon list --format json

# Filter by state
cocoon list --filter state=running
```

**Output Example**:

```
VM ID                              NAME              STATE     CPUS  MEMORY  CREATED
vm-01HXYZ5A3B7C8D9E0F1G2H3J4K     myvm              RUNNING   2     2048MB  2026-02-11T10:30:00Z
vm-01HABC9D8E7F6G5H4J3K2L1M0N     devbox            STOPPED   4     4096MB  2026-02-10T14:20:00Z
vm-01H9ZZ8Y7X6W5V4U3T2S1R0Q9P     cocoon-a3f7b2c1   RUNNING   2     2048MB  2026-02-09T08:15:00Z
```

Note: The `NAME` column shows the user-provided name or the auto-generated name (`cocoon-{random}` if `--name` was omitted at create time). Either the `VM ID` or `NAME` can be used as a `<vm-ref>` in subsequent commands. The `MEMORY` column displays the value with an `MB` suffix (e.g., `2048MB`). The `CREATED` column shows the creation timestamp in RFC 3339 format.

### 4.10 cocoon inspect (VM Details)

**Command**: `cocoon inspect <vm-ref> [FLAGS]`

**Purpose**: Display detailed VM information. `<vm-ref>` is resolved via [Section 3](#3-vm-identifier-resolution).

```go
func inspectCommand() *cli.Command {
    return &cli.Command{
        Name:      "inspect",
        Usage:     "Display detailed VM information",
        ArgsUsage: "VM_REF",
        Flags: []cli.Flag{
            &cli.StringFlag{
                Name:  "format",
                Usage: "output format (json)",
                Value: "json",
            },
        },
        Action: inspectAction,
    }
}
```

**Example Usage**:

```bash
# Inspect VM in JSON
cocoon inspect myvm
```

**Output Example**:

The inspect output merges data from `config.json` (immutable) and `metadata.json` (runtime state). See [07-vm-lifecycle.md § 5](./07-vm-lifecycle.md#5-vm-configuration-schema) for the schema details.

```json
{
  "vm_id": "vm-01HXYZ5A3B7C8D9E0F1G2H3J4K",
  "name": "myvm",
  "state": "RUNNING",
  "image": {
    "ref": "ubuntu-22.04-cloudimg",
    "digest": "ef015678abcd1234567890abcdef1234567890abcdef1234567890abcdef1234",
    "base_key": "ef015678abcd1234_amd64"
  },
  "storage": {
    "overlay_path": "/var/lib/cocoon/vms/vm-01HXYZ5A3B7C8D9E0F1G2H3J4K/overlay.qcow2",
    "base_path": "/var/lib/cocoon/cache/images/ef015678abcd1234_amd64.qcow2",
    "size": "10G"
  },
  "hypervisor": {
    "ch_socket": "/run/cocoon/vms/vm-01HXYZ5A3B7C8D9E0F1G2H3J4K/api.sock",
    "ch_pid": 12345,
    "serial_log": "/var/log/cocoon/vm-01HXYZ5A3B7C8D9E0F1G2H3J4K-serial.log"
  },
  "boot_config": {
    "cpus": 2,
    "memory_mb": 2048,
    "boot_strategy": "pvh_then_uefi",
    "firmware_path": "/var/lib/cocoon/firmware/hypervisor-fw"
  },
  "timestamps": {
    "created_at": "2026-02-11T10:30:00Z",
    "updated_at": "2026-02-11T10:30:07Z",
    "started_at": "2026-02-11T10:30:05Z"
  },
  "runtime": {
    "boot_time": "2.3s",
    "last_boot_mode": "pvh",
    "error_count": 0
  }
}
```

### 4.11 cocoon logs (Serial Console Output)

**Command**: `cocoon logs <vm-ref> [FLAGS]`

**Purpose**: View VM serial console logs. `<vm-ref>` is resolved via [Section 3](#3-vm-identifier-resolution).

```go
func logsCommand() *cli.Command {
    return &cli.Command{
        Name:      "logs",
        Usage:     "View VM serial console logs",
        ArgsUsage: "VM_REF",
        Flags: []cli.Flag{
            &cli.BoolFlag{
                Name:    "follow",
                Aliases: []string{"f"},
                Usage:   "follow log output",
            },
            &cli.IntFlag{
                Name:  "tail",
                Value: 100,
                Usage: "number of lines to show from the end",
            },
            &cli.BoolFlag{
                Name:    "timestamps",
                Aliases: []string{"t"},
                Usage:   "prefix each line with a timestamp",
            },
        },
        Action: logsAction,
    }
}
```

**Example Usage**:

```bash
# View last 100 lines
cocoon logs myvm

# Follow logs in real-time
cocoon logs myvm --follow

# Show last 50 lines with timestamps
cocoon logs myvm --tail 50 --timestamps
```

### 4.12 cocoon image (Image Management)

**Command**: `cocoon image SUBCOMMAND [FLAGS]`

**Purpose**: Manage VM images (multi-source: qcow2 files, cloud image URLs, OCI references)

```go
func imagesCommand() *cli.Command {
    return &cli.Command{
        Name:  "image",
        Usage: "Manage VM images",
        Subcommands: []*cli.Command{
            {
                Name:    "list",
                Aliases: []string{"ls"},
                Usage:   "List cached VM images",
                Flags: []cli.Flag{
                    &cli.StringFlag{
                        Name:  "format",
                        Value: "table",
                        Usage: "output format (table, json)",
                    },
                },
                Action: imagesAction,
            },
            {
                Name:      "pull",
                Usage:     "Pull and cache an image without creating a VM",
                ArgsUsage: "IMAGE_REF",
                Action:    imagePullAction,
            },
            {
                Name:      "inspect",
                Usage:     "Show details of a cached image (size, checksum, ref count)",
                ArgsUsage: "IMAGE_REF",
                Flags: []cli.Flag{
                    &cli.StringFlag{
                        Name:  "format",
                        Value: "json",
                        Usage: "output format (json)",
                    },
                },
                Action: imageInspectAction,
            },
            {
                Name:      "remove",
                Aliases:   []string{"rm"},
                Usage:     "Remove a cached image (only if unreferenced)",
                ArgsUsage: "IMAGE_REF",
                Action:    imageRemoveAction,
            },
            {
                Name:      "verify",
                Usage:     "Check if an image is bootable",
                ArgsUsage: "IMAGE_REF",
                Flags: []cli.Flag{
                    &cli.StringFlag{
                        Name:  "format",
                        Value: "table",
                        Usage: "output format (table, json)",
                    },
                },
                Action: imageVerifyAction,
            },
        },
    }
}
```

**Image Source Detection** (`imagePullAction` auto-detects source type):

| Pattern | Detected Type | Action |
|---------|---------------|--------|
| `/path/to/*.qcow2` or `/path/to/*.img` | `qcow2` | Validate file, copy/link to cache |
| `https://...` or `http://...` | `url` | Download, validate, cache |
| `registry/repo:tag` or `repo:tag` | `oci` | Pull via Buildah, convert to qcow2, validate bootability, cache |

**Example Usage**:

```bash
# Pull cloud image from URL (recommended)
cocoon image pull https://cloud-images.ubuntu.com/releases/22.04/release/ubuntu-22.04-server-cloudimg-amd64.img

# Import local qcow2 file into cache
cocoon image pull /tmp/ubuntu-22.04-cloudimg.qcow2

# Pull bootable OCI image (custom-built, requires root for conversion)
cocoon image pull myorg/ubuntu-bootable:22.04

# List cached images
cocoon image list

# Inspect image
cocoon image inspect ubuntu-22.04-cloudimg

# Verify bootability
cocoon image verify ubuntu-22.04-cloudimg

# Remove image
cocoon image rm ubuntu-22.04-cloudimg
```

**Error Scenarios**:

```bash
# ❌ WRONG: Attempting to pull regular container image
$ cocoon image pull ubuntu:22.04

Pulling ubuntu:22.04...
Converting OCI image to bootable VM disk...
ERROR: Bootability check failed

╭─────────────────────────────────────────────────────────╮
│ This is NOT a bootable VM image                         │
╰─────────────────────────────────────────────────────────╯

The image 'ubuntu:22.04' is a regular container image and
cannot be booted as a VM. It is missing:

  ✗ Kernel (/boot/vmlinuz*)
  ✗ Initrd (/boot/initrd*)
  ✗ Bootloader (GRUB/systemd-boot)
  ✗ Init system (systemd)

Cocoon requires bootable VM images, not application containers.

Solutions:

  1. Use Cloud Hypervisor native cloud images (recommended):
     cocoon image pull https://cloud-images.ubuntu.com/releases/22.04/release/ubuntu-22.04-server-cloudimg-amd64.img

  2. [Phase 2] Build a bootable OCI image with:
     cocoon image build-bootable --base ubuntu:22.04 --output myorg/ubuntu-bootable:22.04

  3. See docs/00-overview.md#supported-image-contract for details

Exit code: 1
```

### 4.13 cocoon gc (Garbage Collection)

**Command**: `cocoon gc [FLAGS]`

**Purpose**: Run garbage collection to cleanup unused resources

**Implementation**: Based on [05-storage-management.md](./05-storage-management.md)

```go
func gcCommand() *cli.Command {
    return &cli.Command{
        Name:  "gc",
        Usage: "Run garbage collection on unreferenced images and orphaned resources",
        Flags: []cli.Flag{
            &cli.IntFlag{
                Name:  "grace-period",
                Usage: "hours before unreferenced images are collected (0 = use config default)",
            },
            &cli.BoolFlag{
                Name:  "dry-run",
                Usage: "only report what would be collected, don't actually delete",
            },
        },
        Action: gcAction,
    }
}
```

**Example Usage**:

```bash
# Run GC with default grace period (24h)
cocoon gc

# Dry run to see what would be collected
cocoon gc --dry-run

# Run GC with custom grace period (12 hours)
cocoon gc --grace-period 12
```

### 4.14 cocoon doctor (System Health Check)

**Command**: `cocoon doctor [FLAGS]`

**Purpose**: Validate Cocoon installation and dependencies

**Implementation**: Based on [08-dependencies.md § Startup Dependency Detection](./08-dependencies.md#startup-dependency-detection-cocoon-doctor) and [Boot Contract § 1.1](./01-boot-contract.md#11-primary-boot-mode-pvh--hypervisor-fw)

```go
func doctorCommand() *cli.Command {
    return &cli.Command{
        Name:  "doctor",
        Usage: "Check system health and dependencies",
        Flags: []cli.Flag{
            &cli.BoolFlag{
                Name:  "fix",
                Usage: "attempt to fix issues automatically",
            },
            &cli.BoolFlag{
                Name:  "force",
                Usage: "force re-check even if cached results exist",
            },
            &cli.StringFlag{
                Name:  "format",
                Value: "table",
                Usage: "output format (table, json)",
            },
        },
        Action: doctorAction,
    }
}

func doctorAction(c *cli.Context) error {
    app, err := initApp(c)
    if err != nil {
        return err
    }

    fix := c.Bool("fix")
    force := c.Bool("force")
    format := c.String("format")

    // Phase 1: Dependency checks (binary existence, firmware, directories).
    checks := runDependencyChecks(app)  // exec.LookPath + os.Stat

    // Phase 2: VM reconciliation (state consistency, orphan cleanup).
    issues, reconcileErr := app.vmMgr.Reconcile(c.Context, fix, force)

    // Print results in table or JSON format.
    // --fix attempts VM state repairs (not dependency installation).
}
```

**Dependency Checks** (informational, --fix does not install missing tools):
- cloud-hypervisor binary
- ch-remote binary
- PVH firmware file
- UEFI firmware file
- qemu-img binary
- buildah binary
- skopeo binary
- guestfish binary
- /dev/kvm device
- Directory structure (root, runtime, log, db, vm, cache, buildah, firmware)

**VM Reconciliation** (--fix repairs these):
- Stale RUNNING state (CH process not found → mark STOPPED)
- Missing runtime directories → recreate
- Name index inconsistencies → rebuild from config.json files
- Orphaned VM directories → report (manual cleanup required)

**Exit Codes**:
- `0`: All checks passed (returns nil)
- `1`: One or more checks failed or error occurred (returns error)

**Example Usage**:

```bash
# Quick health check
cocoon doctor

# Fix VM state issues (reconcile stale states, rebuild name index)
cocoon doctor --fix
```

**Example Output**:

```
$ cocoon doctor
=== Dependency Checks ===
CHECK              STATUS  DETAIL
cloud-hypervisor   pass    /usr/bin/cloud-hypervisor
pvh-firmware       pass    /var/lib/cocoon/firmware/hypervisor-fw
uefi-firmware      fail    not found at /var/lib/cocoon/firmware/CLOUDHV.fd
qemu-img           pass    /usr/bin/qemu-img
ch-remote          pass    /usr/bin/ch-remote
buildah            pass    /usr/bin/buildah
skopeo             pass    /usr/bin/skopeo
guestfish          fail    binary not found in PATH (required for OCI-to-qcow2 conversion)
kvm                pass    /dev/kvm
dir/root-dir       pass    /var/lib/cocoon
dir/runtime-dir    pass    /run/cocoon
dir/log-dir        pass    /var/log/cocoon

2 dependency check(s) failed.

=== VM Reconciliation ===
No VM issues found.
```

**Example Output (with --fix)**:

```
$ cocoon doctor --fix
=== Dependency Checks ===
CHECK              STATUS  DETAIL
cloud-hypervisor   pass    /usr/bin/cloud-hypervisor
pvh-firmware       pass    /var/lib/cocoon/firmware/hypervisor-fw
...

All dependency checks passed.

=== VM Reconciliation ===
VM ID             TYPE            SEVERITY  DETAILS
vm-01HXYZ5A3B...  stale_running   warning   CH process not found, marked STOPPED
vm-01HABC9D8E...  name_index      info      rebuilt name index entry

Attempted to fix 2 issue(s).
```

---

### 4.15 cocoon firmware (Firmware Management)

**Command**: `cocoon firmware <subcommand> [FLAGS]`

**Purpose**: Manage hypervisor firmware files (PVH and UEFI)

**Implementation**: Based on [Boot Contract § 1.1](./01-boot-contract.md#11-primary-boot-mode-pvh--hypervisor-fw)

```go
func firmwareCommand() *cli.Command {
    return &cli.Command{
        Name:  "firmware",
        Usage: "Manage firmware files",
        Subcommands: []*cli.Command{
            {
                Name:    "list",
                Aliases: []string{"ls"},
                Usage:   "List installed firmware files with paths and sizes",
                Flags: []cli.Flag{
                    &cli.StringFlag{
                        Name:  "format",
                        Value: "table",
                        Usage: "output format (table, json)",
                    },
                },
                Action: firmwareListAction,
            },
            {
                Name:  "verify",
                Usage: "Check firmware files exist and are accessible",
                Action: firmwareVerifyAction,
            },
            {
                Name:  "install",
                Usage: "Download and install firmware files",
                Flags: []cli.Flag{
                    &cli.StringFlag{
                        Name:  "pvh-url",
                        Usage: "download PVH firmware (hypervisor-fw) from URL",
                    },
                    &cli.StringFlag{
                        Name:  "uefi-url",
                        Usage: "download UEFI firmware (CLOUDHV.fd) from URL",
                    },
                    &cli.BoolFlag{
                        Name:  "force",
                        Usage: "re-download even if firmware files already exist",
                    },
                },
                Action: firmwareInstallAction,
            },
            {
                Name:  "update",
                Usage: "Update firmware files (alias for install)",
                Flags: []cli.Flag{
                    &cli.StringFlag{
                        Name:  "pvh-url",
                        Usage: "download PVH firmware (hypervisor-fw) from URL",
                    },
                    &cli.StringFlag{
                        Name:  "uefi-url",
                        Usage: "download UEFI firmware (CLOUDHV.fd) from URL",
                    },
                    &cli.BoolFlag{
                        Name:  "force",
                        Usage: "re-download even if firmware files already exist",
                    },
                },
                Action: firmwareInstallAction,
            },
        },
    }
}
```

**Subcommands**:

#### 4.15.1 cocoon firmware list

List all installed firmware files with paths and sizes.

```
$ cocoon firmware list
NAME             TYPE  PATH                                       SIZE     EXISTS
hypervisor-fw    PVH   /var/lib/cocoon/firmware/hypervisor-fw     89.2KB   true
CLOUDHV.fd       UEFI  /var/lib/cocoon/firmware/CLOUDHV.fd        2.1MB    true
```

#### 4.15.2 cocoon firmware install

Download and install firmware files from explicit URLs using `--pvh-url` and `--uefi-url` flags.

```bash
# Install PVH firmware from URL
cocoon firmware install --pvh-url https://github.com/cloud-hypervisor/rust-hypervisor-firmware/releases/download/0.5.0/hypervisor-fw

# Install UEFI firmware from URL
cocoon firmware install --uefi-url https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v50.0/CLOUDHV.fd

# Install both at once
cocoon firmware install \
  --pvh-url https://github.com/cloud-hypervisor/rust-hypervisor-firmware/releases/download/0.5.0/hypervisor-fw \
  --uefi-url https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v50.0/CLOUDHV.fd

# Force re-download
cocoon firmware install --pvh-url URL --force
```

**Installation Process**:
1. HTTP GET the firmware from the provided URL to a temporary file
2. Atomic rename the temporary file to the target path (`/var/lib/cocoon/firmware/`)
3. No checksum verification, no backup of existing files

**Example Output**:

```bash
$ cocoon firmware install --pvh-url https://github.com/cloud-hypervisor/rust-hypervisor-firmware/releases/download/0.5.0/hypervisor-fw
Firmware install complete.
```

#### 4.15.3 cocoon firmware verify

Check that firmware files exist and are accessible (via `os.Stat`). No checksum verification is performed.

```bash
# Verify all firmware files
cocoon firmware verify
```

**Example Output**:

```bash
$ cocoon firmware verify
OK    hypervisor-fw (PVH): /var/lib/cocoon/firmware/hypervisor-fw [89.2KB]
OK    CLOUDHV.fd (UEFI): /var/lib/cocoon/firmware/CLOUDHV.fd [2.1MB]

All firmware files verified.
```

**Firmware Types**:
- `pvh`: rust-hypervisor-firmware (PVH boot, required)
- `uefi`: CLOUDHV.fd (UEFI boot, optional; deprecated fallback: system OVMF)
- `all`: All firmware types

**Firmware Storage**:
```
/var/lib/cocoon/firmware/
├── hypervisor-fw           # Current PVH firmware (x86_64)
└── CLOUDHV.fd              # UEFI firmware (Cloud Hypervisor edk2)
```

**Exit Codes**:
- `0`: Success
- `1`: Command failed (download error, firmware not found, etc.)

**Example Usage**:

```bash
# List installed firmware
cocoon firmware list

# Install PVH firmware from URL
cocoon firmware install --pvh-url https://github.com/cloud-hypervisor/rust-hypervisor-firmware/releases/download/0.5.0/hypervisor-fw

# Install UEFI firmware from URL
cocoon firmware install --uefi-url https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v50.0/CLOUDHV.fd

# Verify firmware files exist
cocoon firmware verify

# Force re-download
cocoon firmware install --pvh-url https://github.com/cloud-hypervisor/rust-hypervisor-firmware/releases/download/0.5.0/hypervisor-fw --force
```

---

## 5. Configuration

Cocoon uses JSON configuration with sensible defaults. See `config/config.go` for the canonical struct definition.

### 5.1 Configuration Structure

The actual configuration struct is `config.CocoonConfig` (flat structure):

```go
package config

// CocoonConfig holds global Cocoon configuration.
type CocoonConfig struct {
    RootDir    string `json:"root_dir"`     // Persistent data directory
    RuntimeDir string `json:"runtime_dir"`  // Runtime directory (tmpfs)
    LogDir     string `json:"log_dir"`      // Log directory

    CHBinary         string `json:"ch_binary"`           // Cloud Hypervisor binary path
    PVHFirmwarePath  string `json:"pvh_firmware_path"`   // PVH firmware path
    UEFIFirmwarePath string `json:"uefi_firmware_path"`  // UEFI firmware path
    BuildahRoot      string `json:"buildah_root"`        // Buildah storage root

    DefaultCPUs     int    `json:"default_cpus"`      // Default vCPUs per VM
    DefaultMemoryMB int64  `json:"default_memory_mb"` // Default memory in MB
    DefaultDiskSize string `json:"default_disk_size"`  // Default overlay disk size

    GCGracePeriodHours int `json:"gc_grace_period_hours"`  // GC grace period
    GCTrashRetentDays  int `json:"gc_trash_retention_days"` // Trash retention

    BootTimeoutSeconds int `json:"boot_timeout_seconds"` // Boot timeout (seconds)
    StopTimeoutSeconds int `json:"stop_timeout_seconds"` // Stop timeout (seconds)

    BootSuccessPatterns []string `json:"boot_success_patterns,omitempty"` // Boot success regex
    BootFailurePatterns []string `json:"boot_failure_patterns,omitempty"` // Boot failure regex
}
```

### 5.2 Example Configuration File

```json
// /etc/cocoon/config.json
{
  "root_dir": "/var/lib/cocoon",
  "runtime_dir": "/run/cocoon",
  "log_dir": "/var/log/cocoon",
  "ch_binary": "cloud-hypervisor",
  "pvh_firmware_path": "/var/lib/cocoon/firmware/hypervisor-fw",
  "uefi_firmware_path": "/var/lib/cocoon/firmware/CLOUDHV.fd",
  "buildah_root": "/var/lib/cocoon/buildah",
  "default_cpus": 2,
  "default_memory_mb": 2048,
  "default_disk_size": "10G",
  "gc_grace_period_hours": 24,
  "gc_trash_retention_days": 7,
  "boot_timeout_seconds": 60,
  "stop_timeout_seconds": 30
}
```

All fields are optional — `config.DefaultConfig()` provides sensible defaults. If the config file is missing, defaults are used.

---

## 6. Implementation Flow

### 6.1 VM Creation Flow (`cocoon run`)

1. **Parse CLI flags** → Validate inputs
2. **Load configuration** → Read JSON config from `/etc/cocoon/config.json`
3. **Initialize managers** (via `initApp`, which also verifies root privileges on Linux):
   - Create `HypervisorClient`
   - Create `ImageManager` via pipeline
   - Create `COWManager` for qcow2 operations
   - Create `ReferenceCounter` for tracking
4. **Resolve and fetch image** → `ImageManager.Resolve(image)` detects source type, then `ImageManager.Pull(image)` fetches/converts/caches to local qcow2 (acquires per-image conversion lock, Level 3 — see `06-concurrency.md § Lock Hierarchy`)
   - **Rootless constraint** *(Phase 2 — Phase 1 requires root)*: In rootless mode without a helper, OCI image conversion and libguestfs-based verification are unavailable (see [08-dependencies.md § Rootless Limitations](./08-dependencies.md)). If the image source is an OCI reference, fail fast with: `"OCI conversion requires root privileges. Use a qcow2 cloud image, pre-convert with a rootful environment, or enable the hybrid helper (08-dependencies.md § Option C)."`
5. **Verify bootability** → `ImageManager.VerifyBootable(image)` (Boot Contract §6)
   - qcow2 files: inspect partitions via guestfish (rootful) or skip deep inspection (rootless — rely on boot-time detection)
   - OCI images: validate rootfs components before conversion (rootful only)
6. **Pin base image reference** (short lock hold):
   - **Acquire references.lock** (Level 2)
   - `refCounter.AddReference(baseKey, vmID, digestFull, sourceRef)` — immediately adds `vmID` to `refs[]`, protecting the base image from GC
   - **Release references.lock**
   - This "pin" ensures the base image survives even if GC runs during the subsequent (slow) steps. On failure in later steps, the cleanup path removes this reference.
7. **Create COW overlay** → `StorageManager.CreateOverlay(baseKey, vmID)`
8. **[Phase 2] Metadata server** → HTTP server on 169.254.169.254 for cloud-init is not yet implemented. Phase 1 relies on NoCloud seed files or pre-baked cloud-init config in the image.
9. **Configure VM**:
   ```go
   config := &types.VMConfig{
       VMID:          vmID,
       Name:          name,
       ImageRef:      imageRef,
       BaseKey:       identity.BaseKey,
       BaseDigestFull: identity.DigestFull,
       Arch:          identity.Arch,
       BootStrategy:  types.BootStrategy(bootStrategy),
       FirmwarePath:  firmwarePath,
       CPUs:          cpus,
       MemoryMB:      memoryMB,
       DiskSize:      c.String("disk"),
       BaseImagePath: baseImagePath,
       OverlayPath:   overlayPath,
       SerialLog:     serialLogPath,
       SocketPath:    socketPath,
       CreatedAt:     time.Now().UTC().Format(time.RFC3339),
       SchemaVersion: types.CurrentConfigSchemaVersion,
   }
   ```
10. **Start Cloud Hypervisor** (REST-first):
    ```bash
    # Launch CH process with API socket and firmware only:
    cloud-hypervisor \
      --api-socket /run/cocoon/vms/{vm_id}/api.sock \
      --firmware /var/lib/cocoon/firmware/hypervisor-fw
    ```
    Then configure the VM via CH REST API:
    ```
    PUT /api/v1/vm.create
    {
      "cpus": {"boot_vcpus": 2},
      "memory": {"size": 2147483648},
      "disks": [{"path": "/var/lib/cocoon/vms/{vm_id}/overlay.qcow2"}],
      "serial": {"mode": "File", "file": "/var/log/cocoon/{vm_id}-serial.log"},
      "console": {"mode": "Off"}
    }
    ```
    Followed by `PUT /api/v1/vm.boot` to start the VM.
11. **Wait for boot** → Poll serial log for boot completion marker (timeout: 60s)
12. **Save VM metadata** → Write `config.json` (immutable) and `metadata.json` (mutable) to VM directory (acquires per-VM `metadata.lock`, Level 4)
13. **Acquire name-index.lock** (Level 2) → Add `name → vm_id` to `name-index.json`, release lock
14. **Print VM ID** → Output `vm_id` to stdout for scripting
15. **Auto-remove bookkeeping** → If `--rm`, set `AutoRemove=true` in metadata; delete is triggered when the VM is later stopped via `cocoon stop` (crash/external-kill: `cocoon doctor --fix` performs state reconciliation; automatic deletion of crashed `auto_remove` VMs is a future enhancement)

**Failure cleanup**: If any step after 6 fails, the cleanup path must:
- **Acquire references.lock** (Level 2)
- `refCounter.RemoveReference(baseKey, vmID)` — remove the pinned reference
- **Release references.lock**
- Delete overlay, VM directory, and any partial resources

### 6.2 VM Stop Flow (`cocoon stop`)

1. **Resolve vm-ref** → `ResolveVMRef(vmRef)` (see § 3 VM Identifier Resolution)
2. **Load VM config** → Read `config.json` from VM directory
3. **Acquire per-VM metadata.lock** (Level 4)
4. **Check VM state** → Verify VM is running (read `metadata.json`)
5. **Send ACPI shutdown** → `PUT /api/v1/vm.power-button` (Boot Contract §4.3)
6. **Wait for graceful shutdown** → Poll CH process exit (timeout: 30s default)
7. **Force kill on timeout** → `syscall.Kill(chPid, syscall.SIGKILL)`
8. **Verify shutdown** → Confirm CH process terminated
9. **Update VM state** → Write `metadata.json` with state=`STOPPED`, `last_boot_mode`, timestamps
10. **Release metadata.lock**

### 6.3 VM Delete Flow (`cocoon delete`)

1. **Resolve vm-ref** → `ResolveVMRef(vmRef)` (see § 3)
2. **Load VM config** → Read `config.json` (get `base_key` for refcount)
3. **Check VM state** → If running and no `--force`, error
4. **Stop VM** (if needed) → Call stop flow with `--timeout 10s`
5. **Acquire references.lock** (Level 2)
6. **Remove reference** → `refCounter.RemoveReference(baseKey, vmID)`
7. **Release references.lock**
8. **Acquire name-index.lock** (Level 2 — never held with references.lock)
9. **Remove name from name-index.json**
10. **Release name-index.lock**
11. **Delete resources**:
    - Move overlay to trash: `mv overlay.qcow2 /var/lib/cocoon/trash/`
    - Delete serial log: `rm /var/log/cocoon/{vm_id}-serial.log`
    - [Phase 2] Stop metadata server for this VM (not yet implemented)
    - Delete API socket: `rm /run/cocoon/vms/{vm_id}/api.sock`
    - Delete VM directory: `rm -rf /var/lib/cocoon/vms/{vm_id}/`
12. **Trigger GC** (optional) → If base image unreferenced, mark for collection

---

## 7. Examples

### 7.1 Basic VM Creation

```bash
# Create and start Ubuntu VM
cocoon run ubuntu-22.04-cloudimg --name myvm --cpus 4 --memory 4G

# Run VM with custom disk size
cocoon run ubuntu-22.04-cloudimg \
  --name devvm \
  --disk 50G \
  --memory 8G \
  --cpus 4

# Run in background with auto-cleanup
cocoon run --rm -d ubuntu-22.04-cloudimg --name temp-vm
```

### 7.2 VM Lifecycle Management

```bash
# Create VM without starting (positional IMAGE parameter)
cocoon create ubuntu-22.04-cloudimg --name myvm

# Start VM
cocoon start myvm

# View logs
cocoon logs myvm --follow

# Stop VM gracefully
cocoon stop myvm

# Force kill (immediate SIGKILL, no graceful period)
cocoon kill myvm

# Delete VM
cocoon delete myvm

# Force delete running VM
cocoon delete myvm --force
```

### 7.3 Image Management

```bash
# Pull bootable OCI image
cocoon image pull myorg/ubuntu-bootable:22.04

# Verify bootability
cocoon image verify myorg/ubuntu-bootable:22.04

# List cached images
cocoon image list

# Inspect image details
cocoon image inspect myorg/ubuntu-bootable:22.04

# Remove image (fails if VMs using it)
cocoon image rm myorg/ubuntu-bootable:22.04
```

### 7.4 Monitoring and Cleanup

```bash
# List all VMs
cocoon list --all

# Filter by state
cocoon list --filter state=running

# Inspect VM details
cocoon inspect myvm

# Run garbage collection
cocoon gc

# Dry run GC
cocoon gc --dry-run
```

### 7.5 High-Concurrency VM Pool

```bash
# Create 100 VMs from same base image (uses COW)
for i in {1..100}; do
  cocoon run ubuntu-22.04-cloudimg \
    --name "vm-$(printf %03d $i)" \
    --cpus 2 \
    --memory 2G \
    --rm \
    -d \
    /bin/sh -c "echo Hello from VM $i"
done

# Storage usage: ~5GB base + 100×200KB overlays = ~5.02GB
# Without COW: 100×5GB = 500GB
```

---

## 8. Cross-References

### 8.1 Related Cocoon Documents

- [00-overview.md](./00-overview.md): Project motivation and architecture overview
- [01-boot-contract.md](./01-boot-contract.md): Boot modes, lifecycle semantics, VM configuration schema
- [02-installation.md](./02-installation.md): Cloud Hypervisor installation and prerequisites
- [05-storage-management.md](./05-storage-management.md): COW overlays, reference counting, garbage collection
- [06-concurrency.md](./06-concurrency.md): Thread-safety for reference counting and storage operations
- [07-vm-lifecycle.md](./07-vm-lifecycle.md): VM state machine, identifier rules (`vm_id`/`name`), config.json/metadata.json schemas
- [08-dependencies.md](./08-dependencies.md): Required packages and tools

### 8.2 Boot Contract Integration

This CLI design implements the Boot Contract specification:

| Boot Contract Section | CLI Implementation |
|----------------------|-------------------|
| §1 Boot Path Decision | `--boot-strategy` flag (default: pvh_then_uefi), config-level firmware paths, automatic UEFI fallback |
| §2 Guest Init Model | [Phase 2] Metadata server for cloud-init (not yet implemented) |
| §3 I/O Mechanisms | Serial console via `--serial file=...` (CH flag), `cocoon logs` command |
| §4 Lifecycle Semantics | `run`, `stop`, `delete`, `kill` commands |
| §5 VM Configuration Schema | `types.VMConfig` in Go code |
| §6 OCI to Bootable Bridge | `ImageManager.VerifyBootable()` and conversion logic |

### 8.3 Storage Management Integration

| Storage Document Section | CLI Implementation |
|-------------------------|-------------------|
| COW Strategy | `StorageManager.CreateOverlay()` in VM creation flow |
| Reference Counting | Automatic in `create` and `delete` flows |
| Garbage Collection | `cocoon gc` command |
| Storage Layout | Configured via `root_dir` / `runtime_dir` / `log_dir` in JSON config |

### 8.4 External References

- **Cloud Hypervisor API**: https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/vmm/src/api/openapi/cloud-hypervisor.yaml
- **cloud-init NoCloud**: https://cloudinit.readthedocs.io/en/latest/topics/datasources/nocloud.html
- **urfave/cli/v2**: https://cli.urfave.org/v2/
- **qemu-img**: https://www.qemu.org/docs/master/tools/qemu-img.html

### Reconcile Strategy

**Policy**: Regular CLI commands (`start`, `stop`, `delete`, `list`, `inspect`, etc.) do **not** perform automatic reconciliation. They operate on the recorded state in `metadata.json` and trust that state reflects reality.

**`cocoon doctor`** is the sole entry point for reconciliation:
- Scans all VM directories for stale state (e.g., metadata says RUNNING but CH process is dead)
- Cleans up orphaned sockets, PID files, and runtime directories
- Fixes metadata inconsistencies (transitions stale RUNNING → STOPPED)
- Rebuilds the name index if needed

**Rationale**: Auto-reconcile on every command adds latency and complexity. Users who suspect state drift run `cocoon doctor` explicitly. This is the same pattern used by containerd and other container runtimes.

---

## 9. Implementation Checklist

### Phase 1: Core Commands (P0)

- [ ] **CLI Framework**:
  - [ ] Setup urfave/cli/v2 application structure
  - [ ] Implement version command
  - [ ] Implement global flags (--config)

- [ ] **VM Lifecycle**:
  - [ ] `cocoon run` command with full flow
  - [ ] `cocoon create` command
  - [ ] `cocoon start` command with boot timeout
  - [ ] `cocoon stop` command with ACPI shutdown
  - [ ] `cocoon delete` command with resource cleanup
  - [ ] `cocoon kill` command for force termination

- [ ] **Information Commands**:
  - [ ] `cocoon list` with table/json output
  - [ ] `cocoon inspect` with detailed VM info
  - [ ] `cocoon logs` with follow/tail options

- [ ] **Image Management** (multi-source):
  - [ ] Image source auto-detection (qcow2 file / URL / OCI ref)
  - [ ] `cocoon image pull` from any source (qcow2, URL, OCI)
  - [ ] `cocoon image list` from cache
  - [ ] `cocoon image inspect` with metadata
  - [ ] `cocoon image verify` for boot contract
  - [ ] `cocoon image rm` with reference checking

- [ ] **Storage**:
  - [ ] Implement StorageManager interface
  - [ ] COW overlay creation
  - [ ] Reference counter integration
  - [ ] `cocoon gc` command

### Phase 2: Advanced Features (P1)

- [ ] **Boot Modes**:
  - [ ] Direct kernel boot support (`--boot-strategy direct_kernel`)
  - [ ] Kernel/initrd extraction from OCI images

- [ ] **I/O Enhancements**:
  - [ ] vsock support (future)
  - [ ] virtiofs support (future)
  - [ ] Structured output protocol parsing

- [ ] **Monitoring**:
  - [ ] VM metrics collection
  - [ ] Boot time tracking
  - [ ] Resource usage reporting

- [ ] **Testing**:
  - [ ] Unit tests for all interfaces
  - [ ] Integration tests with real Cloud Hypervisor
  - [ ] CLI command parsing tests
  - [ ] End-to-end workflow tests

---

## Appendix A: Complete VM Files Example

For the canonical schema definitions, see [07-vm-lifecycle.md § 5](./07-vm-lifecycle.md#5-vm-configuration-schema).

**config.json** (immutable, written once at create):
```json
{
  "vm_id": "vm-01HXYZ5A3B7C8D9E0F1G2H3J4K",
  "name": "myvm",
  "image_ref": "ubuntu-22.04-cloudimg",
  "base_key": "ef015678abcd1234_amd64",
  "base_digest_full": "ef015678abcd1234567890abcdef1234567890abcdef1234567890abcdef1234",
  "arch": "amd64",
  "boot_strategy": "pvh_then_uefi",
  "firmware_path": "/var/lib/cocoon/firmware/hypervisor-fw",
  "cpus": 2,
  "memory_mb": 1024,
  "disk_size": "10G",
  "base_image_path": "/var/lib/cocoon/cache/images/ef015678abcd1234_amd64.qcow2",
  "overlay_path": "/var/lib/cocoon/vms/vm-01HXYZ5A3B7C8D9E0F1G2H3J4K/overlay.qcow2",
  "serial_log": "/var/log/cocoon/vm-01HXYZ5A3B7C8D9E0F1G2H3J4K-serial.log",
  "socket_path": "/run/cocoon/vms/vm-01HXYZ5A3B7C8D9E0F1G2H3J4K/api.sock",
  "created_at": "2026-02-11T10:30:00Z",
  "schema_version": 1
}
```

**metadata.json** (mutable, updated on every state transition):
```json
{
  "vm_id": "vm-01HXYZ5A3B7C8D9E0F1G2H3J4K",
  "state": "RUNNING",
  "previous_state": "STARTING",
  "process_pid": 12345,
  "boot_time": "2.3s",
  "last_boot_mode": "pvh",
  "last_firmware_path": "/var/lib/cocoon/firmware/hypervisor-fw",
  "last_error": "",
  "error_count": 0,
  "auto_remove": false,
  "updated_at": "2026-02-11T10:30:07Z",
  "started_at": "2026-02-11T10:30:05Z",
  "stopped_at": "",
  "schema_version": 1
}
```

---

**End of CLI Design Document v1.0**

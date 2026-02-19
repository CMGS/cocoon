# CLI Design and Commands

**Version**: 1.0
**Status**: Implemented
**Phase**: Phase 1
**Last Updated**: 2026-02-18

## ⚠️ Supported Image Contract

**CRITICAL**: Cocoon requires **bootable VM images**, not regular container images.

**Supported Image Types**:
1. **Cloud Hypervisor Native Cloud Images** (Recommended):
   - Ubuntu Cloud, Fedora Cloud, Debian Cloud (qcow2 format)
   - Pre-configured for UEFI boot
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

This document defines the command-line interface for Cocoon, a lightweight VM management tool built on Cloud Hypervisor. The CLI follows Docker-like patterns for familiarity while exposing VM-specific capabilities like UEFI/Direct kernel boot modes, resource allocation, and lifecycle management.

The design integrates the [Boot Contract](./01-boot-contract.md) decisions, including UEFI boot for cloud images (Phase 1), serial console I/O, and graceful shutdown semantics. Direct kernel boot for OCI VM images is planned for Phase 2 (see [04.1-oci-vm-images.md](./04.1-oci-vm-images.md)). It also leverages the [storage management](./05-storage-management.md) system for efficient copy-on-write disk handling.

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
│   ├── init.go               # cocoon init (dirs + config; optional firmware download via --with-uefi-firmware)
│   ├── create.go             # cocoon create + shared vmCreateFlags()
│   ├── run.go                # cocoon run (create + start)
│   ├── start.go              # cocoon start
│   ├── stop.go               # cocoon stop
│   ├── kill.go               # cocoon kill
│   ├── rm.go                 # cocoon delete/rm
│   ├── ps.go                 # cocoon list/ps/ls
│   ├── inspect.go            # cocoon inspect
│   ├── logs.go               # cocoon logs
│   ├── console.go            # cocoon console (interactive PTY relay)
│   ├── console_linux.go      # Linux-specific SIGWINCH + ioctl
│   ├── console_darwin.go     # Darwin stub (SIGWINCH no-op)
│   ├── images.go             # cocoon image (list/pull/build/push/login/inspect/remove/verify)
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
│   ├── builder.go            # Builder interface (Build, Push, Login, Inspect, ListBuilds)
│   ├── buildtypes.go         # BuildResult, PushResult, BuildEntry, InspectResult
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
│   ├── refcache/
│   │   ├── index.go             # Manifest ref-cache: IMAGE_REF → base_key mapping
│   │   └── index_test.go
│   └── mocks/
│       └── mock.go
├── oci/
│   ├── types.go              # Media types, VMImageConfig, BuildResult, PushResult, TagEntry
│   ├── cocoonfile.go         # Cocoonfile parser (FROM/RUN/COPY/LABEL)
│   ├── kernel.go             # Kernel detection from /boot file listing
│   ├── tar.go                # Deterministic tar layer builder
│   ├── build_linux.go        # Build pipeline: extract kernel/rootfs, assemble OCI layout
│   ├── build_darwin.go       # Stub: returns "Linux only" error
│   ├── push.go               # Push OCI layout to registry via go-containerregistry
│   ├── login.go              # Registry login, credential storage (~/.cocoon/config.json)
│   ├── layout.go             # InspectLayout: parse OCI layout metadata
│   ├── store.go              # Tag index: local build tag → OCI layout mapping
│   ├── store_linux.go        # Linux-specific store operations (manifest ref cleanup)
│   ├── blobstore.go          # Shared content-addressed blob store (cache/oci/blobs/)
│   ├── layerrefs.go          # Blob-to-manifest reference tracking for GC
│   ├── build_context.go      # Build context sidecar for FROM resolution and layer reuse
│   ├── delta_linux.go        # Delta layer generation for Cocoonfile RUN/COPY
│   ├── progress.go           # Docker-like step progress output to stderr
│   └── builder.go            # image.Builder adapter (delegates to package functions)
├── storage/
│   ├── storage.go            # ReferenceCounter, COWManager, GarbageCollector interfaces
│   ├── types.go              # OverlayInfo, GCResult
│   ├── local/
│   │   ├── refcount.go       # ReferenceCounter impl (references.json + flock)
│   │   ├── cow.go            # COWManager impl (qemu-img create/resize)
│   │   ├── gc.go             # GarbageCollector impl (permanent delete)
│   │   └── gc_oci.go         # OCI GC: layouts, tags, manifest refs, blobs
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
│   │   ├── reconcile.go      # VM state reconciliation logic
│   │   └── classify.go       # Error classification (reason → ErrorType)
│   └── mocks/
│       └── mock.go
├── types/
│   ├── boot.go               # BootStrategy, BootConfig, DefaultBootStrategy
│   ├── config.go             # VMConfig (immutable, written at create)
│   ├── errors.go             # ErrorType (15 constants), ClassifiedError
│   ├── inspect.go            # VMInspect (merged config + metadata view)
│   ├── metadata.go           # VMMetadataFile (mutable runtime state)
│   ├── reference.go          # NameIndex, Reference types
│   ├── state.go              # VMState (8 states), ValidateTransition
│   └── state_test.go
├── lock/
│   ├── lock.go               # Locker interface (Lock, TryLock, Unlock, Path)
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
    GetConsolePTYPath(ctx context.Context, socketPath string) (string, error)

    // --- Utilities ---
    WaitForSocket(ctx context.Context, socketPath string, timeout time.Duration) error
    CheckSocketConnectivity(socketPath string) error
}
```

The concrete implementation lives in `hypervisor/cloudhypervisor/client.go`. Key implementation details:
- `Launch()` starts a CH process with `--api-socket` only; all VM configuration including firmware is sent via the `PUT /api/v1/vm.create` REST call.
- `buildCHVMConfig()` sets `payload.firmware` for UEFI (CLOUDHV.fd) or `payload.kernel` + `payload.initramfs` + `payload.cmdline` for Direct kernel boot.
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
// All deletions are permanent. See docs/18-garbage-collection.md.
// Locking order: gc.lock (Level 1) → references.lock (Level 2).
type GarbageCollector interface {
    CollectUnreferencedImages() ([]string, error)
    CollectOrphanedOverlays() ([]string, error)
    CollectOrphanedOCILayouts() ([]string, error)
    CollectStaleOCITags() ([]string, error)
    CollectOrphanedOCIManifestRefs() ([]string, error)
    CollectUnreferencedOCIBlobs() ([]string, error)
    CollectTempFiles(maxAge time.Duration) ([]string, error)
    FullGC() error
}
```

Concrete implementations live in `storage/local/` (refcount.go, cow.go, gc.go, gc_oci.go).

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
    Kill(ctx context.Context, vmID string) error
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
    UpdateMetadata(vmID string, mutate func(*types.VMMetadataFile)) error

    // Reconciliation (used by cocoon doctor).
    Reconcile(ctx context.Context, fix bool, force bool) ([]Inconsistency, error)
}
```

The concrete implementation is `vm/engine/manager.go`. `Kill()` handles state transitions, metadata cleanup (ProcessPID=0, StoppedAt), and the state machine. `UpdateMetadata()` acquires the per-VM metadata lock, loads the current metadata, applies the caller's mutation function, and saves it atomically. `Start()` boots using the strategy stored in config.json (`uefi` by default for cloud images, or `direct` for OCI VM images).

### 2.5 Manager Initialization

Cocoon does not use a factory pattern. All managers are constructed directly in `cmd/cocoon/app.go`:

```go
// appContext holds initialized managers for CLI commands.
type appContext struct {
    cfg      *config.CocoonConfig
    vmMgr    vm.Manager
    imgMgr   image.Manager  // Cloud image pipeline (pull/convert/cache).
    imgBuild image.Builder  // OCI VM image build/push/login.
    hyper    hypervisor.Client
    refCtr   storage.ReferenceCounter
    cowMgr   storage.COWManager
    gc       storage.GarbageCollector
}

func initApp(_ *cli.Context) (*appContext, error) {
    // Require root on Linux.
    if runtime.GOOS == "linux" && os.Geteuid() != 0 {
        return nil, fmt.Errorf("cocoon requires root privileges")
    }

    cfg, err := config.LoadConfig(configPath)
    // ... apply --root-dir, --runtime-dir, --log-dir overrides ...

    hyper := cloudhypervisor.New(cfg)
    refCtr := local.NewReferenceCounter(cfg)
    cowMgr := local.NewCOWManager(cfg)
    gc := local.NewGarbageCollector(cfg)
    imgMgr := pipeline.New(cfg, refCtr)
    imgBuild := oci.NewBuilder(cfg)
    vmMgr := engine.New(cfg, hyper, refCtr, cowMgr, imgMgr)

    return &appContext{cfg, vmMgr, imgMgr, imgBuild, hyper, refCtr, cowMgr, gc}, nil
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

| Identifier | Format                        | Example                         | Mutable?               | Used In                           |
| ---------- | ----------------------------- | ------------------------------- | ---------------------- | --------------------------------- |
| `vm_id`    | `vm-{ulid}`                   | `vm-01HXYZ5A3B7C8D9E0F1G2H3J4K` | Never                  | Directories, logs, sockets, locks |
| `name`     | User-chosen or auto-generated | `myvm`, `cocoon-a3f7b2c1`       | Immutable after create | CLI commands, display, name index |

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
        consoleCommand(),
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
                Usage: "overwrite existing config file; with --with-uefi-firmware, also re-download firmware",
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
2. Create all directories via `cfg.EnsureDirs()` (db/, cache/images/, cache/manifests/, cache/locks/, cache/oci/blobs/sha256/, cache/oci/layouts/, vms/, temp/, firmware/, buildah/, runtime/vms/, log/)
3. If `--with-uefi-firmware URL`: download to `firmware/CLOUDHV.fd` (0644), atomic rename
4. Write `config.json` to `--config` path (default `/etc/cocoon/config.json`); skip if exists unless `--force`
5. Print "Done. Run 'cocoon doctor' to verify system dependencies."

**Example Usage**:

```bash
# Basic initialization
sudo cocoon init

# Initialize with UEFI firmware download
sudo cocoon init --with-uefi-firmware https://github.com/cloud-hypervisor/edk2/releases/download/ch-a54f262b09/CLOUDHV.fd

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
        &cli.BoolFlag{
            Name:   "oci",
            Hidden: true,
            Usage:  "use OCI VM image (direct kernel boot)",
        },
        &cli.BoolFlag{
            Name:  "skip-verify",
            Usage: "skip bootability verification of the image",
        },
        &cli.BoolFlag{
            Name:  "tpm",
            Usage: "enable TPM 2.0 emulation via swtpm",
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
        &cli.IntFlag{
            Name:  "boot-timeout",
            Usage: "boot detection timeout in seconds (0 = skip boot detection)",
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

1. **Create VM** (`vmMgr.Create`): Generate VM ID, resolve image, prepare COW overlay, pin reference, register name, transition to CREATED.
2. **Print VM ID**: Output the VM ID immediately after Create, **before** boot detection. This allows scripts to capture the ID for cleanup even if Start fails (see `cmd/cocoon/run.go` rationale comment).
3. **Start VM** (`vmMgr.Start`): Launch CH process, configure via REST, boot VM, poll serial log for boot completion (timeout: config default), transition to RUNNING.
4. **Background behavior**: VM runs as a background CH process. Serial log is written to disk; use `cocoon logs --follow` to stream.
   > **Phase 1 note**: All runs are background (CH process). The `--detach/-d` flag is accepted but is a no-op. In Phase 2, non-detach runs will attach to the serial log automatically; `--detach/-d` will then control attach vs. detach mode.
5. **Auto-remove** (if `--rm`): The `AutoRemove` flag is recorded in metadata after Start succeeds. When the VM is stopped via `cocoon stop`, the delete flow is triggered automatically. Note: if the VM crashes or is killed externally, auto-remove does not fire. Use `cocoon doctor --fix` for state reconciliation; automatic deletion of crashed `auto_remove` VMs is a future enhancement.

**Example Usage**:

```bash
# Run Ubuntu VM with 4 CPUs and 4GB memory
cocoon run ubuntu-22.04-cloudimg --name myvm --cpus 4 --memory 4G

# Run VM with auto-remove on stop
cocoon run --rm ubuntu-22.04-cloudimg --name temp-vm

# Run an OCI VM image (Phase 2 runtime path -- not yet implemented)
# cocoon run myorg/ubuntu-vm:22.04

# Run with TPM 2.0 emulation enabled
cocoon run --tpm ubuntu-22.04-cloudimg --name secure-vm

# Run with custom boot timeout (seconds)
cocoon run --boot-timeout 120 ubuntu-22.04-cloudimg --name slow-vm
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
  - Enhanced bootability diagnostics with specific remediation guidance

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

The `createCommand` uses the same `vmCreateFlags()` as `runCommand`, which includes `--name`, `--cpus`, `--memory` (default "2048M"), `--disk`, `--skip-verify`, and `--tpm`. Runtime image-type selection is resolver-driven (user does not need a mode flag). The hidden `--oci` flag remains an internal debug override while Phase 2 runtime wiring is unfinished. Note: `--boot-timeout` is only available on `run` and `start` commands (not `create`, since `create` does not boot the VM).

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
        Usage:     "Remove one or more VMs and cleanup storage",
        ArgsUsage: "VM_REF [VM_REF...]",
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
2. **Stop VM** (if `--force`): Call stop with configured `stop_timeout_seconds` (default 30s)
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

**Purpose**: List VMs (default: running only)

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

# List all VMs (including stopped/error)
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
vm-01H9ZZ8Y7X6W5V4U3T2S1R0Q9P     cocoon-a3f7b2c1   RUNNING   2     2048MB  2026-02-09T08:15:00Z
```

Note: By default, `cocoon list` shows only `RUNNING` VMs. Use `--all` to include `CREATED`, `STOPPED`, and `ERROR`. The `NAME` column shows the user-provided name or the auto-generated name (`cocoon-{random}` if `--name` was omitted at create time). Either the `VM ID` or `NAME` can be used as a `<vm-ref>` in subsequent commands. The `MEMORY` column displays the value with an `MB` suffix (e.g., `2048MB`). The `CREATED` column shows the creation timestamp in RFC 3339 format.

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
When the serial log is readable, `hypervisor.serial_log_excerpt` contains the latest 100 lines.

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
    "serial_log": "/var/log/cocoon/vm-01HXYZ5A3B7C8D9E0F1G2H3J4K-serial.log",
    "console_pty": "/dev/pts/7",
    "serial_log_excerpt": [
      "[    0.000000] Linux version 6.8.0...",
      "[    2.134221] systemd[1]: Reached target Multi-User System.",
      "Ubuntu 22.04.5 LTS ready"
    ]
  },
  "boot_config": {
    "cpus": 2,
    "memory_mb": 2048,
    "boot_strategy": "uefi",
    "firmware_path": "/var/lib/cocoon/firmware/CLOUDHV.fd"
  },
  "timestamps": {
    "created_at": "2026-02-11T10:30:00Z",
    "updated_at": "2026-02-11T10:30:07Z",
    "started_at": "2026-02-11T10:30:05Z"
  },
  "runtime": {
    "boot_time": "2.3s",
    "last_boot_mode": "uefi",
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

### 4.12 cocoon console (Interactive Console)

**Command**: `cocoon console <vm-ref> [FLAGS]`

**Purpose**: Attach an interactive console to a running VM. Provides bidirectional TTY access via the Cloud Hypervisor virtio-console PTY device. See [12-console.md](./12-console.md) for design details.

```go
func consoleCommand() *cli.Command {
    return &cli.Command{
        Name:      "console",
        Usage:     "Attach an interactive console to a running VM",
        ArgsUsage: "VM_REF",
        Flags: []cli.Flag{
            &cli.StringFlag{
                Name:  "escape-char",
                Value: "^]",
                Usage: "escape character for disconnect (single character or ^X caret notation; default ^] matches telnet)",
            },
        },
        Action: consoleAction,
    }
}
```

**Example Usage**:

```bash
# Attach console to a running VM
cocoon console myvm

# Use a custom escape character (e.g., tilde)
cocoon console myvm --escape-char "~"
```

**Escape Sequences** (at start of line, default escape char `^]` = Ctrl-]):

| Sequence | Action                          |
| -------- | ------------------------------- |
| `^].`    | Disconnect from console         |
| `^]?`    | Show supported escape sequences |
| `^]^]`   | Send literal Ctrl-] to guest    |

**Notes**:
- Requires Linux (Cloud Hypervisor is Linux-only)
- Requires an interactive terminal (stdin must be a TTY)
- The VM must be in RUNNING state
- Terminal is set to raw mode during the session and restored on exit
- Disconnecting does not stop the VM

### 4.13 cocoon image (Image Management)

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
                Flags: []cli.Flag{
                    // Current behavior: image type is inferred from IMAGE_REF.
                    // Future direct-kernel OCI pull flow (Phase 2) may add
                    // explicit mode selection.
                    &cli.BoolFlag{
                        Name:  "skip-verify",
                        Usage: "skip bootability verification after pull",
                    },
                },
                Action:    imagePullAction,
            },
            {
                Name:      "build",
                Usage:     "Build an OCI VM image from a cloud image or Cocoonfile",
                ArgsUsage: "[CLOUD_IMAGE]",
                Flags: []cli.Flag{
                    &cli.StringFlag{
                        Name:    "tag",
                        Aliases: []string{"t"},
                        Usage:   "OCI reference tag for the built image",
                    },
                    &cli.StringFlag{
                        Name:    "file",
                        Aliases: []string{"f"},
                        Usage:   "path to Cocoonfile",
                    },
                },
                Action: imageBuildAction,
            },
            {
                Name:      "push",
                Usage:     "Push a locally built OCI VM image to a container registry",
                ArgsUsage: "REF",
                Action:    imagePushAction,
            },
            {
                Name:      "login",
                Usage:     "Log in to a container registry",
                ArgsUsage: "REGISTRY",
                Flags: []cli.Flag{
                    &cli.StringFlag{
                        Name:    "username",
                        Aliases: []string{"u"},
                        Usage:   "registry username",
                    },
                    &cli.StringFlag{
                        Name:    "password",
                        Aliases: []string{"p"},
                        Usage:   "registry password",
                    },
                },
                Action: imageLoginAction,
            },
            {
                Name:      "inspect",
                Usage:     "Show details of a cached cloud image or locally built OCI VM image",
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
                Name:      "tag",
                Usage:     "Create or update a local image tag/alias",
                ArgsUsage: "SOURCE_REF TARGET_REF",
                Action:    imageTagAction,
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

| Pattern                                | Detected Type | Action                                                          |
| -------------------------------------- | ------------- | --------------------------------------------------------------- |
| `/path/to/*.qcow2` or `/path/to/*.img` | `qcow2`       | Validate file, copy/link to cache                               |
| `https://...` or `http://...`          | `url`         | Download, validate, cache                                       |
| `registry/repo:tag` or `repo:tag`      | `oci`         | Pull via Buildah, convert to qcow2, validate bootability, cache |

**Cache Resolution Behavior**:

- `image pull` updates the local manifest cache (`cache/manifests/index.json`) to map `IMAGE_REF` aliases to `base_key`.
- `image inspect`, `image remove`, and `image verify` resolve local OCI tags with an implicit `:latest` fallback when no tag/digest is provided.
- `image inspect` and `image remove` resolve cloud-image `IMAGE_REF` from local cache only (`base_key` direct hit or manifest-cache alias), without pulling.
- `image tag SOURCE_REF TARGET_REF` behavior:
  1. If `SOURCE_REF` is a local OCI build tag (implicit `:latest` fallback supported), creates/updates another local OCI tag pointing to the same layout.
  2. Otherwise, resolves a cached cloud image (`base_key`/manifest alias) and creates/updates a cloud-image alias in `cache/manifests/index.json`.
- `image push <ref>` for local OCI images applies implicit `:latest` when no tag/digest is present.
- `image build` default tag behavior (when `--tag` is omitted):
  1. Derive the name from the effective build source (`CLOUD_IMAGE` or Cocoonfile `FROM`).
  2. Strip local file extension when applicable.
  3. Append `:latest` if no explicit tag suffix is present.
- Cocoonfile `FROM` resolution behavior:
  1. Local file path/relative existing file next to Cocoonfile.
  2. Local OCI tag (implicit `:latest` fallback).
  3. `http(s)` cloud-image URL via image pipeline prepare.
  4. Docker-like OCI ref normalization and prepare fallback (`ubuntu` -> `docker.io/library/ubuntu:latest`).
- `image remove <ref>` behavior for OCI tags:
  1. Removes only the requested tag entry from `db/oci-build-tags.json`.
  2. Keeps the layout directory when other tags still reference the same `layout_path`.
  3. Removes the layout directory only after the last tag referencing that layout is deleted.
  4. Blob reference cleanup still keys off `manifest_digest` and only collects zero-ref blobs.
- `image verify` resolution order:
  1. local OCI build tag (`cache/oci/layouts/...`) -> checks OCI layer media types and reports `direct` mode when kernel layer exists
  2. local file path
  3. cached image via `base_key`/manifest alias
  4. fallback `Prepare` only when not found locally
  5. if local cache resolution fails due ambiguity/corruption, return error (do not auto-pull)
- `image list` shows a **unified table** combining both cloud images and OCI builds. The table columns are:

  | Column | Description |
  |--------|-------------|
  | TYPE | `cloudimg` for cloud images, `oci` for OCI VM builds |
  | REF | `base_key` for cloud images, tag name for OCI |
  | DIGEST | Truncated `sha256:` manifest digest (first 12 hex chars) for OCI, `-` for cloud images |
  | SIZE | Human-readable total size |
  | SOURCE | Source reference string for cloud images, `local` for OCI builds |
  | CREATED | ISO 8601 timestamp |

  **Example output**:

  ```
  TYPE      REF                              DIGEST              SIZE      SOURCE     CREATED
  cloudimg  a1b2c3d4e5f6a7b8_amd64          -                   2.1GB     https://   2026-02-15T10:00:00Z
  oci       myregistry.io/ubuntu-vm:22.04   sha256:ef0123456789  1.8GB     local      2026-02-16T14:30:00Z
  ```

  JSON output includes `source_refs`.

**Example Usage**:

```bash
# Pull cloud image from URL (recommended)
cocoon image pull https://cloud-images.ubuntu.com/releases/22.04/release/ubuntu-22.04-server-cloudimg-amd64.img

# Import local qcow2 file into cache
cocoon image pull /tmp/ubuntu-22.04-cloudimg.qcow2

# Pull bootable OCI image (custom-built, requires root for conversion)
cocoon image pull myorg/ubuntu-bootable:22.04

# Build an OCI VM image from a cloud image
cocoon image build /path/to/ubuntu-22.04-cloudimg.qcow2 --tag myorg/ubuntu-vm:22.04

# Build using a Cocoonfile (see cocoonfile.example in project root)
cocoon image build --file cocoonfile.example --tag myorg/ubuntu-vm:22.04

# Build output includes progress to stderr:
#   Step 1/8 : Detecting kernel...
#    ---> 5.15.0-100-generic
#   Step 2/8 : Extracting kernel and initrd...
#   ...
#   Step 8/8 : Saving tag...
#    ---> myorg/ubuntu-vm:22.04

# Log in to a container registry
cocoon image login ghcr.io -u myuser

# Push a built image to a registry
cocoon image push myorg/ubuntu-vm:22.04

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

  2. (Phase 2) Build a bootable OCI image with:
     cocoon image build --file Cocoonfile --tag myorg/ubuntu-bootable:22.04

  3. See docs/00-overview.md#supported-image-contract for details

Exit code: 1
```

### 4.14 cocoon gc (Garbage Collection)

**Command**: `cocoon gc [FLAGS]`

**Purpose**: Run garbage collection to cleanup unused resources

**Implementation**: Based on [05-storage-management.md](./05-storage-management.md)

```go
func gcCommand() *cli.Command {
    return &cli.Command{
        Name:  "gc",
        Usage: "Run garbage collection on unreferenced images and orphaned resources",
        Flags: []cli.Flag{
            &cli.BoolFlag{
                Name:  "dry-run",
                Usage: "only report what would be collected, don't actually delete",
            },
        },
        Action: gcAction,
    }
}
```

**GC Phases**: The garbage collector runs 8 phases in order. All deletions are permanent (no trash). See [18-garbage-collection.md](./18-garbage-collection.md) for the full design.

1. **Unreferenced base images**: Permanently delete cloud image qcow2 files with zero VM references.
2. **Orphaned overlays**: Permanently delete VM directories where overlay.qcow2 exists but config.json is missing.
3. **Orphaned OCI layouts**: Remove layout directories in `cache/oci/layouts/` not referenced by any tag.
4. **Stale OCI tags**: Remove tags from `oci-build-tags.json` whose layout path no longer exists; cascade cleanup to orphaned manifests/blobs.
5. **Orphaned OCI manifest refs**: Remove manifest digests from `oci-layer-refs.json` not associated with any live tag; delete zero-ref blobs.
6. **Unreferenced OCI blobs**: Remove blobs from `cache/oci/blobs/sha256/` with zero manifest references.
7. **Stale conversion locks**: Remove stale `cache/locks/*.lock` files when corresponding base image is missing and lock is not held.
8. **Temp entries**: Remove files/directories in `temp/` older than 1 hour.

**Example Usage**:

```bash
# Run GC (permanently deletes all unreferenced resources)
cocoon gc

# Dry run to see what would be collected
cocoon gc --dry-run
```

**Example Output**:

```
collected image: a1b2c3d4e5f6a7b8_amd64
collected orphaned OCI layout: 7f3a1b2c
collected stale OCI tag: myregistry.io/old-image:v1
collected unreferenced OCI blob: abc123def456...

Collected 4 item(s): 1 images, 0 overlays, 1 OCI layouts, 1 stale tags, 0 orphaned manifests, 1 OCI blobs, 0 stale locks, 0 temp files.
```

**Dry-run output** reports candidates for each phase without deleting:

```
Dry run

Unreferenced images (candidates for collection):
  a1b2c3d4e5f6a7b8_amd64

No orphaned overlays found.

Orphaned OCI layouts (candidates for collection):
  7f3a1b2c

Stale OCI tags (candidates for collection):
  myregistry.io/old-image:v1

No orphaned OCI manifest refs found.

Unreferenced OCI blobs (candidates for collection):
  abc123def456...

No temp files found for collection.

Use 'cocoon gc' without --dry-run to perform collection.
```

### 4.15 cocoon doctor (System Health Check)

**Command**: `cocoon doctor [FLAGS]`

**Purpose**: Validate Cocoon installation and dependencies

**Implementation**: Based on [08-dependencies.md § Startup Dependency Detection](./08-dependencies.md#startup-dependency-detection-cocoon-doctor) and [Boot Contract § 1.1](./01-boot-contract.md#11-default-boot-mode-uefi--cloudhvfd)

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
                Usage: "kill zombie processes and force-fix stuck states (requires --fix)",
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

    if force && !fix {
        return fmt.Errorf("--force requires --fix; run 'cocoon doctor --fix --force'")
    }

    // Phase 1: Dependency checks (binary/path/version, firmware, directories).
    checks := runDependencyChecks(app)

    // Phase 2: VM reconciliation (state consistency, orphan cleanup).
    issues, reconcileErr := app.vmMgr.Reconcile(c.Context, fix, force)

    // Print results in table or JSON format.
    // --fix attempts VM state repairs (not dependency installation).
}
```

**Dependency Checks** (informational, --fix does not install missing tools):
- cloud-hypervisor binary (minimum `38.0.0`)
- ch-remote binary (minimum `38.0.0`)
- UEFI firmware file (CLOUDHV.fd)
- qemu-img binary (minimum `8.0.0`)
- buildah binary (minimum `1.35.0`)
- skopeo binary (minimum `1.14.0`)
- guestfish binary (minimum `1.50.0`)
- swtpm binary (TPM 2.0 emulator)
- virt-customize binary (optional; required for Cocoonfile RUN/COPY steps)
- /dev/kvm device
- Directory structure (root, runtime, log, db, vm, cache, buildah, firmware)

**VM Reconciliation** (--fix repairs these):
- Stale RUNNING state (CH process not found → mark ERROR)
- Zombie socket / stale PID file → remove
- Orphaned overlay (overlay exists, config missing) → permanently delete VM dir
- Missing reference (config exists, references missing vmID) → restore `references.json` entry
- Dangling reference (references vmID missing/mismatched config) → remove stale vmID from `references.json`
- Name index inconsistencies → detect in dry-run, rebuild only with `--fix`

**Exit Codes**:
- `0`: All checks passed (returns nil)
- `1`: One or more checks failed or error occurred (returns error)

**Example Usage**:

```bash
# Quick health check
cocoon doctor

# Fix VM state issues (reconcile stale states, rebuild name index)
cocoon doctor --fix

# Force repair mode (must be used with --fix)
cocoon doctor --fix --force
```

**Example Output**:

```
$ cocoon doctor
=== Dependency Checks ===
CHECK              STATUS  DETAIL
cloud-hypervisor   pass    /usr/bin/cloud-hypervisor
uefi-firmware      fail    not found at /var/lib/cocoon/firmware/CLOUDHV.fd
qemu-img           pass    /usr/bin/qemu-img (version 8.2.0)
ch-remote          pass    /usr/bin/ch-remote
buildah            pass    /usr/bin/buildah (version 1.35.2)
skopeo             pass    /usr/bin/skopeo (version 1.14.4)
guestfish          fail    binary not found in PATH (required for OCI-to-qcow2 conversion)
swtpm              pass    /usr/bin/swtpm (TPM 2.0 emulator)
virt-customize     warn    not found in PATH (optional: required for Cocoonfile RUN/COPY steps)
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
uefi-firmware      pass    /var/lib/cocoon/firmware/CLOUDHV.fd
...

All dependency checks passed.

=== VM Reconciliation ===
VM ID             TYPE            SEVERITY  DETAILS
vm-01HXYZ5A3B...  stale_running   warning   CH process not found, marked ERROR
vm-01HABC9D8E...  name_index      info      rebuilt name index entry

Attempted to fix 2 issue(s).
```

---

### 4.16 cocoon firmware (Firmware Management)

**Command**: `cocoon firmware <subcommand> [FLAGS]`

**Purpose**: Manage hypervisor firmware files (UEFI)

**Implementation**: Based on [Boot Contract § 1.1](./01-boot-contract.md#11-default-boot-mode-uefi--cloudhvfd)

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

#### 4.16.1 cocoon firmware list

List all installed firmware files with paths and sizes.

```
$ cocoon firmware list
NAME             TYPE  PATH                                       SIZE     EXISTS
CLOUDHV.fd       UEFI  /var/lib/cocoon/firmware/CLOUDHV.fd        2.1MB    true
```

#### 4.16.2 cocoon firmware install

Download and install firmware files. Uses the latest edk2 release by default; override with `--uefi-url`.

```bash
# Install UEFI firmware from URL
cocoon firmware install --uefi-url https://github.com/cloud-hypervisor/edk2/releases/latest/download/CLOUDHV.fd

# Force re-download
cocoon firmware install --uefi-url URL --force
```

**Installation Process**:
1. HTTP GET the firmware from the provided URL to a temporary file
2. Atomic rename the temporary file to the target path (`/var/lib/cocoon/firmware/`)
3. No checksum verification, no backup of existing files

**Example Output**:

```bash
$ cocoon firmware install --uefi-url https://github.com/cloud-hypervisor/edk2/releases/download/ch-a54f262b09/CLOUDHV.fd
Firmware install complete.
```

#### 4.16.3 cocoon firmware verify

Check that firmware files exist and are accessible (via `os.Stat`). No checksum verification is performed.

```bash
# Verify all firmware files
cocoon firmware verify
```

**Example Output**:

```bash
$ cocoon firmware verify
OK    CLOUDHV.fd (UEFI): /var/lib/cocoon/firmware/CLOUDHV.fd [2.1MB]

All firmware files verified.
```

**Firmware Types**:
- `uefi`: CLOUDHV.fd (UEFI boot, required for cloud images; deprecated fallback: system OVMF)

**Firmware Storage**:
```
/var/lib/cocoon/firmware/
└── CLOUDHV.fd              # UEFI firmware (Cloud Hypervisor edk2)
```

**Exit Codes**:
- `0`: Success
- `1`: Command failed (download error, firmware not found, etc.)

**Example Usage**:

```bash
# List installed firmware
cocoon firmware list

# Install UEFI firmware from URL
cocoon firmware install --uefi-url https://github.com/cloud-hypervisor/edk2/releases/latest/download/CLOUDHV.fd

# Verify firmware files exist
cocoon firmware verify

# Force re-download
cocoon firmware install --uefi-url https://github.com/cloud-hypervisor/edk2/releases/latest/download/CLOUDHV.fd --force
```

#### 4.16.4 cocoon firmware update

Alias for `cocoon firmware install`. Accepts the same flags (`--uefi-url`, `--force`) and behaves identically. Provided as a convenience for users who prefer the "update" verb.

```bash
# Update firmware (same as install)
cocoon firmware update --uefi-url https://github.com/cloud-hypervisor/edk2/releases/latest/download/CLOUDHV.fd
```

### 4.17 cocoon version (Version Information)

**Purpose**: Display Cocoon version, git revision, and build timestamp. The version fields are injected at build time via `go build -ldflags`.

**Implementation**: `cmd/cocoon/version.go`

```go
func versionCommand() *cli.Command {
    return &cli.Command{
        Name:  "version",
        Usage: "Show version information",
        Action: func(_ *cli.Context) error {
            fmt.Print(version.String())
            return nil
        },
    }
}
```

**Output format** (from `version/version.go`):

```
Version:    v0.1.0
Revision:   abc1234def5678...
Built at:   2026-02-17T10:30:00
```

The three fields (`VERSION`, `REVISION`, `BUILTAT`) default to `"unknown"`, `"HEAD"`, and `"now"` when not set via ldflags. The `Makefile` sets them automatically during `make build`:

```bash
go build -ldflags "-X github.com/CMGS/cocoon/version.REVISION=$(git rev-parse HEAD) \
  -X github.com/CMGS/cocoon/version.VERSION=$(git describe --tags) \
  -X github.com/CMGS/cocoon/version.BUILTAT=$(date +%Y-%m-%dT%H:%M:%S)"
```

Note: `cocoon --version` (the global flag provided by urfave/cli) uses the same `version.String()` output via the custom `cli.VersionPrinter` set in `main()`.

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
    UEFIFirmwarePath string `json:"uefi_firmware_path"`  // UEFI firmware path
    BuildahRoot      string `json:"buildah_root"`        // Buildah storage root

    DefaultCPUs     int    `json:"default_cpus"`      // Default vCPUs per VM
    DefaultMemoryMB int64  `json:"default_memory_mb"` // Default memory in MB
    DefaultDiskSize string `json:"default_disk_size"`  // Default overlay disk size

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
  "uefi_firmware_path": "/var/lib/cocoon/firmware/CLOUDHV.fd",
  "buildah_root": "/var/lib/cocoon/buildah",
  "default_cpus": 2,
  "default_memory_mb": 2048,
  "default_disk_size": "10G",
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
4. **Prepare image** → `ImageManager.Prepare(image)` detects source type, pulls/converts/caches to local qcow2 in one call. For OCI images, pull+convert are both inside the per-image conversion lock (Level 3). For URL/local images, pull runs unlocked (baseKey unknown until download), then convert acquires the lock. See `06-concurrency.md § Usage Pattern` for details. Returns `ImageIdentity` + cached base image path.
5. **Verify bootability** → `ImageManager.VerifyBootability(baseImagePath)` (Boot Contract §6)
   - Inspects partitions via guestfish (kernel, initrd, UEFI bootloader, systemd)
   - Skipped if `--skip-verify` is set
6. **Configure VM** → Write `config.json` and initial `metadata.json` (CREATING state) to the VM directory:
   ```go
   vmCfg := &types.VMConfig{
       VMID:           vmID,
       Name:           name,
       ImageRef:       imageRef,
       BaseKey:        identity.BaseKey(),
       BaseDigestFull: identity.FullDigest,
       Arch:           identity.Arch,
       BootStrategy:   bootStrategy,
       FirmwarePath:   firmwarePath,
       CPUs:           cpus,
       MemoryMB:       memoryMB,
       DiskSize:       diskSize,
       BaseImagePath:  baseImagePath,
       OverlayPath:    overlayPath,
       SocketPath:     socketPath,
       SerialLog:      serialLogPath,
       CreatedAt:      time.Now().UTC().Format(time.RFC3339),
       SchemaVersion:  types.CurrentConfigSchemaVersion,
   }
   ```
7. **Pin base image reference** (short lock hold):
   - **Acquire references.lock** (Level 2)
   - `refCounter.AddReference(baseKey, vmID, digestFull, sourceRef)` — immediately adds `vmID` to `refs[]`, protecting the base image from GC
   - **Release references.lock**
   - This "pin" ensures the base image survives even if GC runs during the subsequent (slow) steps. On failure in later steps, the cleanup path removes this reference.
   - Metadata must exist before pin so reconciliation can find the VM if we crash after pinning.
8. **Create COW overlay** → `COWManager.CreateOverlay(baseKey, vmID, diskSize)`
9. **Register name** → `AddName(cfg, name, vmID)` acquires `name-index.lock` (Level 2), adds `name → vm_id` to `name-index.json`, release lock. Fails with `ErrVMAlreadyExists` if name is taken.
10. **Transition CREATING → CREATED** → Atomically updates `metadata.json` state. At this point the VM is fully persisted and discoverable via `cocoon inspect`/`cocoon list`, but not yet running.
11. **Print VM ID** → Output `vm_id` to stdout immediately after Create, **before** boot detection. This allows scripts to capture the ID for cleanup even if Start fails (see `cmd/cocoon/run.go` rationale).

    **--- End of `Create()` phase; `Start()` phase begins ---**

12. **Start Cloud Hypervisor** (REST-first):
    ```bash
    # Launch CH process with API socket only (no firmware/config on CLI):
    cloud-hypervisor --api-socket /run/cocoon/vms/{vm_id}/api.sock
    ```
    Then configure the VM via CH REST API:
    ```
    PUT /api/v1/vm.create
    {
      "payload": {"firmware": "/var/lib/cocoon/firmware/CLOUDHV.fd"},
      "cpus": {"boot_vcpus": 2},
      "memory": {"size": 2147483648},
      "disks": [{"path": "/var/lib/cocoon/vms/{vm_id}/overlay.qcow2"}],
      "serial": {"mode": "File", "file": "/var/log/cocoon/{vm_id}-serial.log"},
      "console": {"mode": "Pty"}
    }
    ```
    Followed by `PUT /api/v1/vm.boot` to start the VM.
13. **Wait for boot** → Poll serial log for boot completion marker (timeout: 60s), transition to RUNNING, write runtime fields (PID, socket path) to `metadata.json`. Note: `config.json` is immutable after step 6 and is never rewritten.
14. **Auto-remove bookkeeping** → If `--rm`, set `AutoRemove=true` in metadata after Start succeeds; delete is triggered when the VM is later stopped via `cocoon stop` (crash/external-kill: `cocoon doctor --fix` performs state reconciliation; automatic deletion of crashed `auto_remove` VMs is a future enhancement)

**Failure cleanup**: If any step after 6 (in Create) fails, the cleanup path must:
- `RemoveName(cfg, name)` — release the name-index entry
- **Acquire references.lock** (Level 2)
- `refCounter.RemoveReference(baseKey, vmID)` — remove the pinned reference
- **Release references.lock**
- `cowMgr.RemoveOverlay(vmID)` — remove overlay if created
- Delete VM directory and any partial resources

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
4. **Stop VM** (if needed) → Call stop flow with configured `stop_timeout_seconds` (default 30s)
5. **Acquire references.lock** (Level 2)
6. **Remove reference** → `refCounter.RemoveReference(baseKey, vmID)`
7. **Release references.lock**
8. **Acquire name-index.lock** (Level 2 — never held with references.lock)
9. **Remove name from name-index.json**
10. **Release name-index.lock**
11. **Delete resources** (permanent):
    - Delete overlay: `rm overlay.qcow2`
    - Delete serial log: `rm /var/log/cocoon/{vm_id}-serial.log`
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

# Run with auto-cleanup (Phase 1: -d is a no-op; Phase 2: controls attach/detach mode)
cocoon run --rm ubuntu-22.04-cloudimg --name temp-vm
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

# Retag local image for a different registry namespace
cocoon image tag myorg/ubuntu-bootable:22.04 ghcr.io/acme/ubuntu-bootable:22.04

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

| Boot Contract Section      | CLI Implementation                                                                                                                        |
| -------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------- |
| §1 Boot Path Decision      | UEFI boot (Phase 1); resolver-driven OCI direct-boot auto-routing in Phase 2 (not yet implemented)                                        |
| §2 Guest Init Model        | Guest initialization is the user's responsibility; DHCP-based network config planned for Phase 2 ([16-networking.md](./16-networking.md)) |
| §3 I/O Mechanisms          | Serial console via `--serial file=...` (CH flag), `cocoon logs` command                                                                   |
| §4 Lifecycle Semantics     | `run`, `stop`, `delete`, `kill` commands                                                                                                  |
| §5 VM Configuration Schema | `types.VMConfig` in Go code                                                                                                               |
| §6 OCI to Bootable Bridge  | `ImageManager.VerifyBootability()` and conversion logic                                                                                   |

### 8.3 Storage Management Integration

| Storage Document Section | CLI Implementation                                                   |
| ------------------------ | -------------------------------------------------------------------- |
| COW Strategy             | `COWManager.CreateOverlay()` in VM creation flow                     |
| Reference Counting       | Automatic in `create` and `delete` flows                             |
| Garbage Collection       | `cocoon gc` command                                                  |
| Storage Layout           | Configured via `root_dir` / `runtime_dir` / `log_dir` in JSON config |

### 8.4 External References

- **Cloud Hypervisor API**: https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/vmm/src/api/openapi/cloud-hypervisor.yaml
- **urfave/cli/v2**: https://cli.urfave.org/v2/
- **qemu-img**: https://www.qemu.org/docs/master/tools/qemu-img.html

### Reconcile Strategy

**Policy**: Regular CLI commands (`start`, `stop`, `delete`, `list`, `inspect`, etc.) do **not** perform automatic reconciliation. They operate on the recorded state in `metadata.json` and trust that state reflects reality.

**`cocoon doctor`** is the sole entry point for reconciliation:
- Scans all VM directories for stale state (e.g., metadata says RUNNING but CH process is dead)
- Cleans up orphaned sockets and stale PID files
- Fixes metadata inconsistencies (transitions stale RUNNING → ERROR)
- Checks name index drift in dry-run mode; rebuilds name index only with `cocoon doctor --fix`

**Rationale**: Auto-reconcile on every command adds latency and complexity. Users who suspect state drift run `cocoon doctor` explicitly. This is the same pattern used by containerd and other container runtimes.

---

## 9. Implementation Checklist

### Phase 1: Core Commands (P0)

- [x] **CLI Framework** (`cmd/cocoon/main.go`, `cmd/cocoon/app.go`):
  - [x] Setup urfave/cli/v2 application structure
  - [x] Implement version command (`cmd/cocoon/version.go`)
  - [x] Implement global flags (--config, --root-dir, --runtime-dir, --log-dir, --log-level)

- [x] **VM Lifecycle**:
  - [x] `cocoon run` command with full flow (`cmd/cocoon/run.go`)
  - [x] `cocoon create` command (`cmd/cocoon/create.go`)
  - [x] `cocoon start` command with boot timeout (`cmd/cocoon/start.go`)
  - [x] `cocoon stop` command with ACPI shutdown (`cmd/cocoon/stop.go`)
  - [x] `cocoon delete` command with resource cleanup (`cmd/cocoon/rm.go`)
  - [x] `cocoon kill` command for force termination (`cmd/cocoon/kill.go`)

- [x] **Information Commands**:
  - [x] `cocoon list` with table/json output (`cmd/cocoon/ps.go`)
  - [x] `cocoon inspect` with detailed VM info (`cmd/cocoon/inspect.go`)
  - [x] `cocoon logs` with follow/tail options (`cmd/cocoon/logs.go`)

- [x] **Image Management** (multi-source) (`cmd/cocoon/images.go`):
  - [x] Image source auto-detection (qcow2 file / URL / OCI ref) (`image/pipeline/manager.go`)
  - [x] `cocoon image pull` from any source (qcow2, URL, OCI)
  - [x] `cocoon image list` from cache
  - [x] `cocoon image inspect` with metadata
  - [x] `cocoon image verify` for boot contract
  - [x] `cocoon image rm` with reference checking

- [x] **OCI VM Image Build** (`oci/`, `image/builder.go`):
  - [x] `cocoon image build` from cloud image or Cocoonfile (`oci/build_linux.go`, `oci/cocoonfile.go`)
  - [x] `cocoon image push` to container registries (`oci/push.go`)
  - [x] `cocoon image login` with credential storage (`oci/login.go`)
  - [x] `cocoon image inspect` for OCI VM images (`oci/layout.go`)
  - [x] OCI media types and VM config schema (`oci/types.go`)
  - [x] Local tag index for built images (`oci/store.go`)

- [x] **Storage** (`storage/local/`):
  - [x] Implement COWManager interface (`storage/storage.go`, `storage/local/cow.go`)
  - [x] COW overlay creation (`storage/local/cow.go`)
  - [x] Reference counter integration (`storage/local/refcount.go`)
  - [x] `cocoon gc` command (`cmd/cocoon/gc.go`, `storage/local/gc.go`)

### Phase 2: Advanced Features (P1)

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
  "boot_strategy": "uefi",
  "firmware_path": "/var/lib/cocoon/firmware/CLOUDHV.fd",
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
  "last_boot_mode": "uefi",
  "last_firmware_path": "/var/lib/cocoon/firmware/CLOUDHV.fd",
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

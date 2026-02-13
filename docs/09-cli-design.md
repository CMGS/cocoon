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

Cocoon follows the flat package organization pattern from the core project, emphasizing interface-driven design for testability and modularity.

```
cocoon/
├── main.go                    # CLI entry point using urfave/cli/v2
├── go.mod                     # Go 1.25+ module definition
├── config/
│   ├── config.go             # Configuration types and loading
│   └── defaults.go           # Default configuration values
├── vm/
│   ├── vm.go                 # VM lifecycle management interface and implementation
│   ├── create.go             # VM creation logic
│   ├── lifecycle.go          # Start/stop/delete operations
│   └── list.go               # List and inspect operations
├── image/
│   ├── image.go              # ImageManager interface (multi-source)
│   ├── resolve.go            # Image source auto-detection (qcow2/URL/OCI)
│   ├── buildah.go            # Buildah implementation for OCI image handling
│   └── convert.go            # OCI to qcow2 conversion logic
├── storage/
│   ├── storage.go            # StorageManager interface
│   ├── qcow2.go              # qcow2 operations implementation
│   └── layout.go             # Storage layout and path management
├── hypervisor/
│   ├── hypervisor.go         # Hypervisor interface (engine pattern from core)
│   ├── cloudhypervisor.go   # Cloud Hypervisor implementation
│   └── factory/
│       └── factory.go        # Factory for hypervisor selection
├── types/
│   ├── vm.go                 # VM types and specifications
│   ├── image.go              # Image types
│   ├── config.go             # Configuration types
│   └── errors.go             # Error definitions
├── client/
│   ├── client.go             # Cloud Hypervisor REST API client
│   └── types.go              # API request/response types
├── utils/
│   ├── fs.go                 # Filesystem utilities
│   └── validation.go         # Input validation
└── version/
    └── version.go            # Version information
```

---

## 2. Core Interfaces

### 2.1 Hypervisor Interface

Following the core project's engine pattern, the hypervisor interface abstracts VM operations:

```go
package hypervisor

import (
    "context"
    "io"
    "time"

    "github.com/CMGS/cocoon/types"
)

// API defines the hypervisor interface (similar to core's engine.API)
type API interface {
    // Info returns hypervisor information
    Info(ctx context.Context) (*types.HypervisorInfo, error)

    // Ping checks hypervisor connectivity
    Ping(ctx context.Context) error

    // CloseConn closes the connection
    CloseConn() error

    // VM lifecycle operations
    VMCreate(ctx context.Context, opts *types.VMCreateOptions) (*types.VMInfo, error)
    VMStart(ctx context.Context, id string) error
    VMStop(ctx context.Context, id string, gracefulTimeout time.Duration) error
    VMDelete(ctx context.Context, id string, force bool) error
    VMPause(ctx context.Context, id string) error
    VMResume(ctx context.Context, id string) error

    // VM information
    VMInspect(ctx context.Context, id string) (*types.VMInfo, error)
    VMList(ctx context.Context) ([]*types.VMInfo, error)

    // VM resource management
    VMResize(ctx context.Context, id string, cpus int, memory int64) error

    // Console access
    VMAttach(ctx context.Context, id string) (io.ReadWriteCloser, error)
}
```

### 2.2 ImageManager Interface

The ImageManager handles **multi-source image references**, not just OCI images.
Cocoon accepts three image source types:
1. **Local qcow2 file**: `/path/to/ubuntu-22.04-cloudimg.qcow2`
2. **Cloud image URL**: `https://cloud-images.ubuntu.com/.../ubuntu-22.04.img`
3. **OCI registry reference**: `myorg/ubuntu-bootable:22.04` (converted to qcow2 with bootability validation)

```go
package image

import (
    "context"
    "io"

    "github.com/CMGS/cocoon/types"
)

// SourceType identifies how an image reference should be resolved
type SourceType string

const (
    SourceQcow2 SourceType = "qcow2" // Local qcow2 file path
    SourceURL   SourceType = "url"   // Remote cloud image URL (downloaded + cached)
    SourceOCI   SourceType = "oci"   // OCI registry reference (pulled + converted + validated)
)

// Manager defines the image management interface (multi-source)
type Manager interface {
    // Resolve detects the source type of an image reference
    Resolve(ctx context.Context, ref string) (SourceType, error)

    // Pull fetches an image from any supported source and returns a cached qcow2 path
    // - qcow2 file: validates and returns path directly
    // - URL: downloads, caches, returns local path
    // - OCI ref: pulls via Buildah, converts to qcow2, validates bootability, caches
    Pull(ctx context.Context, ref string) (string, error)

    // List returns all cached images (qcow2 base images in cache)
    List(ctx context.Context, filter string) ([]*types.ImageInfo, error)

    // Inspect returns detailed image information
    Inspect(ctx context.Context, ref string) (*types.ImageInfo, error)

    // Remove deletes a cached image
    Remove(ctx context.Context, ref string, force bool) error

    // VerifyBootable checks if image meets boot contract requirements
    // For qcow2: inspects partitions via guestfish
    // For OCI: validates rootfs components before conversion
    VerifyBootable(ctx context.Context, ref string) error
}
```

### 2.3 StorageManager Interface

```go
package storage

import (
    "context"

    "github.com/CMGS/cocoon/types"
)

// Manager defines the storage management interface
type Manager interface {
    // CreateVolume creates a new qcow2 volume
    CreateVolume(ctx context.Context, opts *types.VolumeCreateOptions) (*types.VolumeInfo, error)

    // DeleteVolume removes a qcow2 volume
    DeleteVolume(ctx context.Context, path string) error

    // ListVolumes returns all volumes for a VM
    ListVolumes(ctx context.Context, vmID string) ([]*types.VolumeInfo, error)

    // ResizeVolume resizes a qcow2 volume
    ResizeVolume(ctx context.Context, path string, size int64) error

    // GetVolumeInfo returns volume information
    GetVolumeInfo(ctx context.Context, path string) (*types.VolumeInfo, error)

    // CloneVolume creates a copy-on-write clone
    CloneVolume(ctx context.Context, source, dest string) error

    // CreateOverlay creates COW overlay with backing file
    CreateOverlay(ctx context.Context, baseKey, vmID string) (*types.VolumeInfo, error)
}
```

### 2.4 Factory Pattern

Following core's factory pattern for hypervisor selection:

```go
package factory

import (
    "context"
    "fmt"

    "github.com/CMGS/cocoon/config"
    "github.com/CMGS/cocoon/hypervisor"
    "github.com/CMGS/cocoon/hypervisor/cloudhypervisor"
)

type factory func(ctx context.Context, config *config.Config, endpoint string) (hypervisor.API, error)

var hypervisors = map[string]factory{
    "cloud-hypervisor": cloudhypervisor.New,
    // Future: "firecracker": firecracker.New,
    // Future: "qemu": qemu.New,
}

// NewHypervisor creates a hypervisor instance based on configuration
func NewHypervisor(ctx context.Context, cfg *config.Config, hypervisorType string) (hypervisor.API, error) {
    fn, ok := hypervisors[hypervisorType]
    if !ok {
        return nil, fmt.Errorf("unsupported hypervisor type: %s", hypervisorType)
    }
    return fn(ctx, cfg, cfg.Hypervisor.Endpoint)
}
```

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

### 4.2 cocoon run (Create and Start)

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

### 4.3 cocoon create (Prepare VM)

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

### 4.4 cocoon start (Boot VM)

**Command**: `cocoon start <vm-ref> [FLAGS]`

**Purpose**: Start a previously created VM. `<vm-ref>` is resolved via [Section 3](#3-vm-identifier-resolution).

**Implementation**: Based on [Boot Contract §4.2](./01-boot-contract.md#42-cocoon-run-create-and-start)

```go
func StartCommand() *cli.Command {
    return &cli.Command{
        Name:      "start",
        Usage:     "Start a stopped VM",
        ArgsUsage: "<vm-ref>",
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

### 4.5 cocoon stop (Graceful Shutdown)

**Command**: `cocoon stop <vm-ref> [FLAGS]`

**Purpose**: Gracefully stop a running VM. `<vm-ref>` is resolved via [Section 3](#3-vm-identifier-resolution).

**Implementation**: Based on [Boot Contract §4.3](./01-boot-contract.md#43-cocoon-stop-graceful-shutdown)

```go
func StopCommand() *cli.Command {
    return &cli.Command{
        Name:      "stop",
        Usage:     "Stop a running VM",
        ArgsUsage: "<vm-ref>",
        Flags: []cli.Flag{
            &cli.DurationFlag{
                Name:  "timeout",
                Usage: "Graceful shutdown timeout",
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

### 4.6 cocoon delete (Remove VM)

**Command**: `cocoon delete <vm-ref> [FLAGS]`

**Purpose**: Delete VM and cleanup storage. `<vm-ref>` is resolved via [Section 3](#3-vm-identifier-resolution).

**Implementation**: Based on [Boot Contract §4.4](./01-boot-contract.md#44-cocoon-delete-remove-resources)

```go
func DeleteCommand() *cli.Command {
    return &cli.Command{
        Name:      "delete",
        Aliases:   []string{"rm"},
        Usage:     "Delete a VM and cleanup storage",
        ArgsUsage: "<vm-ref>",
        Flags: []cli.Flag{
            &cli.BoolFlag{
                Name:    "force",
                Aliases: []string{"f"},
                Usage:   "Force delete even if VM is running",
            },
        },
        Action: deleteAction,
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

### 4.7 cocoon kill (Force Terminate)

**Command**: `cocoon kill <vm-ref>`

**Purpose**: Force terminate a VM (SIGKILL). `<vm-ref>` is resolved via [Section 3](#3-vm-identifier-resolution).

**Implementation**: Based on [Boot Contract §4.5](./01-boot-contract.md#45-cocoon-kill-force-terminate)

```go
func KillCommand() *cli.Command {
    return &cli.Command{
        Name:      "kill",
        Usage:     "Force terminate a VM",
        ArgsUsage: "<vm-ref>",
        Action:    killAction,
    }
}
```

**Example Usage**:

```bash
# Force kill hung VM
cocoon kill myvm
```

### 4.8 cocoon list (List VMs)

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
vm-01HXYZ5A3B7C8D9E0F1G2H3J4K     myvm              RUNNING   2     2048M   2026-02-11T10:30:00Z
vm-01HABC9D8E7F6G5H4J3K2L1M0N     devbox            STOPPED   4     4096M   2026-02-10T14:20:00Z
vm-01H9ZZ8Y7X6W5V4U3T2S1R0Q9P     cocoon-a3f7b2c1   RUNNING   2     2048M   2026-02-09T08:15:00Z
```

Note: The `NAME` column shows the user-provided name or the auto-generated name (`cocoon-{random}` if `--name` was omitted at create time). Either the `VM ID` or `NAME` can be used as a `<vm-ref>` in subsequent commands. The `MEMORY` column displays the value in megabytes. The `CREATED` column shows the creation timestamp in RFC 3339 format.

### 4.9 cocoon inspect (VM Details)

**Command**: `cocoon inspect <vm-ref> [FLAGS]`

**Purpose**: Display detailed VM information. `<vm-ref>` is resolved via [Section 3](#3-vm-identifier-resolution).

```go
func InspectCommand() *cli.Command {
    return &cli.Command{
        Name:      "inspect",
        Usage:     "Display detailed VM information",
        ArgsUsage: "<vm-ref>",
        Flags: []cli.Flag{
            &cli.StringFlag{
                Name:  "format",
                Usage: "Output format (json)",
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

### 4.10 cocoon logs (Serial Console Output)

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

### 4.11 cocoon image (Image Management)

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

  2. Build a bootable OCI image with:
     cocoon image build-bootable --base ubuntu:22.04 --output myorg/ubuntu-bootable:22.04

  3. See docs/00-overview.md#supported-image-contract for details

Exit code: 1
```

### 4.12 cocoon gc (Garbage Collection)

**Command**: `cocoon gc [FLAGS]`

**Purpose**: Run garbage collection to cleanup unused resources

**Implementation**: Based on [05-storage-management.md](./05-storage-management.md)

```go
func GCCommand() *cli.Command {
    return &cli.Command{
        Name:  "gc",
        Usage: "Run garbage collection",
        Flags: []cli.Flag{
            &cli.IntFlag{
                Name:  "grace-period",
                Usage: "hours before unreferenced images are collected (0 = use config default)",
            },
            &cli.BoolFlag{
                Name:  "dry-run",
                Usage: "Show what would be collected without deleting",
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

### 4.13 cocoon doctor (System Health Check)

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
    issues, reconcileErr := app.vmMgr.Reconcile(ctx, fix, force)

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
- `0`: All required checks passed
- `1`: One or more required checks failed
- `2`: Dependency missing (with fix suggestions)

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

### 4.14 cocoon firmware (Firmware Management)

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

#### 4.14.1 cocoon firmware list

List all installed firmware files with paths and sizes.

```
$ cocoon firmware list
NAME             TYPE  PATH                                       SIZE     EXISTS
hypervisor-fw    PVH   /var/lib/cocoon/firmware/hypervisor-fw     89.2KB   true
CLOUDHV.fd       UEFI  /var/lib/cocoon/firmware/CLOUDHV.fd        2.1MB    true
```

#### 4.14.2 cocoon firmware install

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
1. Download firmware from the provided URL
2. Verify checksum against published SHA256
3. Back up existing firmware (if present)
4. Install new firmware to `/var/lib/cocoon/firmware/`
5. Update checksums.txt

**Example Output**:

```bash
$ cocoon firmware install --pvh-url https://github.com/cloud-hypervisor/rust-hypervisor-firmware/releases/download/0.5.0/hypervisor-fw
Downloading rust-hypervisor-firmware v0.5.0...
Downloaded: hypervisor-fw (89.2KB)
Verifying checksum: 3d7ae8c1a45b2e9f...
Checksum verified
Backing up existing firmware...
Backup created: hypervisor-fw-0.4.2
Installing firmware...
Installed: /var/lib/cocoon/firmware/hypervisor-fw

Firmware installation complete. Run 'cocoon doctor' to verify.
```

#### 4.14.3 cocoon firmware verify

Verify firmware file integrity using SHA256 checksums.

```bash
# Verify all firmware files
cocoon firmware verify
```

**Example Output**:

```bash
$ cocoon firmware verify
Verifying PVH firmware...
✅ hypervisor-fw: checksum matched (3d7ae8c1a45b2e9f...)
Verifying UEFI firmware...
⚠️  OVMF: No checksum file (system-managed firmware)

All firmware files verified successfully.
```

**Firmware Types**:
- `pvh`: rust-hypervisor-firmware (PVH boot, required)
- `uefi`: CLOUDHV.fd (UEFI boot, optional; deprecated fallback: system OVMF)
- `all`: All firmware types

**Firmware Storage**:
```
/var/lib/cocoon/firmware/
├── hypervisor-fw           # Current PVH firmware (x86_64)
├── hypervisor-fw-0.4.2     # Versioned backup
├── hypervisor-fw-0.4.1     # Older backup
├── CLOUDHV.fd              # UEFI firmware (Cloud Hypervisor edk2)
└── checksums.txt           # SHA256 verification
```

**Exit Codes**:
- `0`: Success
- `1`: Command failed (download error, checksum mismatch, etc.)
- `2`: Firmware not found

**Example Usage**:

```bash
# List installed firmware
cocoon firmware list

# Install PVH firmware from URL
cocoon firmware install --pvh-url https://github.com/cloud-hypervisor/rust-hypervisor-firmware/releases/download/0.5.0/hypervisor-fw

# Install UEFI firmware from URL
cocoon firmware install --uefi-url https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v50.0/CLOUDHV.fd

# Verify firmware integrity
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
       ID: vmID,
       Boot: types.BootConfig{
           Strategy: "pvh_then_uefi", // config.json: boot_strategy (immutable)
           Firmware: "/var/lib/cocoon/firmware/hypervisor-fw",
       },
       Disk: types.DiskConfig{
           RootDiskPath: overlayPath,         // auto-created in /var/lib/cocoon/vms/{vm-id}/
           Size:         c.String("disk"), // from --disk flag
       },
       Resources: types.ResourceConfig{
           CPUs:     cpus,
           MemoryMB: memoryMB,
       },
       IO: types.IOConfig{
           Serial: types.SerialConfig{
               Mode:    "file",
               LogFile: serialLogPath,
           },
       },
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
  "last_error": "",
  "error_count": 0,
  "updated_at": "2026-02-11T10:30:07Z",
  "started_at": "2026-02-11T10:30:05Z",
  "stopped_at": "",
  "schema_version": 1
}
```

---

**End of CLI Design Document v1.0**

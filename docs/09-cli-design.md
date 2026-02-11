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
   - Must contain: kernel, initrd, init system (systemd), bootloader
   - Requires build tooling (see docs/11-bootable-oci-build.md)

**NOT Supported**:
- Regular container images (`ubuntu:latest`, `python:3.11`, etc.)
- These lack kernel/bootloader and will **fail bootability validation**

See [00-overview.md § Supported Image Contract](./00-overview.md#️-supported-image-contract) for details.

---

## Executive Summary

This document defines the command-line interface for Cocoon, a lightweight VM management tool built on Cloud Hypervisor. The CLI follows Docker-like patterns for familiarity while exposing VM-specific capabilities like PVH/UEFI boot modes, resource allocation, and lifecycle management.

The design integrates the [Boot Contract](./01-boot-contract.md) decisions, including flexible boot modes (PVH preferred, UEFI optional), cloud-init task injection, serial console I/O, and graceful shutdown semantics. It also leverages the [storage management](./05-storage-management.md) system for efficient copy-on-write disk handling.

## Table of Contents

1. [Project Structure](#1-project-structure)
2. [Core Interfaces](#2-core-interfaces)
3. [CLI Commands](#3-cli-commands)
4. [Configuration](#4-configuration)
5. [Implementation Flow](#5-implementation-flow)
6. [Examples](#6-examples)
7. [Cross-References](#7-cross-references)

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
│   ├── image.go              # ImageManager interface
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

```go
package image

import (
    "context"
    "io"

    "github.com/CMGS/cocoon/types"
)

// Manager defines the image management interface
type Manager interface {
    // Pull downloads an OCI image from registry
    Pull(ctx context.Context, ref string) error

    // List returns available OCI images
    List(ctx context.Context, filter string) ([]*types.ImageInfo, error)

    // Inspect returns detailed image information
    Inspect(ctx context.Context, ref string) (*types.ImageInfo, error)

    // Remove deletes an OCI image
    Remove(ctx context.Context, ref string, force bool) error

    // ConvertToQcow2 converts OCI image to qcow2 format
    ConvertToQcow2(ctx context.Context, ref string, output string) (*types.Qcow2Info, error)

    // ExtractRootfs extracts the rootfs from OCI image
    ExtractRootfs(ctx context.Context, ref string) (io.ReadCloser, error)

    // VerifyBootable checks if image meets boot contract requirements
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
    CreateOverlay(ctx context.Context, baseImage, vmID string) (*types.VolumeInfo, error)
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

## 3. CLI Commands

Using `urfave/cli/v2` (same as core project):

### 3.1 Main Application Structure

```go
package main

import (
    "fmt"
    "os"

    "github.com/CMGS/cocoon/commands"
    "github.com/CMGS/cocoon/version"
    "github.com/urfave/cli/v2"
)

var configPath string

func main() {
    cli.VersionPrinter = func(c *cli.Context) {
        fmt.Print(version.String())
    }

    app := cli.NewApp()
    app.Name = version.NAME
    app.Usage = "Lightweight VM management with OCI images"
    app.Version = version.VERSION
    app.Flags = []cli.Flag{
        &cli.StringFlag{
            Name:        "config",
            Value:       "/etc/cocoon/config.yaml",
            Usage:       "config file path for cocoon, in yaml",
            Destination: &configPath,
            EnvVars:     []string{"COCOON_CONFIG_PATH"},
        },
    }

    app.Commands = []*cli.Command{
        commands.RunCommand(),
        commands.CreateCommand(),
        commands.StartCommand(),
        commands.StopCommand(),
        commands.DeleteCommand(),
        commands.KillCommand(),
        commands.ListCommand(),
        commands.InspectCommand(),
        commands.ImageCommand(),
        commands.LogsCommand(),
        commands.GCCommand(),
    }

    if err := app.Run(os.Args); err != nil {
        fmt.Fprintf(os.Stderr, "Error: %v\n", err)
        os.Exit(1)
    }
}
```

### 3.2 cocoon run (Create and Start)

**Command**: `cocoon run IMAGE [COMMAND] [FLAGS]`

**Purpose**: Create and start a VM in one operation (like `docker run`)

**Implementation**: Based on [Boot Contract §4.2](./01-boot-contract.md#42-cocoon-run-create-and-start)

```go
func RunCommand() *cli.Command {
    return &cli.Command{
        Name:      "run",
        Usage:     "Create and start a VM from an OCI image",
        ArgsUsage: "IMAGE [COMMAND...]",
        Flags: []cli.Flag{
            &cli.StringFlag{
                Name:    "name",
                Aliases: []string{"n"},
                Usage:   "VM name (auto-generated if not specified)",
            },
            &cli.IntFlag{
                Name:    "cpus",
                Aliases: []string{"c"},
                Usage:   "Number of vCPUs",
                Value:   2,
            },
            &cli.StringFlag{
                Name:    "memory",
                Aliases: []string{"m"},
                Usage:   "Memory size (e.g., 512M, 1G, 2G)",
                Value:   "2G",
            },
            &cli.StringFlag{
                Name:  "disk",
                Usage: "Disk size (e.g., 10G, 20G)",
                Value: "10G",
            },
            &cli.StringSliceFlag{
                Name:    "env",
                Aliases: []string{"e"},
                Usage:   "Set environment variables (KEY=VALUE)",
            },
            &cli.StringFlag{
                Name:    "workdir",
                Aliases: []string{"w"},
                Usage:   "Working directory in guest",
                Value:   "/root",
            },
            &cli.DurationFlag{
                Name:  "timeout",
                Usage: "Task execution timeout",
                Value: 300 * time.Second,
            },
            &cli.BoolFlag{
                Name:  "rm",
                Usage: "Automatically remove VM when it exits",
            },
            &cli.BoolFlag{
                Name:    "detach",
                Aliases: []string{"d"},
                Usage:   "Run VM in background",
            },
            &cli.StringFlag{
                Name:  "boot-mode",
                Usage: "Boot mode: uefi (default) or direct-kernel",
                Value: "uefi",
            },
            &cli.StringFlag{
                Name:  "firmware",
                Usage: "Path to UEFI firmware (OVMF)",
                Value: "/usr/share/OVMF/OVMF_CODE.fd",
            },
        },
        Action: runAction,
    }
}
```

**Behavior**:

1. **Create VM Configuration**: Generate VM ID, convert OCI image to qcow2, create cloud-init ISO
2. **Start Cloud Hypervisor**: Launch CH process with configured resources
3. **Wait for Boot**: Poll serial log for boot completion (timeout: 60s)
4. **Monitor Execution**: Stream serial output to user
5. **Auto-cleanup** (if `--rm`): Stop VM and delete resources on exit

**Example Usage**:

```bash
# Run Ubuntu with Python script
cocoon run ubuntu:22.04 python3 /workspace/script.py --cpus 4 --memory 4G

# Run with environment variables
cocoon run python:3.11 -e WORKSPACE=/workspace -e DEBUG=1 python main.py

# Run in background with auto-cleanup
cocoon run --rm -d alpine:3.19 /bin/sh -c "echo hello"
```

### 3.3 cocoon create (Prepare VM)

**Command**: `cocoon create IMAGE [FLAGS]`

**Purpose**: Create a VM without starting it

```go
func CreateCommand() *cli.Command {
    return &cli.Command{
        Name:      "create",
        Usage:     "Create a new VM from an OCI image",
        ArgsUsage: "IMAGE",
        Flags: []cli.Flag{
            &cli.StringFlag{
                Name:     "name",
                Aliases:  []string{"n"},
                Usage:    "VM name",
                Required: true,
            },
            &cli.StringFlag{
                Name:     "image",
                Aliases:  []string{"i"},
                Usage:    "OCI image reference",
                Required: true,
            },
            &cli.IntFlag{
                Name:    "cpus",
                Aliases: []string{"c"},
                Usage:   "Number of vCPUs",
                Value:   2,
            },
            &cli.StringFlag{
                Name:    "memory",
                Aliases: []string{"m"},
                Usage:   "Memory size (e.g., 512M, 1G)",
                Value:   "2G",
            },
            &cli.StringFlag{
                Name:  "disk",
                Usage: "Disk size (e.g., 10G, 20G)",
                Value: "10G",
            },
            &cli.StringSliceFlag{
                Name:    "env",
                Aliases: []string{"e"},
                Usage:   "Set environment variables",
            },
        },
        Action: createAction,
    }
}
```

**Example Usage**:

```bash
# Create VM (without starting)
cocoon create --name myvm --image ubuntu:22.04 --cpus 4 --memory 8G
```

### 3.4 cocoon start (Boot VM)

**Command**: `cocoon start VM_ID [FLAGS]`

**Purpose**: Start a previously created VM

**Implementation**: Based on [Boot Contract §4.2](./01-boot-contract.md#42-cocoon-run-create-and-start)

```go
func StartCommand() *cli.Command {
    return &cli.Command{
        Name:      "start",
        Usage:     "Start a VM",
        ArgsUsage: "<vm-id>",
        Flags: []cli.Flag{
            &cli.DurationFlag{
                Name:  "boot-timeout",
                Usage: "Boot timeout",
                Value: 60 * time.Second,
            },
            &cli.BoolFlag{
                Name:    "attach",
                Aliases: []string{"a"},
                Usage:   "Attach to VM console",
            },
        },
        Action: startAction,
    }
}
```

**Example Usage**:

```bash
# Start VM and attach to console
cocoon start myvm --attach

# Start with custom boot timeout
cocoon start myvm --boot-timeout 120s
```

### 3.5 cocoon stop (Graceful Shutdown)

**Command**: `cocoon stop VM_ID [FLAGS]`

**Purpose**: Gracefully stop a running VM

**Implementation**: Based on [Boot Contract §4.3](./01-boot-contract.md#43-cocoon-stop-graceful-shutdown)

```go
func StopCommand() *cli.Command {
    return &cli.Command{
        Name:      "stop",
        Usage:     "Stop a running VM",
        ArgsUsage: "<vm-id>",
        Flags: []cli.Flag{
            &cli.DurationFlag{
                Name:  "timeout",
                Usage: "Graceful shutdown timeout",
                Value: 30 * time.Second,
            },
            &cli.BoolFlag{
                Name:  "force",
                Usage: "Force stop VM (SIGKILL)",
            },
        },
        Action: stopAction,
    }
}
```

**Behavior**:

1. **Check VM State**: Verify VM is running
2. **Send ACPI Shutdown**: Trigger graceful shutdown via Cloud Hypervisor API
3. **Wait for Graceful Shutdown**: Wait up to `--timeout` seconds
4. **Force Kill on Timeout**: If timeout reached, send SIGKILL
5. **Verify Shutdown**: Confirm Cloud Hypervisor process exited

**Example Usage**:

```bash
# Graceful stop with 30s timeout
cocoon stop myvm

# Force stop immediately
cocoon stop myvm --force

# Custom timeout
cocoon stop myvm --timeout 60s
```

### 3.6 cocoon delete (Remove VM)

**Command**: `cocoon delete VM_ID [FLAGS]`

**Purpose**: Delete VM and cleanup storage

**Implementation**: Based on [Boot Contract §4.4](./01-boot-contract.md#44-cocoon-delete-remove-resources)

```go
func DeleteCommand() *cli.Command {
    return &cli.Command{
        Name:      "delete",
        Aliases:   []string{"rm"},
        Usage:     "Delete a VM and cleanup storage",
        ArgsUsage: "<vm-id>",
        Flags: []cli.Flag{
            &cli.BoolFlag{
                Name:    "force",
                Aliases: []string{"f"},
                Usage:   "Force delete even if VM is running",
            },
            &cli.BoolFlag{
                Name:  "volumes",
                Usage: "Remove associated volumes",
                Value: true,
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
4. **Delete Resources**: Remove overlay, serial log, cloud-init ISO, config files
5. **Mark as Deleted**: Update VM state

**Example Usage**:

```bash
# Delete stopped VM
cocoon delete myvm

# Force delete running VM
cocoon delete myvm --force

# Delete but keep volumes
cocoon delete myvm --volumes=false
```

### 3.7 cocoon kill (Force Terminate)

**Command**: `cocoon kill VM_ID`

**Purpose**: Force terminate a VM (SIGKILL)

**Implementation**: Based on [Boot Contract §4.5](./01-boot-contract.md#45-cocoon-kill-force-terminate)

```go
func KillCommand() *cli.Command {
    return &cli.Command{
        Name:      "kill",
        Usage:     "Force terminate a VM",
        ArgsUsage: "<vm-id>",
        Action:    killAction,
    }
}
```

**Example Usage**:

```bash
# Force kill hung VM
cocoon kill myvm
```

### 3.8 cocoon list (List VMs)

**Command**: `cocoon list [FLAGS]`

**Purpose**: List all VMs

```go
func ListCommand() *cli.Command {
    return &cli.Command{
        Name:    "list",
        Aliases: []string{"ls"},
        Usage:   "List all VMs",
        Flags: []cli.Flag{
            &cli.BoolFlag{
                Name:    "all",
                Aliases: []string{"a"},
                Usage:   "Show all VMs (including stopped)",
            },
            &cli.StringFlag{
                Name:  "format",
                Usage: "Output format (table, json, yaml)",
                Value: "table",
            },
            &cli.StringFlag{
                Name:  "filter",
                Usage: "Filter VMs by state (running, stopped, error)",
            },
        },
        Action: listAction,
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
cocoon list --filter running
```

**Output Example**:

```
VM ID          NAME     IMAGE           STATE     CPU  MEMORY  UPTIME
vm-abc-123     myvm1    ubuntu:22.04    running   2    2G      5m30s
vm-def-456     myvm2    python:3.11     stopped   4    4G      -
```

### 3.9 cocoon inspect (VM Details)

**Command**: `cocoon inspect VM_ID [FLAGS]`

**Purpose**: Display detailed VM information

```go
func InspectCommand() *cli.Command {
    return &cli.Command{
        Name:      "inspect",
        Usage:     "Display detailed VM information",
        ArgsUsage: "<vm-id>",
        Flags: []cli.Flag{
            &cli.StringFlag{
                Name:  "format",
                Usage: "Output format (json, yaml)",
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

# Inspect in YAML
cocoon inspect myvm --format yaml
```

**Output Example**:

```json
{
  "id": "vm-abc-123",
  "name": "myvm",
  "state": "running",
  "image": "ubuntu:22.04",
  "boot": {
    "mode": "uefi",
    "firmware_path": "/usr/share/OVMF/OVMF_CODE.fd"
  },
  "resources": {
    "cpus": 2,
    "memory_mb": 2048
  },
  "disk": {
    "root_disk_path": "/var/lib/cocoon/images/vm-abc-123.qcow2",
    "size": "10G",
    "base_image": "/var/lib/cocoon/cache/images/ubuntu-22.04-abc123.qcow2"
  },
  "created_at": "2026-02-11T10:30:00Z",
  "started_at": "2026-02-11T10:30:05Z",
  "uptime": "5m30s"
}
```

### 3.10 cocoon logs (Serial Console Output)

**Command**: `cocoon logs VM_ID [FLAGS]`

**Purpose**: View VM serial console logs

```go
func LogsCommand() *cli.Command {
    return &cli.Command{
        Name:      "logs",
        Usage:     "View VM serial console logs",
        ArgsUsage: "<vm-id>",
        Flags: []cli.Flag{
            &cli.BoolFlag{
                Name:    "follow",
                Aliases: []string{"f"},
                Usage:   "Follow log output",
            },
            &cli.IntFlag{
                Name:  "tail",
                Usage: "Number of lines to show from the end",
                Value: 100,
            },
            &cli.BoolFlag{
                Name:  "timestamps",
                Usage: "Show timestamps",
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

### 3.11 cocoon image (Image Management)

**Command**: `cocoon image SUBCOMMAND [FLAGS]`

**Purpose**: Manage OCI images

```go
func ImageCommand() *cli.Command {
    return &cli.Command{
        Name:  "image",
        Usage: "Manage OCI images",
        Subcommands: []*cli.Command{
            {
                Name:      "pull",
                Usage:     "Pull an OCI image from registry",
                ArgsUsage: "<image-ref>",
                Action:    imagePullAction,
            },
            {
                Name:    "list",
                Aliases: []string{"ls"},
                Usage:   "List available OCI images",
                Flags: []cli.Flag{
                    &cli.StringFlag{
                        Name:  "format",
                        Usage: "Output format (table, json)",
                        Value: "table",
                    },
                },
                Action: imageListAction,
            },
            {
                Name:      "inspect",
                Usage:     "Display detailed image information",
                ArgsUsage: "<image-ref>",
                Action:    imageInspectAction,
            },
            {
                Name:      "remove",
                Aliases:   []string{"rm"},
                Usage:     "Remove an OCI image",
                ArgsUsage: "<image-ref>",
                Flags: []cli.Flag{
                    &cli.BoolFlag{
                        Name:    "force",
                        Aliases: []string{"f"},
                        Usage:   "Force removal",
                    },
                },
                Action: imageRemoveAction,
            },
            {
                Name:      "verify",
                Usage:     "Verify image meets boot contract",
                ArgsUsage: "<image-ref>",
                Action:    imageVerifyAction,
            },
        },
    }
}
```

**Example Usage**:

```bash
# Pull cloud image (recommended)
cocoon image pull https://cloud-images.ubuntu.com/releases/22.04/release/ubuntu-22.04-server-cloudimg-amd64.img

# Pull bootable OCI image (custom-built)
cocoon image pull myorg/ubuntu-bootable:22.04

# List images
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

### 3.12 cocoon gc (Garbage Collection)

**Command**: `cocoon gc [FLAGS]`

**Purpose**: Run garbage collection to cleanup unused resources

**Implementation**: Based on [05-storage-management.md](./05-storage-management.md)

```go
func GCCommand() *cli.Command {
    return &cli.Command{
        Name:  "gc",
        Usage: "Run garbage collection",
        Flags: []cli.Flag{
            &cli.DurationFlag{
                Name:  "grace-period",
                Usage: "Grace period before deleting unreferenced images",
                Value: 24 * time.Hour,
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

# Run GC with custom grace period
cocoon gc --grace-period 12h
```

---

## 4. Configuration

Following core's YAML-based configuration pattern:

### 4.1 Configuration Structure

```go
package types

import (
    "time"
)

// Config holds cocoon configuration
type Config struct {
    // Storage configuration
    Storage StorageConfig `yaml:"storage" required:"true"`

    // Hypervisor configuration
    Hypervisor HypervisorConfig `yaml:"hypervisor" required:"true"`

    // Image configuration
    Image ImageConfig `yaml:"image" required:"true"`

    // Boot configuration
    Boot BootConfig `yaml:"boot"`

    // Global timeouts
    GlobalTimeout     time.Duration `yaml:"global_timeout" default:"300s"`
    ConnectionTimeout time.Duration `yaml:"connection_timeout" default:"10s"`

    // Logging
    Log LogConfig `yaml:"log"`
}

// StorageConfig defines storage settings
type StorageConfig struct {
    // Root directory for VM storage
    Root string `yaml:"root" default:"/var/lib/cocoon"`

    // Images directory (base qcow2 cache)
    ImagesDir string `yaml:"images_dir" default:"/var/lib/cocoon/cache/images"`

    // Volumes directory (VM overlays)
    VolumesDir string `yaml:"volumes_dir" default:"/var/lib/cocoon/vms"`

    // Temp directory
    TempDir string `yaml:"temp_dir" default:"/var/lib/cocoon/temp"`

    // Trash directory (soft delete)
    TrashDir string `yaml:"trash_dir" default:"/var/lib/cocoon/trash"`

    // Default volume size
    DefaultVolumeSize string `yaml:"default_volume_size" default:"10G"`
}

// HypervisorConfig defines hypervisor settings
type HypervisorConfig struct {
    // Type of hypervisor (cloud-hypervisor, firecracker, qemu)
    Type string `yaml:"type" default:"cloud-hypervisor"`

    // Binary path (for process-based hypervisors)
    BinaryPath string `yaml:"binary_path" default:"/usr/local/bin/cloud-hypervisor"`

    // Socket path for communication
    SocketPath string `yaml:"socket_path" default:"/var/run/cocoon"`

    // Default CPU count
    DefaultCPUs int `yaml:"default_cpus" default:"2"`

    // Default memory size
    DefaultMemory string `yaml:"default_memory" default:"2G"`
}

// ImageConfig defines image management settings
type ImageConfig struct {
    // Registry credentials
    Registries map[string]RegistryConfig `yaml:"registries"`

    // Image cache directory
    CacheDir string `yaml:"cache_dir" default:"/var/lib/cocoon/cache"`

    // Buildah storage root
    BuildahRoot string `yaml:"buildah_root" default:"/var/lib/cocoon/buildah"`
}

// RegistryConfig defines registry credentials
type RegistryConfig struct {
    Username string `yaml:"username"`
    Password string `yaml:"password"`
    Insecure bool   `yaml:"insecure"`
}

// BootConfig defines boot configuration
type BootConfig struct {
    // Default boot mode (uefi or direct-kernel)
    DefaultMode string `yaml:"default_mode" default:"uefi"`

    // UEFI firmware path
    UEFIFirmware string `yaml:"uefi_firmware" default:"/usr/share/OVMF/OVMF_CODE.fd"`

    // Boot timeout
    BootTimeout time.Duration `yaml:"boot_timeout" default:"60s"`
}

// LogConfig defines logging configuration
type LogConfig struct {
    Level   string `yaml:"level" default:"info"`
    UseJSON bool   `yaml:"use_json"`
    File    string `yaml:"file"`
}
```

### 4.2 Example Configuration File

```yaml
# /etc/cocoon/config.yaml

storage:
  root: /var/lib/cocoon
  images_dir: /var/lib/cocoon/cache/images
  volumes_dir: /var/lib/cocoon/vms
  temp_dir: /var/lib/cocoon/temp
  trash_dir: /var/lib/cocoon/trash
  default_volume_size: 20G

hypervisor:
  type: cloud-hypervisor
  binary_path: /usr/local/bin/cloud-hypervisor
  socket_path: /var/run/cocoon
  default_cpus: 2
  default_memory: 2G

image:
  cache_dir: /var/lib/cocoon/cache
  buildah_root: /var/lib/cocoon/buildah
  registries:
    docker.io:
      username: ""
      password: ""
    ghcr.io:
      username: myuser
      password: mytoken

boot:
  default_mode: uefi
  uefi_firmware: /usr/share/OVMF/OVMF_CODE.fd
  boot_timeout: 60s

global_timeout: 300s
connection_timeout: 10s

log:
  level: info
  use_json: false
  file: /var/log/cocoon.log
```

---

## 5. Implementation Flow

### 5.1 VM Creation Flow (`cocoon run`)

1. **Parse CLI flags** → Validate inputs
2. **Load configuration** → Read YAML config from `/etc/cocoon/config.yaml`
3. **Initialize managers**:
   - Create `HypervisorAPI` via factory
   - Create `ImageManager` with Buildah
   - Create `StorageManager` for qcow2 operations
   - Create `ReferenceCounter` for tracking
4. **Check/pull image** → `ImageManager.Pull(image)` if not cached
5. **Verify bootability** → `ImageManager.VerifyBootable(image)` (Boot Contract §6)
6. **Convert to qcow2** → `ImageManager.ConvertToQcow2(image, baseImagePath)`
7. **Create COW overlay** → `StorageManager.CreateOverlay(baseImage, vmID)`
8. **Generate cloud-init ISO** → Create NoCloud ISO with task command (Boot Contract §2.2.1)
9. **Configure VM**:
   ```go
   config := &types.VMConfig{
       ID: vmID,
       Boot: types.BootConfig{
           Mode: "uefi",
           UEFI: &types.UEFIConfig{
               FirmwarePath: "/usr/share/OVMF/OVMF_CODE.fd",
           },
       },
       Disk: types.DiskConfig{
           RootDiskPath: overlayPath,
           Size:         diskSize,
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
10. **Start Cloud Hypervisor**:
    ```bash
    cloud-hypervisor \
      --api-socket /var/run/cocoon/vm-123.sock \
      --disk path=/var/lib/cocoon/vms/vm-123/overlay.qcow2 \
      --disk path=/var/lib/cocoon/cloud-init/vm-123.iso,readonly=on \
      --cpus boot=2 \
      --memory size=2G \
      --serial file=/var/log/cocoon/vm-123-serial.log \
      --console off
    ```
11. **Wait for boot** → Poll serial log for boot completion marker (timeout: 60s)
12. **Update reference counter** → `refCounter.AddReference(baseImage, vmID)`
13. **Save VM metadata** → Write `config.json`, `metadata.json` to VM directory
14. **Stream output** → If not `--detach`, stream serial log to stdout
15. **Auto-cleanup** → If `--rm`, delete VM when process exits

### 5.2 VM Stop Flow (`cocoon stop`)

1. **Load VM config** → Read `config.json` from VM directory
2. **Check VM state** → Verify VM is running
3. **Send ACPI shutdown** → `PUT /api/v1/vm.power-button` (Boot Contract §4.3)
4. **Wait for graceful shutdown** → Poll CH process exit (timeout: 30s default)
5. **Force kill on timeout** → `syscall.Kill(chPid, syscall.SIGKILL)`
6. **Verify shutdown** → Confirm CH process terminated
7. **Update VM state** → Mark as `stopped` in metadata

### 5.3 VM Delete Flow (`cocoon delete`)

1. **Load VM config** → Read `config.json`
2. **Check VM state** → If running and no `--force`, error
3. **Stop VM** (if needed) → Call `cocoon stop --timeout 10s`
4. **Remove reference** → `refCounter.RemoveReference(baseImage, vmID)`
5. **Delete resources**:
   - Move overlay to trash: `mv overlay.qcow2 /var/lib/cocoon/trash/`
   - Delete serial log: `rm /var/log/cocoon/vm-123-serial.log`
   - Delete cloud-init ISO: `rm /var/lib/cocoon/cloud-init/vm-123.iso`
   - Delete API socket: `rm /var/run/cocoon/vm-123.sock`
   - Delete VM directory: `rm -rf /var/lib/cocoon/vms/vm-123/`
6. **Mark as deleted** → Update VM state to `deleted`
7. **Trigger GC** (optional) → If base image unreferenced, mark for collection

---

## 6. Examples

### 6.1 Basic VM Creation

```bash
# Create and start Ubuntu VM
cocoon run ubuntu:22.04 --name myvm --cpus 4 --memory 4G

# Run Python script with environment variables
cocoon run python:3.11 \
  -e WORKSPACE=/workspace \
  -e DEBUG=1 \
  --workdir /workspace \
  python main.py

# Run in background with auto-cleanup
cocoon run --rm -d alpine:3.19 /bin/sh -c "echo hello world"
```

### 6.2 VM Lifecycle Management

```bash
# Create VM without starting
cocoon create --name myvm --image ubuntu:22.04

# Start VM
cocoon start myvm

# View logs
cocoon logs myvm --follow

# Stop VM gracefully
cocoon stop myvm

# Force stop
cocoon stop myvm --force

# Delete VM
cocoon delete myvm

# Force delete running VM
cocoon delete myvm --force
```

### 6.3 Image Management

```bash
# Pull image
cocoon image pull ubuntu:22.04

# Verify bootability
cocoon image verify ubuntu:22.04

# List cached images
cocoon image list

# Inspect image details
cocoon image inspect ubuntu:22.04

# Remove image (fails if VMs using it)
cocoon image rm ubuntu:22.04

# Force remove image
cocoon image rm ubuntu:22.04 --force
```

### 6.4 Monitoring and Cleanup

```bash
# List all VMs
cocoon list --all

# Filter by state
cocoon list --filter running

# Inspect VM details
cocoon inspect myvm

# Run garbage collection
cocoon gc

# Dry run GC
cocoon gc --dry-run
```

### 6.5 High-Concurrency VM Pool

```bash
# Create 100 VMs from same base image (uses COW)
for i in {1..100}; do
  cocoon run ubuntu:22.04 \
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

## 7. Cross-References

### 7.1 Related Cocoon Documents

- [00-overview.md](./00-overview.md): Project motivation and architecture overview
- [01-boot-contract.md](./01-boot-contract.md): Boot modes, lifecycle semantics, VM configuration schema
- [02-installation.md](./02-installation.md): Cloud Hypervisor installation and prerequisites
- [05-storage-management.md](./05-storage-management.md): COW overlays, reference counting, garbage collection
- [06-concurrency.md](./06-concurrency.md): Thread-safety for reference counting and storage operations
- [08-dependencies.md](./08-dependencies.md): Required packages and tools

### 7.2 Boot Contract Integration

This CLI design implements the Boot Contract specification:

| Boot Contract Section | CLI Implementation |
|----------------------|-------------------|
| §1 Boot Path Decision | `--boot-mode` flag (default: uefi), `--firmware` flag |
| §2 Guest Init Model | cloud-init ISO generation in `cocoon run` |
| §3 I/O Mechanisms | Serial console via `--serial-log`, `cocoon logs` command |
| §4 Lifecycle Semantics | `run`, `stop`, `delete`, `kill` commands |
| §5 VM Configuration Schema | `types.VMConfig` in Go code |
| §6 OCI to Bootable Bridge | `ImageManager.VerifyBootable()` and conversion logic |

### 7.3 Storage Management Integration

| Storage Document Section | CLI Implementation |
|-------------------------|-------------------|
| COW Strategy | `StorageManager.CreateOverlay()` in VM creation flow |
| Reference Counting | Automatic in `create` and `delete` flows |
| Garbage Collection | `cocoon gc` command |
| Storage Layout | Configured via `storage.*` in YAML config |

### 7.4 External References

- **Cloud Hypervisor API**: https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/vmm/src/api/openapi/cloud-hypervisor.yaml
- **cloud-init NoCloud**: https://cloudinit.readthedocs.io/en/latest/topics/datasources/nocloud.html
- **urfave/cli/v2**: https://cli.urfave.org/v2/
- **qemu-img**: https://www.qemu.org/docs/master/tools/qemu-img.html

---

## 8. Implementation Checklist

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

- [ ] **Image Management**:
  - [ ] `cocoon image pull` via Buildah
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
  - [ ] Direct kernel boot support (`--boot-mode direct-kernel`)
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

## Appendix A: Complete VMConfig Example

```go
// Complete VM configuration as saved to disk
{
  "id": "vm-abc-123",
  "name": "myvm",
  "boot": {
    "mode": "uefi",
    "uefi": {
      "firmware_path": "/usr/share/OVMF/OVMF_CODE.fd"
    }
  },
  "disk": {
    "root_disk_path": "/var/lib/cocoon/vms/vm-abc-123/overlay.qcow2",
    "size": "10G",
    "oci_image": "ubuntu:22.04",
    "base_image": "/var/lib/cocoon/cache/images/ubuntu-22.04-abc123def456.qcow2"
  },
  "resources": {
    "cpus": 2,
    "memory_mb": 2048
  },
  "runtime": {
    "api_socket": "/var/run/cocoon/vm-abc-123.sock",
    "work_dir": "/var/lib/cocoon/vms/vm-abc-123",
    "state": "running",
    "process_id": 12345
  },
  "task": {
    "command": ["python3", "/workspace/main.py"],
    "env": {
      "WORKSPACE": "/workspace",
      "COCOON_TASK_ID": "task-123"
    },
    "working_dir": "/root",
    "cloud_init_iso": "/var/lib/cocoon/cloud-init/vm-abc-123.iso"
  },
  "io": {
    "serial": {
      "mode": "file",
      "log_file": "/var/log/cocoon/vm-abc-123-serial.log"
    }
  },
  "timeouts": {
    "boot": "60s",
    "stop": "30s",
    "task": "300s"
  },
  "created_at": "2026-02-11T10:30:00Z",
  "updated_at": "2026-02-11T10:30:05Z",
  "started_at": "2026-02-11T10:30:05Z"
}
```

---

**End of CLI Design Document v1.0**

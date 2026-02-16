# Volume Passthrough

**Version**: 1.0
**Status**: Planned
**Phase**: Phase 2
**Last Updated**: 2026-02-15

---

## Table of Contents

- [1. Overview](#1-overview)
- [2. Motivation](#2-motivation)
- [3. Phase 1 Behavior](#3-phase-1-behavior)
- [4. Design Options](#4-design-options)
- [5. Detailed Design: virtio-fs](#5-detailed-design-virtio-fs)
- [6. Config Changes](#6-config-changes)
- [7. virtiofsd Management](#7-virtiofsd-management)
- [8. Security](#8-security)
- [9. Interface Changes](#9-interface-changes)
- [10. Compatibility](#10-compatibility)
- [11. Implementation Plan](#11-implementation-plan)
- [12. Unresolved Questions](#12-unresolved-questions)
- [13. Cross-Feature Alignment](#13-cross-feature-alignment)
- [14. References](#14-references)

---

## 1. Overview

Volume passthrough enables sharing host directories and files with guest VMs.
This allows data to flow between the host filesystem and a running VM without
copying files into the guest disk image. The guest sees shared directories as
mounted filesystems, transparently backed by the host's storage.

The primary mechanism is **virtio-fs**, a high-performance shared filesystem
protocol purpose-built for VM-host file sharing. It uses a userspace filesystem
daemon (virtiofsd) on the host and the virtio transport inside the guest,
providing near-native filesystem performance with strong consistency guarantees.

```
┌──────────────────────────────────────────────────────────┐
│  Host                                                    │
│                                                          │
│  /data/models/  ──────┐                                  │
│  /src/project/  ────┐ │                                  │
│                     │ │                                   │
│          ┌──────────┴─┴───────────────┐                  │
│          │  virtiofsd (per volume)     │                  │
│          │  FUSE daemon on host        │                  │
│          └──────────┬─────────────────┘                  │
│                     │ Unix socket                        │
│          ┌──────────┴─────────────────┐                  │
│          │  Cloud Hypervisor           │                  │
│          │  virtio-fs device           │                  │
│          └──────────┬─────────────────┘                  │
│                     │ virtio transport                   │
├─────────────────────┼────────────────────────────────────┤
│  Guest VM           │                                    │
│                     v                                    │
│          ┌─────────────────────────┐                     │
│          │  /mnt/models/ (ro)      │                     │
│          │  /mnt/src/    (rw)      │                     │
│          │  (virtiofs mount)       │                     │
│          └─────────────────────────┘                     │
└──────────────────────────────────────────────────────────┘
```

---

## 2. Motivation

### 2.1 Development Workflows

Share source code from the host into a VM for in-VM compilation and testing.
Edits on the host are immediately visible in the guest without restart or copy.

```bash
cocoon run --volume /home/user/project:/mnt/src:rw myimage
# Inside guest: cd /mnt/src && make test
```

### 2.2 Data Pipelines

Share large datasets (CSVs, Parquet files, databases) into VMs without
duplicating multi-gigabyte files into each guest overlay. Multiple VMs can
mount the same dataset directory read-only.

```bash
cocoon run --volume /data/warehouse:/mnt/data:ro pipeline-image
```

### 2.3 AI Workloads

Share model weights and training data with guest VMs. Large language model
weights (tens to hundreds of gigabytes) should not be copied into each VM's
overlay disk. Read-only sharing via virtio-fs with DAX support provides
near-native read performance through memory-mapped access.

```bash
cocoon run --volume /models/llama-70b:/mnt/model:ro \
           --volume /data/training:/mnt/train:ro \
           ai-worker-image
```

### 2.4 Build Systems

Share build caches (ccache, Go module cache, pip cache) between the host
and guest VMs to avoid redundant downloads and compilations.

```bash
cocoon run --volume /var/cache/ccache:/mnt/ccache:rw \
           --volume /home/user/.cache/go-mod:/mnt/gomod \
           build-image
```

### 2.5 Logs and Output Collection

Mount a host directory as the output target so that build artifacts, test
results, or log files are written directly to the host filesystem. The
files survive VM deletion.

```bash
cocoon run --volume /var/log/vm-output:/mnt/output:rw results-image
# After VM stops, results are at /var/log/vm-output/
```

---

## 3. Phase 1 Behavior

**Current (Phase 1)**: VMs have NO volume sharing capability.

- No virtio-fs, no 9p, no shared directories
- All data lives inside the qcow2 overlay or base image
- Files must be baked into the image at build time or copied in manually
- No `--volume` flag on `cocoon create` or `cocoon run`

This is intentional for Phase 1, which focuses on core VM lifecycle management.
Volume passthrough is a Phase 2 feature that builds on the existing hypervisor
integration and process management infrastructure.

---

## 4. Design Options

### Option A: virtio-fs (virtiofsd) -- Recommended

**Mechanism**: A userspace FUSE daemon (virtiofsd) runs on the host and serves
filesystem requests from the guest over a Unix domain socket. Cloud Hypervisor
presents the shared filesystem as a virtio-fs device to the guest.

**CH API**: The `fs` array in the `vm.create` payload:

```json
{
  "fs": [
    {
      "tag": "mydata",
      "socket": "/run/cocoon/vms/vm-ABC123/virtiofs-mydata.sock",
      "num_queues": 1,
      "queue_size": 1024
    }
  ]
}
```

**Guest mount**: `mount -t virtiofs mydata /mnt/data`

**Pros**:
- Best performance for shared filesystems (FUSE bypass, DAX support)
- Strong POSIX semantics and consistency
- Designed specifically for VM-host sharing
- Active development by virtio community
- Supports read-only and read-write modes
- DAX (Direct Access) eliminates data copies for read-heavy workloads
- Cloud Hypervisor has native support

**Cons**:
- Requires virtiofsd binary on the host (additional dependency)
- One virtiofsd process per shared directory
- Process lifecycle management adds complexity
- Guest kernel must support virtio-fs (Linux 5.4+, all modern distros)

### Option B: Additional Block Devices (raw/qcow2)

**Mechanism**: Create additional qcow2 or raw disk images and attach them to
the VM as extra block devices. The guest sees them as `/dev/vdX`.

**CH API**: Additional entries in the `disks` array:

```json
{
  "disks": [
    {"path": "/var/lib/cocoon/vms/vm-ABC123/overlay.qcow2"},
    {"path": "/data/extra-volume.raw", "readonly": true}
  ]
}
```

**Pros**:
- No additional daemon (virtiofsd) required
- Simple implementation -- reuses existing disk attachment code
- Good for structured data that does not need host-side live editing
- Works with any guest OS

**Cons**:
- Data must be in a disk image format, not a plain directory
- No live host-guest sharing -- host cannot modify files while VM is running
- Requires filesystem creation (mkfs) inside the image
- Not suitable for development workflows (edit-on-host, see-in-guest)

### Option C: 9p (Plan 9 Filesystem Protocol) -- Deprecated

**Mechanism**: The 9p protocol provides directory sharing similar to virtio-fs
but with inferior performance and weaker consistency guarantees.

**Pros**:
- Mature protocol, widely supported in older hypervisors
- Simple implementation

**Cons**:
- Significantly worse performance than virtio-fs (2-10x slower)
- Weaker POSIX semantics (mmap, locking issues)
- Not actively developed for modern VMMs
- Cloud Hypervisor does NOT support 9p natively
- Considered deprecated in favor of virtio-fs

### Option D: NFS/SMB over Network -- Not Recommended for Phase 2

**Mechanism**: Run NFS or SMB server on the host, configure guest networking,
and mount the network share inside the guest.

**Pros**:
- Standard network filesystem protocols
- Works across hosts (not just local sharing)

**Cons**:
- Requires VM networking (Phase 2 dependency on networking.md)
- Additional server setup and configuration
- Higher latency than virtio-fs
- Security complexity (network exposure)
- Overkill for local host-to-guest sharing

### Recommendation

**Primary: Option A (virtio-fs)** for all directory sharing use cases. This
provides the best performance, strongest consistency, and most natural UX
(Docker-like `--volume` syntax).

**Supplementary: Option B (block devices)** for pre-built data volumes that
do not require live host-side editing. Useful for distributing read-only
datasets packaged as disk images.

Option C is rejected (deprecated, not supported by CH). Option D is deferred
until networking support lands.

---

## 5. Detailed Design: virtio-fs

### 5.1 CLI Interface

Volume passthrough uses Docker-like syntax on `create` and `run` commands:

```
--volume HOST_PATH:GUEST_MOUNT[:OPTIONS]
```

Short form: `-v`

**Options**:
- `ro` -- read-only mount (default)
- `rw` -- explicitly read-write (must be specified)

**Examples**:

```bash
# Single read-only volume (default mode)
cocoon create --volume /host/src:/mnt/src myimage

# Single read-write volume (explicit :rw required)
cocoon create --volume /host/src:/mnt/src:rw myimage

# Multiple volumes with mixed modes
cocoon run \
  --volume /data/models:/mnt/models \
  --volume /output:/mnt/output:rw \
  --name worker \
  ai-image

# Short form
cocoon run -v /src:/mnt/src -v /data:/mnt/data:ro myimage
```

**Validation rules**:
- `HOST_PATH` must be an absolute path and must exist at create time
- `GUEST_MOUNT` must be an absolute path
- `HOST_PATH` must not escape allowed directories (see [Security](#8-security))
- Duplicate `GUEST_MOUNT` paths are rejected
- Empty `HOST_PATH` or `GUEST_MOUNT` is rejected

**CLI flag definition** (added to `vmCreateFlags()` in `cmd/cocoon/create.go`):

```go
&cli.StringSliceFlag{
    Name:    "volume",
    Aliases: []string{"v"},
    Usage:   "bind mount a host directory into the guest (HOST:GUEST[:ro|:rw], default ro)",
},
&cli.BoolFlag{
    Name:  "allow-dangerous-paths",
    Usage: "allow mounting sensitive host paths (/, /etc, /var/run) as volumes",
},
```

### 5.2 Volume Parsing

The `--volume` flag value is parsed into a `VolumeMount` struct:

```go
// ParseVolumeFlag parses a --volume flag value into a VolumeMount.
// Format: HOST_PATH:GUEST_MOUNT[:ro|:rw]
func ParseVolumeFlag(spec string) (*VolumeMount, error) {
    parts := strings.SplitN(spec, ":", 3)
    if len(parts) < 2 {
        return nil, fmt.Errorf("invalid volume spec %q: expected HOST:GUEST[:ro]", spec)
    }

    vm := &VolumeMount{
        HostPath:   parts[0],
        GuestMount: parts[1],
        ReadOnly:   true, // Default: read-only for security (see §8.2)
    }

    if len(parts) == 3 {
        switch parts[2] {
        case "ro":
            vm.ReadOnly = true
        case "rw":
            vm.ReadOnly = false
        default:
            return nil, fmt.Errorf("invalid volume option %q: expected 'ro' or 'rw'", parts[2])
        }
    }

    // Validate paths.
    if !filepath.IsAbs(vm.HostPath) {
        return nil, fmt.Errorf("host path must be absolute: %s", vm.HostPath)
    }
    if !filepath.IsAbs(vm.GuestMount) {
        return nil, fmt.Errorf("guest mount must be absolute: %s", vm.GuestMount)
    }

    return vm, nil
}
```

### 5.3 Volume Tags

Each shared directory needs a unique tag for the virtio-fs device. The tag is
used as the mount source inside the guest (`mount -t virtiofs <tag> <mountpoint>`).

Tags are derived from the guest mount path by sanitizing it:

```go
// VolumeTag derives a virtio-fs device tag from the guest mount path.
// Example: "/mnt/data" -> "mnt-data"
//          "/mnt/my-project" -> "mnt-my-project"
func VolumeTag(guestMount string) string {
    tag := strings.TrimPrefix(guestMount, "/")
    tag = strings.ReplaceAll(tag, "/", "-")
    // CH tag max length is 36 characters.
    if len(tag) > 36 {
        tag = tag[:36]
    }
    return tag
}
```

### 5.4 virtiofsd Lifecycle

For each `--volume` flag, Cocoon spawns a virtiofsd process before creating
the VM via the CH REST API. The virtiofsd listens on a Unix domain socket
that Cloud Hypervisor connects to.

**Socket path**: `/run/cocoon/vms/{vmID}/virtiofs-{tag}.sock`

**Process flow**:

```
cocoon create --volume /data:/mnt/data myimage
  │
  ├─ 1. Validate volume spec
  ├─ 2. Create VM directory, write config.json + metadata.json
  ├─ 3. Pin reference, create overlay (existing flow)
  └─ 4. Record volumes in config.json

cocoon start <vmID>
  │
  ├─ 1. Load config, check volumes
  ├─ 2. For each volume:
  │     ├─ Derive tag from guest mount path
  │     ├─ Compute socket path
  │     ├─ Spawn virtiofsd process
  │     └─ Wait for virtiofsd socket to be ready
  ├─ 3. Launch Cloud Hypervisor process (existing flow)
  ├─ 4. Build CHVMConfig with fs[] entries
  ├─ 5. PUT /api/v1/vm.create (includes fs config)
  ├─ 6. PUT /api/v1/vm.boot
  └─ 7. Record virtiofsd PIDs in metadata.json

cocoon stop <vmID>
  │
  ├─ 1. Graceful shutdown of CH (existing flow)
  ├─ 2. For each virtiofsd process:
  │     ├─ Send SIGTERM
  │     ├─ Wait with timeout
  │     └─ Send SIGKILL if still alive
  └─ 3. Clean up virtiofsd sockets

cocoon delete <vmID>
  │
  ├─ 1. Force stop if running (existing flow)
  ├─ 2. Kill any remaining virtiofsd processes
  └─ 3. Remove runtime directory (sockets cleaned up)
```

**virtiofsd spawn command**:

```go
// launchVirtiofsd starts a virtiofsd process for a single shared directory.
func launchVirtiofsd(ctx context.Context, vol *VolumeConfig, socketPath string) (int, error) {
    args := []string{
        "--socket-path", socketPath,
        "--shared-dir", vol.HostPath,
        "--cache=auto",
        "--sandbox=chroot",
    }
    if vol.ReadOnly {
        // virtiofsd does not have a --readonly flag; read-only is enforced
        // at the CH level via the fs config and guest mount options.
        // We additionally run virtiofsd with restricted permissions.
    }

    cmd := exec.CommandContext(ctx, "virtiofsd", args...)
    configureDaemonProcess(cmd) // Detach from process group, like CH

    if err := cmd.Start(); err != nil {
        return 0, fmt.Errorf("start virtiofsd for %s: %w", vol.HostPath, err)
    }

    pid := cmd.Process.Pid
    _ = cmd.Process.Release()
    return pid, nil
}
```

### 5.5 Cloud Hypervisor Configuration

The `CHVMConfig` struct gains a new `Fs` field for virtio-fs devices. This
is sent to CH in the `PUT /api/v1/vm.create` payload.

```go
// In hypervisor/types.go

// CHFsConfig describes a single virtio-fs shared filesystem.
type CHFsConfig struct {
    Tag       string `json:"tag"`
    Socket    string `json:"socket"`
    NumQueues int    `json:"num_queues"`
    QueueSize int    `json:"queue_size"`
}
```

The `buildCHVMConfig` function in `vm/engine/manager.go` is extended:

```go
func buildCHVMConfig(vmCfg *types.VMConfig) *hypervisor.CHVMConfig {
    cfg := &hypervisor.CHVMConfig{
        CPUs: hypervisor.CHCPUConfig{
            BootVCPUs: vmCfg.CPUs,
        },
        Memory: hypervisor.CHMemoryConfig{
            Size: vmCfg.MemoryMB * 1024 * 1024,
        },
        Disks: []hypervisor.CHDiskConfig{
            {Path: vmCfg.OverlayPath, ReadOnly: false},
        },
        Serial: hypervisor.CHSerialConfig{
            Mode: "File",
            File: vmCfg.SerialLog,
        },
        Console: hypervisor.CHConsoleConfig{
            Mode: "Off",
        },
    }

    // Add virtio-fs entries for shared volumes.
    // Socket paths are derived at runtime, not read from VolumeConfig.
    for _, vol := range vmCfg.Volumes {
        socketPath := cocoonCfg.VMVirtiofsSocketPath(vmCfg.ID, vol.Tag)
        cfg.Fs = append(cfg.Fs, hypervisor.CHFsConfig{
            Tag:       vol.Tag,
            Socket:    socketPath,
            NumQueues: 1,
            QueueSize: 1024,
        })
    }

    return cfg
}
```

When virtio-fs is configured, Cloud Hypervisor requires `shared=on` in the
memory configuration to enable memory sharing between the VMM and virtiofsd:

```go
// CHMemoryConfig specifies the guest memory size.
type CHMemoryConfig struct {
    Size         int64 `json:"size"`
    Shared       bool  `json:"shared,omitempty"`        // Required for virtio-fs
    HugePages    bool  `json:"hugepages,omitempty"`     // Optional, improves perf
    HotplugSize  int64 `json:"hotplug_size,omitempty"`  // Future: memory hotplug
}
```

The memory config is adjusted when volumes are present:

```go
if len(vmCfg.Volumes) > 0 {
    cfg.Memory.Shared = true
}
```

### 5.6 Guest Auto-Mount

Shared filesystems appear inside the guest as virtio-fs devices but are not
automatically mounted. There are three strategies for guest-side mounting:

**Strategy A: Tag-convention auto-mount (recommended)**

Cocoon derives virtio-fs tags from the guest mount path (e.g., tag `mnt-data`
for mount point `/mnt/data`). A guest-side udev rule or systemd generator
discovers virtio-fs devices and auto-mounts them by converting the tag back
to a path (`-` becomes `/`):

```bash
# /etc/udev/rules.d/99-virtiofs-automount.rules
# When a virtiofs device appears, derive mount path from tag and mount it.
ACTION=="add", SUBSYSTEM=="virtio", ATTR{name}=="?*", \
  RUN+="/bin/sh -c 'TAG=%k; MPATH=/$$(echo $$TAG | sed s/-/\\//g); mkdir -p $$MPATH && mount -t virtiofs $$TAG $$MPATH'"
```

Alternatively, a simple systemd generator script placed in the guest image at
`/etc/systemd/system-generators/virtiofs-mount-generator` can enumerate
virtio-fs tags at boot and emit `.mount` units dynamically. This approach
requires no per-VM configuration and works with any guest image that includes
the generator.

The tag-to-path convention is deterministic: tag `mnt-data` always mounts at
`/mnt/data`, tag `mnt-src` always mounts at `/mnt/src`. Users who need
non-standard mount points can mount manually.

**Strategy B: systemd mount units (baked into image)**

For production workloads with known mount points, systemd `.mount` unit files
can be baked directly into the guest image:

```ini
# /etc/systemd/system/mnt-data.mount
[Unit]
Description=Mount virtiofs share (mnt-data)
After=local-fs.target
ConditionPathExists=/sys/fs/virtiofs

[Mount]
What=mnt-data
Where=/mnt/data
Type=virtiofs
Options=defaults

[Install]
WantedBy=multi-user.target
```

This is the user's responsibility to include in their guest image. Cocoon
does not inject files into the guest filesystem.

**Strategy C: Guest agent (future)**

A lightweight Cocoon guest agent could discover virtio-fs devices and
auto-mount them. This is the most seamless approach but requires agent
installation in the guest image.

**Recommendation**: Start with Strategy A (tag-convention auto-mount) for
Phase 2.1. Strategy B is available for users who prefer explicit systemd units
baked into their images. Defer Strategy C to a later phase.

### 5.7 Complete Example Flow

```bash
$ cocoon run -v /home/user/src:/mnt/src:rw -v /data/models:/mnt/models ai-image
```

Step-by-step:

1. Parse `--volume` flags into `[]VolumeMount`
2. Validate host paths exist and are in the allowlist, guest paths are absolute, no duplicates
3. Create VM (generate ID, prepare image, create overlay, write config)
4. Record volumes in config.json (immutable after creation)
5. Start VM:
   a. Spawn virtiofsd for `/home/user/src` on socket `virtiofs-mnt-src.sock`
   b. Spawn virtiofsd for `/data/models` on socket `virtiofs-mnt-models.sock`
   c. Wait for both sockets to be connectable
   d. Launch Cloud Hypervisor with `--api-socket`
   e. Send `PUT /api/v1/vm.create` with `fs` array referencing both sockets
   f. Send `PUT /api/v1/vm.boot`
   g. Wait for boot detection (serial log patterns)
6. Guest kernel discovers two virtio-fs devices: `mnt-src`, `mnt-models`
7. Guest auto-mounts them via tag convention (or user mounts manually):
   - `mount -t virtiofs mnt-src /mnt/src` (rw, explicit)
   - `mount -t virtiofs mnt-models /mnt/models -o ro` (ro, default)
8. Guest can read/write `/mnt/src`, read `/mnt/models`

---

## 6. Config Changes

### 6.1 VolumeConfig (new type in `types/config.go`)

```go
// VolumeConfig describes a single shared volume attached to a VM.
// Stored in config.json (immutable after creation).
type VolumeConfig struct {
    // HostPath is the absolute path on the host to share.
    HostPath string `json:"host_path"`

    // GuestMount is the absolute path where the volume appears in the guest.
    GuestMount string `json:"guest_mount"`

    // ReadOnly controls whether the guest can write to the shared directory.
    ReadOnly bool `json:"read_only,omitempty"`

    // Tag is the virtio-fs device tag, derived from GuestMount.
    // Used as the mount source in the guest: mount -t virtiofs <tag> <mount>.
    Tag string `json:"tag"`

    // SocketPath is NOT persisted — derived at runtime via
    // cfg.VMVirtiofsSocketPath(vmID, tag) under /run/cocoon/vms/{vmID}/
}
```

### 6.2 VMConfig Changes (in `types/config.go`)

Add `Volumes` field to `VMConfig`:

```go
type VMConfig struct {
    // ... existing fields ...

    // Volumes lists shared host directories.
    // Empty if no volumes are configured (Phase 1 default).
    Volumes []VolumeConfig `json:"volumes,omitempty"`

    // ... existing fields ...
}
```

SchemaVersion stays at `1`. The new `Volumes` field uses `omitempty`, so it is
omitted for VMs without volumes and additive for VMs with volumes. This follows
the additive-fields strategy from [04.1-oci-vm-images.md](./04.1-oci-vm-images.md)
— no schema version bump is needed for backward-compatible additions.

### 6.3 CreateOptions Changes (in `vm/types.go`)

Add `Volumes` to the create options:

```go
type CreateOptions struct {
    // ... existing fields ...

    // Volumes is a list of host:guest volume mounts.
    // Parsed from --volume CLI flags.
    Volumes []VolumeMount `json:"volumes,omitempty"`
}

// VolumeMount represents a parsed --volume flag before tag/socket derivation.
type VolumeMount struct {
    HostPath   string `json:"host_path"`
    GuestMount string `json:"guest_mount"`
    ReadOnly   bool   `json:"read_only"`
}
```

### 6.4 VMMetadataFile Changes (in `types/metadata.go`)

Track virtiofsd PIDs in metadata for lifecycle management:

```go
type VMMetadataFile struct {
    // ... existing fields ...

    // VirtiofsPIDs maps volume tags to their virtiofsd process PIDs.
    // Populated on Start, cleared on Stop/Delete.
    VirtiofsPIDs map[string]int `json:"virtiofs_pids,omitempty"`
}
```

### 6.5 CHVMConfig Changes (in `hypervisor/types.go`)

```go
type CHVMConfig struct {
    CPUs    CHCPUConfig     `json:"cpus"`
    Memory  CHMemoryConfig  `json:"memory"`
    Disks   []CHDiskConfig  `json:"disks,omitempty"`
    Fs      []CHFsConfig    `json:"fs,omitempty"`       // NEW
    Serial  CHSerialConfig  `json:"serial"`
    Console CHConsoleConfig `json:"console"`
}

type CHMemoryConfig struct {
    Size   int64 `json:"size"`
    Shared bool  `json:"shared,omitempty"`               // NEW
}

type CHFsConfig struct {
    Tag       string `json:"tag"`
    Socket    string `json:"socket"`
    NumQueues int    `json:"num_queues"`
    QueueSize int    `json:"queue_size"`
}
```

### 6.6 Config Path Helpers (in `config/config.go`)

```go
// VMVirtiofsSocketPath returns the virtiofsd socket path for a volume tag.
func (c *CocoonConfig) VMVirtiofsSocketPath(vmID, tag string) string {
    return filepath.Join(c.RuntimeDir, "vms", vmID, "virtiofs-"+tag+".sock")
}

// VMVirtiofsPIDPath returns the PID file path for a virtiofsd process.
func (c *CocoonConfig) VMVirtiofsPIDPath(vmID, tag string) string {
    return filepath.Join(c.RuntimeDir, "vms", vmID, "virtiofs-"+tag+".pid")
}
```

### 6.7 CocoonConfig Changes

Add virtiofsd binary path and volume security settings to global config:

```go
type CocoonConfig struct {
    // ... existing fields ...

    // VirtiofsdBinary is the path to the virtiofsd binary.
    // Defaults to "virtiofsd" (resolved via PATH).
    VirtiofsdBinary string `json:"virtiofsd_binary"`

    // VolumeAllowedPaths is the list of host path prefixes that are allowed
    // as volume sources without --allow-dangerous-paths. See §8.1.
    VolumeAllowedPaths []string `json:"volume_allowed_paths"`

    // AllowDangerousVolumePaths, if true, disables the volume path allowlist
    // globally. Equivalent to always passing --allow-dangerous-paths.
    AllowDangerousVolumePaths bool `json:"allow_dangerous_volume_paths,omitempty"`
}
```

Default:

```go
func DefaultConfig() *CocoonConfig {
    return &CocoonConfig{
        // ... existing defaults ...
        VirtiofsdBinary: "virtiofsd",
        VolumeAllowedPaths: []string{
            "/var/lib/cocoon/shares/",
            "/home/",
            "/tmp/",
        },
    }
}
```

### 6.8 VMInspect Changes (in `types/inspect.go`)

Add volume information to the inspect output:

```go
type VMInspect struct {
    // ... existing fields ...
    Volumes []InspectVolumeInfo `json:"volumes,omitempty"`
}

type InspectVolumeInfo struct {
    HostPath   string `json:"host_path"`
    GuestMount string `json:"guest_mount"`
    ReadOnly   bool   `json:"read_only"`
    Tag        string `json:"tag"`
    DaemonPID  int    `json:"daemon_pid,omitempty"`
}
```

---

## 7. virtiofsd Management

### 7.1 Process Lifecycle

virtiofsd processes follow a lifecycle tightly coupled to the VM:

```
VM Create      VM Start                                VM Stop         VM Delete
    │              │                                       │               │
    │              ├─ spawn virtiofsd (per volume)         │               │
    │              ├─ wait for socket ready                │               │
    │              ├─ launch CH                            │               │
    │              ├─ vm.create (with fs[])                │               │
    │              ├─ vm.boot                              │               │
    │              │                                       │               │
    │              │  [VM running, virtiofsd serving]      │               │
    │              │                                       │               │
    │              │                ┌──────────────────────┤               │
    │              │                │  ACPI shutdown CH    │               │
    │              │                │  SIGTERM virtiofsd   │               │
    │              │                │  Wait + SIGKILL      │               │
    │              │                │  Cleanup sockets     │               │
    │              │                └──────────────────────┘               │
    │              │                                                       │
    │              │                          ┌────────────────────────────┤
    │              │                          │  Kill virtiofsd if alive   │
    │              │                          │  Remove runtime dir        │
    │              │                          └────────────────────────────┘
```

**Key invariant**: virtiofsd must be started BEFORE `vm.create` and stopped
AFTER the CH process exits. The socket must be listening before CH attempts
to connect to it.

### 7.2 PID Tracking

Each virtiofsd process PID is tracked in two places:

1. **PID files**: `/run/cocoon/vms/{vmID}/virtiofs-{tag}.pid` -- for process
   management across CLI invocations
2. **metadata.json**: `virtiofs_pids` map -- for inspect and reconciliation

This mirrors the existing pattern where the CH process PID is stored in both
`ch.pid` and `metadata.json.process_pid`.

### 7.3 Socket Management

virtiofsd sockets live under the VM runtime directory:

```
/run/cocoon/vms/{vmID}/
    api.sock                    # CH REST API socket (existing)
    ch.pid                      # CH process PID (existing)
    virtiofs-mnt-data.sock      # virtiofsd socket for /mnt/data
    virtiofs-mnt-data.pid       # virtiofsd PID for /mnt/data
    virtiofs-mnt-src.sock       # virtiofsd socket for /mnt/src
    virtiofs-mnt-src.pid        # virtiofsd PID for /mnt/src
```

Socket cleanup follows the same best-effort pattern used for `api.sock`:
remove stale sockets before spawning new processes.

### 7.4 Spawn and Wait

```go
// startVolumeDaemons launches virtiofsd processes for all configured volumes.
// Returns a map of tag -> PID for metadata tracking.
func (m *manager) startVolumeDaemons(ctx context.Context, vmID string, volumes []types.VolumeConfig) (map[string]int, error) {
    pids := make(map[string]int, len(volumes))

    for _, vol := range volumes {
        // Derive socket path at runtime — not stored in VolumeConfig.
        socketPath := m.cfg.VMVirtiofsSocketPath(vmID, vol.Tag)

        // Best-effort cleanup of stale socket.
        _ = os.Remove(socketPath)

        pid, err := launchVirtiofsd(ctx, &vol, socketPath, m.cfg.VirtiofsdBinary)
        if err != nil {
            // Cleanup already-started daemons.
            m.killVolumeDaemons(pids)
            return nil, fmt.Errorf("launch virtiofsd for %s: %w", vol.Tag, err)
        }

        // Wait for socket to be ready (same pattern as WaitForSocket).
        if err := waitForSocket(ctx, socketPath, 5*time.Second); err != nil {
            // Kill this daemon and all previously started ones.
            _ = killProcess(pid)
            m.killVolumeDaemons(pids)
            return nil, fmt.Errorf("virtiofsd socket not ready for %s: %w", vol.Tag, err)
        }

        pids[vol.Tag] = pid

        // Write PID file.
        pidPath := m.cfg.VMVirtiofsPIDPath(vmID, vol.Tag)
        _ = utils.WritePIDFile(pidPath, pid)
    }

    return pids, nil
}
```

### 7.5 Shutdown and Cleanup

```go
// stopVolumeDaemons sends SIGTERM to all virtiofsd processes, then SIGKILL
// after a timeout. Removes PID files and sockets.
func (m *manager) stopVolumeDaemons(vmID string, volumes []types.VolumeConfig) {
    for _, vol := range volumes {
        pidPath := m.cfg.VMVirtiofsPIDPath(vmID, vol.Tag)
        pid, err := utils.ReadPIDFile(pidPath)
        if err != nil {
            continue
        }

        // Attempt graceful shutdown.
        _ = signalProcess(pid, syscall.SIGTERM)

        // Wait up to 5 seconds for exit.
        deadline := time.Now().Add(5 * time.Second)
        for time.Now().Before(deadline) {
            if !processAlive(pid) {
                break
            }
            time.Sleep(200 * time.Millisecond)
        }

        // Force kill if still alive.
        if processAlive(pid) {
            _ = killProcess(pid)
        }

        // Derive socket path at runtime — not stored in VolumeConfig.
        socketPath := m.cfg.VMVirtiofsSocketPath(vmID, vol.Tag)

        // Cleanup files.
        _ = os.Remove(pidPath)
        _ = os.Remove(socketPath)
    }
}
```

### 7.6 Reconciliation

The reconciler (`vm/engine/reconcile.go`) gains a new inconsistency type:

```go
const (
    // ... existing types ...
    InconsistencyOrphanedVirtiofsd InconsistencyType = "orphaned_virtiofsd"
)
```

During reconciliation:
- If a VM is STOPPED but virtiofsd processes are still alive, kill them
- If virtiofsd PID files exist but the processes are gone, remove stale files
- If a VM is RUNNING but a virtiofsd process has died, report as a warning

### 7.7 Permission Model

virtiofsd requires read (and optionally write) access to the shared host
directory.

> **Note**: Cocoon requires root. virtiofsd inherits root permissions,
> so host directory access is straightforward.

---

## 8. Security

### 8.1 Host Path Allowlist Policy

By default, Cocoon restricts volume source paths to a configurable allowlist.
This prevents accidental or malicious exposure of sensitive host directories
to guest VMs.

**Default allowlist**:

- `/var/lib/cocoon/shares/` -- Cocoon-managed shared data directory
- `/home/` -- User home directories (for development workflows)
- `/tmp/` -- Temporary files

**Configuration** (`config.json`):

```json
{
    "volume_allowed_paths": [
        "/var/lib/cocoon/shares/",
        "/home/",
        "/tmp/"
    ]
}
```

**Dangerous path protection**: Mounting sensitive host paths requires explicit
opt-in. The following paths are ALWAYS denied unless the user passes
`--allow-dangerous-paths` on the CLI or sets `allow_dangerous_volume_paths: true`
in `config.json`:

- `/` (root filesystem)
- `/etc` (system configuration)
- `/var/run` and `/run` (runtime state, sockets)
- `/proc`, `/sys`, `/dev` (kernel interfaces)
- `/boot` (bootloader and kernel images)
- `/root` (root user home directory)

```go
// ValidateHostPath ensures a host path is safe for sharing.
// It enforces the allowlist and deny-list policies.
func ValidateHostPath(hostPath string, cfg *CocoonConfig, allowDangerous bool) error {
    // Must be absolute.
    if !filepath.IsAbs(hostPath) {
        return fmt.Errorf("host path must be absolute: %s", hostPath)
    }

    // Resolve symlinks and clean the path.
    resolved, err := filepath.EvalSymlinks(hostPath)
    if err != nil {
        return fmt.Errorf("resolve host path %s: %w", hostPath, err)
    }

    // Must exist.
    info, err := os.Stat(resolved)
    if err != nil {
        return fmt.Errorf("stat host path %s: %w", resolved, err)
    }

    // Must be a directory (not a file, not a device).
    if !info.IsDir() {
        return fmt.Errorf("host path must be a directory: %s", resolved)
    }

    // Always-denied paths (cannot be overridden even with --allow-dangerous-paths).
    hardDenied := []string{"/proc", "/sys", "/dev"}
    for _, prefix := range hardDenied {
        if resolved == prefix || strings.HasPrefix(resolved, prefix+"/") {
            return fmt.Errorf("sharing %s is never allowed (kernel interface path)", resolved)
        }
    }

    // Dangerous paths require explicit opt-in.
    dangerousPrefixes := []string{"/", "/etc", "/var/run", "/run", "/boot", "/root"}
    for _, prefix := range dangerousPrefixes {
        if resolved == prefix || (prefix != "/" && strings.HasPrefix(resolved, prefix+"/")) {
            if !allowDangerous {
                return fmt.Errorf(
                    "sharing %s is restricted for security reasons; "+
                        "use --allow-dangerous-paths to override", resolved)
            }
        }
    }

    // Check against configurable allowlist.
    if !allowDangerous {
        allowed := false
        for _, allowedPath := range cfg.VolumeAllowedPaths {
            if strings.HasPrefix(resolved, allowedPath) {
                allowed = true
                break
            }
        }
        if !allowed {
            return fmt.Errorf(
                "host path %s is not in the volume allowlist %v; "+
                    "add the path to volume_allowed_paths in config.json "+
                    "or use --allow-dangerous-paths", resolved, cfg.VolumeAllowedPaths)
        }
    }

    return nil
}
```

**CLI flag**:

```go
&cli.BoolFlag{
    Name:  "allow-dangerous-paths",
    Usage: "allow mounting sensitive host paths (/, /etc, /var/run, etc.) as volumes",
},
```

### 8.2 Read-Only vs Read-Write Policy

**Default: read-only**. Volumes are mounted read-only unless explicitly
specified as `rw`. This follows the principle of least privilege -- most
volume use cases (config injection, model weights, static assets) require
only read access.

```bash
# Read-only (default behavior when no option specified)
cocoon run -v /data/models:/mnt/models myimage
cocoon run -v /data/models:/mnt/models:ro myimage   # Explicit read-only

# Read-write (requires explicit :rw option)
cocoon run -v /var/log/output:/mnt/output:rw myimage
```

Read-only is enforced at multiple layers (defense in depth):

1. **virtiofsd**: Launched with `--sandbox=chroot`. For read-only volumes,
   virtiofsd is additionally started with the shared directory bind-mounted
   read-only, so even if the guest bypasses virtio-fs semantics, the host
   filesystem layer rejects writes.
2. **Cloud Hypervisor**: The `fs` config does not have a native read-only
   flag, but the virtiofsd backend enforces it.
3. **Guest mount**: The guest auto-mount convention includes `ro` in the
   mount options when the volume tag encodes read-only status.
4. **Defense in depth**: Even if the guest attempts `mount -o remount,rw`,
   the virtiofsd process rejects write operations at the FUSE layer.

**Use cases by access mode**:

| Access Mode | Use Cases | Risk Level |
|-------------|-----------|------------|
| Read-only (`ro`, default) | Config injection, model weights, static assets, shared libraries, reference data | Low -- guest cannot modify host files |
| Read-write (`rw`) | Log directories, build output, data processing pipelines, development source code | Medium -- guest can create, modify, and delete files on host |

**Recommendation**: Use read-only mounts for all volumes where the guest does
not need to write data back to the host. For untrusted workloads, always use
read-only mounts combined with `--sandbox chroot` (the default).

### 8.3 Permission Mapping (UID/GID)

#### Default Behavior: No Mapping (Pass-Through)

By default, virtiofsd does not remap UIDs or GIDs. Host UIDs pass through
to the guest unmodified. This means:

- **Guest root (UID 0) has root-equivalent access** to shared files on the
  host. If the virtiofsd process runs as root (which it does in Cocoon,
  since Cocoon requires root), guest root can read and write any file in the
  shared directory that host root can access.
- Files created by guest UID 1000 appear as UID 1000 on the host.
- Files created by guest root (UID 0) appear as root-owned on the host.

**Security implication**: Without UID mapping, a compromised guest with root
access can modify or delete any file in a read-write shared directory. This is
acceptable for single-user development workflows but is a security concern for
multi-tenant deployments.

#### Mitigation Strategies

1. **Read-only mounts** (primary defense): Use `:ro` for all volumes where
   the guest does not need write access. Even guest root cannot modify
   read-only shared directories.

2. **virtiofsd sandbox** (`--sandbox=chroot`, default): The chroot sandbox
   prevents virtiofsd from accessing any host path outside the shared directory
   tree. Guest root can only affect files within the shared mount, not arbitrary
   host paths.

3. **UID/GID mapping** (Phase 2.5, optional): For multi-tenant scenarios,
   explicit UID/GID mapping remaps guest UIDs to unprivileged host UIDs:

```bash
# Future: explicit mapping via CLI flags (Phase 2.5)
cocoon run --volume /data:/mnt/data:rw \
           --volume-uid-map 0:100000 \
           --volume-gid-map 0:100000 \
           myimage
```

With `--uid-map 0:100000`, guest UID 0 maps to host UID 100000 (unprivileged),
preventing privilege escalation from guest to host. virtiofsd supports
`--uid-map` and `--gid-map` flags for this purpose.

4. **Namespace sandbox** (`--sandbox=namespace`): Uses user and mount namespaces
   for stronger isolation than chroot. This is available but not the default
   because it adds complexity and may conflict with some host configurations.

#### Recommendation by Deployment Model

| Deployment | UID Mapping | Recommended Mount Mode | Notes |
|------------|-------------|----------------------|-------|
| Single-user development | None (default) | `:rw` for source code, `:ro` for everything else | Simplest setup, acceptable trust level |
| CI/CD pipelines | None (default) | `:rw` for build output, `:ro` for inputs | VMs are ephemeral, blast radius is limited |
| Multi-tenant / untrusted | `--uid-map 0:100000` (Phase 2.5) | `:ro` wherever possible | Prevents guest root from writing as host root |
| Production serving | None (default) | `:ro` only | Never mount read-write into production VMs |

### 8.4 Sandboxing virtiofsd

virtiofsd supports multiple sandboxing modes:

- `--sandbox=chroot`: chroots into the shared directory (default, recommended).
  The virtiofsd process cannot access any host path outside the shared directory.
  This is the primary defense against directory traversal from the guest.
- `--sandbox=namespace`: Uses user/mount namespaces for stronger isolation.
  Provides additional protection via kernel namespace boundaries but may
  conflict with some host security configurations (e.g., user namespace
  restrictions).

Cocoon uses `--sandbox=chroot` by default. Since Cocoon requires root, both
sandboxing modes are available.

**virtiofsd launch with sandbox**:

```go
args := []string{
    "--socket-path", socketPath,
    "--shared-dir", vol.HostPath,
    "--cache=auto",
    "--sandbox=chroot",     // Always enabled by default
}
```

### 8.5 Denial of Service Prevention

- **Max volumes per VM**: Configurable limit (default: 16) to prevent
  excessive virtiofsd process spawning.
- **Socket path length**: Unix socket paths are limited to 108 characters;
  tag derivation ensures paths stay within this limit.
- **virtiofsd resource limits**: Future enhancement to set cgroup limits on
  virtiofsd processes.

### 8.6 Normative Security Rules

> **Security Policy for Volume Passthrough**
>
> The following normative rules govern volume passthrough behavior. Rules use
> RFC 2119 language: MUST (required), SHOULD (recommended), MAY (optional).
>
> **MUST**:
>
> 1. virtiofsd MUST be launched with `--sandbox=chroot` (or `--sandbox=namespace`)
>    enabled. Disabling the sandbox (`--sandbox=none`) is not permitted by Cocoon.
>
> 2. Host paths MUST be validated against the allowlist before any volume mount
>    is created. Paths outside the allowlist are rejected unless
>    `--allow-dangerous-paths` is explicitly provided.
>
> 3. Volumes MUST default to read-only (`:ro`). Read-write access requires
>    explicit `:rw` in the volume specification.
>
> 4. Host paths MUST be resolved (symlinks evaluated, path cleaned) before
>    validation. This prevents symlink-based escapes where a symlink inside
>    an allowed path points to a disallowed path.
>
> 5. `/proc`, `/sys`, and `/dev` MUST never be shared as volumes, even with
>    `--allow-dangerous-paths`. These kernel interface paths present
>    unacceptable host compromise risk.
>
> **SHOULD**:
>
> 6. UID/GID mapping SHOULD be documented and recommended for multi-tenant
>    deployments where guest VMs are not fully trusted. The default
>    no-mapping behavior SHOULD be clearly communicated as "guest root
>    equals host root on shared files."
>
> 7. `cocoon doctor` SHOULD check for virtiofsd availability and version
>    compatibility. The check SHOULD warn if virtiofsd is not found or is
>    older than v1.7.0 (the minimum supported Rust virtiofsd version).
>
> 8. `cocoon doctor` SHOULD verify that `/dev/fuse` is available on the host,
>    as virtiofsd requires FUSE support.
>
> 9. Volume-related errors SHOULD include actionable remediation guidance
>    (e.g., "add path to volume_allowed_paths" or "use --allow-dangerous-paths").
>
> **MAY**:
>
> 10. Cocoon MAY support `--sandbox=namespace` as an alternative to the default
>     `--sandbox=chroot` for environments that require stronger isolation.
>
> 11. Cocoon MAY log a warning when a read-write volume is mounted for
>     informational purposes, to encourage read-only usage where possible.

---

## 9. Interface Changes

### 9.1 hypervisor.Client

No changes to the `Client` interface itself. The `CreateVM` method already
accepts a `*CHVMConfig`, and the new `Fs` field flows through the existing
JSON serialization path.

### 9.2 vm.Manager

No new methods on the `Manager` interface. Volume configuration flows through
the existing `Create` and `Start` paths via the expanded `CreateOptions` and
`VMConfig` types.

### 9.3 New Package: `volume`

A new `volume` package encapsulates volume-specific logic:

```go
// Package volume manages virtio-fs volume sharing between host and guest.
package volume

// Manager handles virtiofsd process lifecycle for shared volumes.
type Manager interface {
    // StartDaemons launches virtiofsd processes for all volumes.
    // Returns a map of tag -> PID. Must be called before vm.create.
    StartDaemons(ctx context.Context, vmID string, volumes []types.VolumeConfig) (map[string]int, error)

    // StopDaemons gracefully stops all virtiofsd processes for a VM.
    // Called after the CH process has exited.
    StopDaemons(vmID string, volumes []types.VolumeConfig)

    // KillDaemons force-kills all virtiofsd processes for a VM.
    KillDaemons(vmID string, volumes []types.VolumeConfig)

    // IsAlive checks if the virtiofsd for a specific volume is still running.
    IsAlive(vmID, tag string) bool

    // Validate checks that all volume specs are valid (paths exist, no conflicts).
    Validate(volumes []VolumeMount) error
}
```

The `volume.Manager` is injected into `vm/engine.manager` alongside the
existing dependencies:

```go
type manager struct {
    cfg        *config.CocoonConfig
    hyper      hypervisor.Client
    refCounter storage.ReferenceCounter
    cowMgr     storage.COWManager
    imgMgr     image.Manager
    volMgr     volume.Manager // NEW
}
```

### 9.4 Doctor Command

`cocoon doctor` is extended to check for virtiofsd availability and related
prerequisites:

```go
// Volume-related doctor checks (added to existing doctor command):
func volumeDoctorChecks() []DoctorCheck {
    return []DoctorCheck{
        {
            Name: "virtiofsd-binary",
            Check: func() error {
                path, err := exec.LookPath("virtiofsd")
                if err != nil {
                    return fmt.Errorf(
                        "'virtiofsd' not found in PATH (needed for volume passthrough): %w\n"+
                        "  Install: see https://gitlab.com/virtio-fs/virtiofsd/-/releases", err)
                }
                // Check version: virtiofsd --version -> "virtiofsd X.Y.Z"
                out, err := exec.Command(path, "--version").CombinedOutput()
                if err != nil {
                    return fmt.Errorf("cannot determine virtiofsd version: %w", err)
                }
                // Parse and verify >= 1.7.0
                if !isVersionAtLeast(string(out), "1.7.0") {
                    return fmt.Errorf(
                        "virtiofsd version too old (need >= 1.7.0, got %s); "+
                        "the older C-based QEMU virtiofsd is not supported", strings.TrimSpace(string(out)))
                }
                return nil
            },
            Severity: "warning", // Volume support is optional.
        },
        {
            Name: "fuse-device",
            Check: func() error {
                if _, err := os.Stat("/dev/fuse"); err != nil {
                    return fmt.Errorf(
                        "/dev/fuse not available (required by virtiofsd): %w\n"+
                        "  Load kernel module: modprobe fuse", err)
                }
                return nil
            },
            Severity: "warning",
        },
    }
}
```

**Doctor check summary for volumes**:

| Doctor Check | What It Verifies | Severity |
|-------------|------------------|----------|
| `virtiofsd-binary` | virtiofsd exists in PATH and version >= 1.7.0 (Rust virtiofsd) | warning |
| `fuse-device` | `/dev/fuse` device exists (kernel FUSE support) | warning |

---

## 10. Compatibility

### 10.1 UEFI and Direct Kernel Boot

Volume passthrough via virtio-fs works with both UEFI and direct kernel boot modes.
The virtio-fs device is presented through the virtio transport layer, which
is independent of the firmware or boot method used. Both UEFI boot (cloud images)
and direct kernel boot (OCI VM images) work with virtio-fs.

### 10.2 Guest Kernel Requirements

The guest kernel must include the `virtiofs` filesystem module. This is
available in:

- Linux 5.4+ (mainline)
- Ubuntu 20.04+ (all LTS cloud images)
- Fedora 31+
- Debian 11+

All cloud images recommended by Cocoon include virtio-fs support.

### 10.3 Backward Compatibility

- VMs created without volumes (Phase 1) continue to work unchanged
- `config.json` schema version remains `1` for VMs without volumes
- The `Volumes` field is `omitempty`, so existing config files parse correctly
- The `Fs` field in `CHVMConfig` is `omitempty`, so VM creation without
  volumes sends the same payload as before
- `CHMemoryConfig.Shared` defaults to `false`, preserving Phase 1 behavior

### 10.4 virtiofsd Versions

Cocoon targets virtiofsd v1.7.0+ (the Rust-based virtiofsd from the
`virtiofsd` crate). The older C-based QEMU virtiofsd is not supported due to
maintenance status and security concerns.

---

## 11. Implementation Plan

### Phase 2.1: Basic virtio-fs (Read-Write)

**Scope**: End-to-end volume sharing with manual guest mounting.

1. Add `VolumeConfig` type and `Volumes` field to `VMConfig`
2. Add `VolumeMount` type and `Volumes` field to `CreateOptions`
3. Add `--volume` / `-v` CLI flag to `create` and `run` commands
4. Parse and validate volume specs
5. Add `CHFsConfig` to hypervisor types
6. Add `Fs` and `Memory.Shared` to `CHVMConfig`
7. Implement `volume.Manager` with `StartDaemons` / `StopDaemons`
8. Integrate virtiofsd spawn into `Start` flow (before `vm.create`)
9. Integrate virtiofsd cleanup into `Stop` and `Delete` flows
10. Add PID tracking for virtiofsd processes
11. Extend `cocoon doctor` to check for virtiofsd
12. Write integration tests with a real VM

**Estimated effort**: 2-3 weeks

### Phase 2.2: Read-Only Enforcement and Validation

**Scope**: Hardened security for read-only volumes.

1. Enforce read-only at virtiofsd level
2. Add path validation and deny-list for sensitive paths
3. Add max-volumes-per-VM configuration
4. Socket path length validation
5. Integration tests for read-only enforcement
6. Add `InspectVolumeInfo` to inspect output

**Estimated effort**: 1 week

### Phase 2.3: Guest Auto-Mount via Tag Convention

**Scope**: Automatic guest-side mounting without manual intervention.

1. Document the tag-to-path convention (`mnt-data` -> `/mnt/data`)
2. Provide a reference udev rule and/or systemd generator script for guest images
3. Test auto-mount with Ubuntu, Fedora, and Debian guest images
4. Document manual mount instructions for users who prefer explicit control

**Estimated effort**: 1 week

### Phase 2.4: Hot-Add Volumes (Future)

**Scope**: Add or remove volumes while a VM is running.

1. Implement `cocoon volume add <vm> /host:/guest[:ro]`
2. Implement `cocoon volume remove <vm> /guest`
3. Use CH hot-plug API (`PUT /api/v1/vm.add-fs`)
4. Spawn/kill virtiofsd dynamically
5. Coordinate guest-side mount/unmount

**Estimated effort**: 2-3 weeks (depends on CH hot-plug API maturity)

### Phase 2.5: UID/GID Mapping and Advanced Options

**Scope**: Explicit control over ownership and permissions.

1. Add `--volume-uid-map` and `--volume-gid-map` CLI flags
2. Pass mapping to virtiofsd via `--uid-map` / `--gid-map`
3. Support `namespace` sandbox mode
4. Document security best practices for multi-tenant volume sharing

**Estimated effort**: 1 week

---

## 12. Unresolved Questions

### Triage Summary

| # | Question | Phase | Decision / Status |
|---|----------|-------|-------------------|
| Q1 | DAX support | Phase 2.1 (defer) | Defer; disable by default. Revisit when hugepages config is streamlined. |
| Q2 | File-level sharing | Phase 2.1 (defer) | Defer; directories only for now. |
| Q3 | Max volume count | **Phase 2.0** (must decide) | Default 16, configurable via `config.json`. |
| Q4 | virtiofsd distribution | **Phase 2.0** (must decide) | Require user install; `cocoon doctor` checks availability and version. |
| Q5 | Auto-mount mechanism | Phase 2.1 (defer) | Defer; manual guest config initially. Provide reference udev rule in docs. |
| Q6 | Concurrent volume access | **Phase 2.0** (must decide) | Warn on multi-VM RW sharing of the same path; allow with `--allow-shared-rw` flag. |
| Q7 | Windows guest support | Phase 2.1 (defer) | Defer; Linux guests only. Document as unsupported. |
| Q8 | Performance tuning | Phase 2.1 (defer) | Defer; use virtiofsd defaults (1 queue, 1024 entries). |

---

1. **DAX (Direct Access) support**: Should DAX be enabled by default for
   read-only volumes? DAX provides memory-mapped access for better read
   performance but requires `hugepages` memory configuration. This adds
   host configuration requirements.

2. **File-level sharing**: Should `--volume` support sharing a single file
   (not just directories)? virtiofsd operates on directories. Single-file
   sharing would require a different mechanism (e.g., block device, or a
   wrapper directory).

3. **Max volume count**: What is a reasonable default limit for volumes per
   VM? Each volume requires one virtiofsd process and one Unix socket.
   Proposed default: 16.

4. **virtiofsd distribution**: Should Cocoon bundle virtiofsd, or require
   the user to install it separately? The Rust virtiofsd is a standalone
   binary, so bundling is feasible. The `cocoon doctor --fix` command could
   download it, similar to firmware management.

5. **Auto-mount mechanism**: Should Cocoon provide a reference guest image
   with the virtiofs auto-mount generator pre-installed, or leave it entirely
   to the user? A reference udev rule or systemd generator simplifies the
   out-of-box experience, but some users may prefer explicit mount control.

6. **Concurrent volume access**: What happens when multiple VMs share the
   same host directory in read-write mode? virtiofsd supports this, but
   POSIX locking semantics may not propagate correctly across VMs. Should
   Cocoon warn or prevent multi-VM RW sharing of the same path?

7. **Windows guest support**: virtio-fs drivers exist for Windows (WinFsp +
   VirtIO drivers) but are less mature. Should the design accommodate
   non-Linux guests, or treat them as unsupported?

8. **Performance tuning**: Should Cocoon expose `num_queues` and `queue_size`
   as advanced CLI flags? The defaults (1 queue, 1024 entries) are suitable
   for most workloads, but high-throughput scenarios may benefit from
   multiple queues.

---

## 13. Cross-Feature Alignment

### Combined CHVMConfig Target

When all Phase 2 features are implemented, the unified `CHVMConfig` in `hypervisor/types.go` will be:

    type CHVMConfig struct {
        CPUs    CHCPUConfig      `json:"cpus"`
        Memory  CHMemoryConfig   `json:"memory"`
        Disks   []CHDiskConfig   `json:"disks,omitempty"`
        Net     []CHNetConfig    `json:"net,omitempty"`      // Networking (tap/macvtap)
        Fs      []CHFsConfig     `json:"fs,omitempty"`       // Volume passthrough (virtio-fs)
        Serial  CHSerialConfig   `json:"serial"`
        Console CHConsoleConfig  `json:"console"`             // Console: mode changes to "Pty"
        Devices []CHDeviceConfig `json:"devices,omitempty"`   // Device passthrough (VFIO)
    }

Each Phase 2 feature adds its fields independently. All additions use `omitempty` for backward compatibility with Phase 1 VMs.

### Feature Interaction Notes

- **Checkpoint**: VMs with volumes can be checkpointed, but host paths must exist at restore time and virtiofsd processes must be restarted. Volume configuration is stored in `config.json` and included in checkpoint metadata.
- **Device passthrough coexistence**: Volumes and device passthrough can coexist on the same VM. The VM delete flow must handle both virtiofsd cleanup and device driver restoration.
- **Console coexistence**: The `buildCHVMConfig` examples in this document show `Console.Mode = "Off"` for illustration. When the console feature is also enabled, this will be `Console.Mode = "Pty"`. See [12-console.md](./12-console.md) for details.

---

## 14. References

- [Cloud Hypervisor virtio-fs documentation](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/fs.md)
- [virtiofsd (Rust)](https://gitlab.com/virtio-fs/virtiofsd) -- the recommended virtiofsd implementation
- [virtio-fs specification](https://virtio-fs.gitlab.io/)
- [Cloud Hypervisor REST API](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/vmm/src/api/openapi/)
- [03-hypervisor-integration.md](./03-hypervisor-integration.md) -- Cocoon's CH integration
- [05-storage-management.md](./05-storage-management.md) -- Storage architecture and COW overlays
- [07-vm-lifecycle.md](./07-vm-lifecycle.md) -- VM lifecycle state machine
- [09-cli-design.md](./09-cli-design.md) -- CLI design patterns
- [16-networking.md](./16-networking.md) -- CNI networking (prerequisite for Option D)

---

**Note**: This document is a Phase 2 design. Implementation details will be
refined based on Phase 1 experience, Cloud Hypervisor API evolution, and
production requirements. The core virtio-fs approach is well-proven in
production VMMs (QEMU, Firecracker, Cloud Hypervisor) and the risk is
primarily in lifecycle management complexity, not in the sharing mechanism
itself.

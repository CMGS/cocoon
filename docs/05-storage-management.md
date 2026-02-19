# Storage Management

**Version**: 1.0
**Status**: Implemented
**Phase**: Phase 1
**Last Updated**: 2026-02-18

## Overview

This document describes the storage management strategy for the Cocoon AI Agent Sandbox, focusing on directory layout, copy-on-write (COW) optimization, reference counting, and garbage collection. The design enables efficient disk space usage while supporting high-concurrency VM operations.

## Storage Layout

### Canonical Filesystem Layout (Normative)

**This section is the single source of truth for Cocoon filesystem paths.**
Other documents may use default paths (e.g., `/var/lib/cocoon/...`) as illustrative examples but MUST NOT introduce new directories or filenames not listed here. All paths are derived from configurable roots (`RootDir`, `RuntimeDir`, `LogDir`); defaults shown below assume the standard `/var/lib/cocoon`, `/run/cocoon`, `/var/log/cocoon`.
Lock file paths are additionally documented in [06-concurrency.md](./06-concurrency.md) § Lock File Locations, which is the SSOT for the lock hierarchy and lock-specific semantics.

```
/var/lib/cocoon/                          # Persistent root (survives reboot)
├── db/                                   # Database files (reference counts, indexes)
│   ├── references.json                   # Base image reference counts
│   ├── references.lock                   # flock for reference counter
│   ├── gc.lock                           # Global GC lock
│   ├── name-index.json                   # name → vm_id mapping (derived, can be rebuilt)
│   ├── name-index.lock                   # flock for name-index updates
│   ├── oci-build-tags.json               # OCI build tag → layout path mapping (cocoon image build)
│   ├── oci-build-txn.lock                # flock for OCI cross-index txn serialization
│   ├── oci-build-tags.lock               # flock for OCI build tag index updates
│   ├── oci-layer-refs.json               # Blob digest → [manifest digests] reference tracking (GC)
│   └── oci-layer-refs.lock               # flock for oci-layer-refs.json updates
├── cache/
│   ├── images/                           # Base qcow2 images (content-addressed)
│   │   ├── {checksum_16}_{arch}.qcow2    # e.g., a1b2c3d4e5f6a7b8_amd64.qcow2
│   │   └── ...
│   ├── manifests/                        # OCI manifest cache
│   │   ├── index.json                    # IMAGE_REF -> base_key mapping index
│   │   └── index.lock                    # flock for manifest index updates
│   ├── locks/
│   │   └── {checksum_16}_{arch}.lock     # Per-image conversion lock
│   └── oci/                              # OCI VM image build cache (shared blob store)
│       ├── blobs/sha256/{hex}            # Shared content-addressed blob store
│       └── layouts/{sha256-hex}/             # OCI layouts (keyed by full tag+manifest hash, blobs are hardlinks)
│           ├── oci-layout
│           ├── index.json
│           └── blobs/sha256/{hex}        # Hardlinks to ../../blobs/sha256/{hex}
├── buildah/                              # Buildah storage root
├── vms/
│   ├── {vm-id}/                          # e.g., vm-01HXYZ.../
│   │   ├── config.json                   # Immutable VM configuration
│   │   ├── metadata.json                 # Mutable runtime state
│   │   ├── metadata.lock                 # flock for metadata writes
│   │   ├── overlay.qcow2                 # COW overlay (ALWAYS this name)
│   │   └── tpm/                          # swtpm TPM state directory (if TPM enabled)
│   └── ...
├── firmware/                             # Boot firmware binaries
│   └── CLOUDHV.fd                        # UEFI firmware (OVMF for Cloud Hypervisor)
└── temp/                                 # Scratch space for conversions

/run/cocoon/                              # Runtime/ephemeral (tmpfs, cleared on reboot)
└── vms/
    └── {vm-id}/
        ├── api.sock                      # Cloud Hypervisor API socket
        ├── ch.pid                        # CH process PID file
        ├── swtpm.sock                    # swtpm TPM socket (if TPM enabled)
        └── swtpm.pid                     # swtpm PID file (if TPM enabled)

/var/log/cocoon/                          # Logs (persistent)
├── {vm_id}-serial.log                    # Serial console per VM (e.g., vm-01HXYZ...-serial.log)
├── {vm_id}-ch.log                        # Cloud Hypervisor log per VM
├── {vm_id}-swtpm.log                     # swtpm log per VM (if TPM enabled)
└── cocoon.log                            # Main cocoon log (optional)
```

**Phase 2 Additions (Planned)**:

> **SSOT Note**: This section is the Single Source of Truth (SSOT) for all Phase 2
> storage and runtime paths. Individual Phase 2 design documents should reference
> this section rather than maintaining independent path listings.

```
Phase 2 Planned Paths:
│
├── Persistent Storage (/var/lib/cocoon/)
│   │
│   ├── Warm-Start Checkpoints (docs/15-warm-start.md)
│   │   └── checkpoints/
│   │       ├── checkpoint-index.json           # name -> ckpt-id mapping
│   │       ├── checkpoint-index.lock           # flock for index updates
│   │       └── {ckpt-id}/                      # e.g., ckpt-01HXYZ.../
│   │           ├── checkpoint.json             # Checkpoint metadata (checkpoint_type: "qcow2" | "overlayfs")
│   │           ├── ch-snapshot/                # CH memory + device state
│   │           │   ├── config                  # CH VM config snapshot
│   │           │   └── state                   # Memory + device + vCPU state
│   │           ├── overlay.qcow2               # Disk state (qcow2 path only)
│   │           └── upper/                      # Preserved overlayfs upperdir (overlayfs path only)
│   │
│   ├── OCI VM Image Build Cache (docs/04.1-oci-vm-images.md) — Implemented
│   │   └── cache/oci/
│   │       ├── blobs/sha256/{hex}              # Shared content-addressed blob store
│   │       └── layouts/{sha256-hex}/               # OCI layouts (keyed by full tag+manifest hash)
│   │           ├── oci-layout
│   │           ├── index.json
│   │           └── blobs/sha256/{hex}          # Hardlinks to shared blob store
│   │
│   ├── OCI VM Image Pull Cache (docs/04.1-oci-vm-images.md) — Phase 2 Planned
│   │   └── cache/oci/
│   │       └── {manifest-digest}/              # Per-image extracted artifacts
│   │           ├── manifest.json               # OCI manifest
│   │           ├── config.json                 # OCI VM config blob (kernel cmdline, etc.)
│   │           ├── vmlinuz                     # Extracted kernel (payload.kernel)
│   │           ├── initrd.img                  # Extracted initrd (payload.initramfs)
│   │           ├── rootfs/                     # Rootfs layer (extracted dir tree, read-only)
│   │           ├── custom-1/                   # First customization layer (read-only, if present)
│   │           └── custom-N/                   # Nth customization layer (read-only, if present)
│   │   └── db/
│   │       ├── oci-references.json             # manifest-digest -> [vm-id] ref tracking
│   │       └── oci-references.lock             # flock for oci-references.json updates
│   │
│   ├── Per-VM OCI Rootfs State (docs/04.1-oci-vm-images.md)
│   │   └── vms/{vm-id}/
│   │       ├── upper/                          # OverlayFS upperdir (per-VM COW writes)
│   │       ├── work/                           # OverlayFS workdir (kernel internal)
│   │       └── merged/                         # OverlayFS mount point (virtiofsd serves this)
│   │
│   ├── dnsmasq DHCP State (docs/16-networking.md)
│   │   └── dnsmasq/{bridge}/                   # Per-bridge state (e.g., dnsmasq/cni0/)
│   │       ├── dnsmasq.conf                    # Generated dnsmasq configuration
│   │       ├── dnsmasq.pid                     # PID file for running dnsmasq process
│   │       └── hosts                           # Static DHCP leases (<mac>,<ip>,<hostname>)
│   │
│   └── Volume Passthrough Shared Directory (docs/17-volume-passthrough.md)
│       └── shares/                             # Default allowed host path for volume mounts
│
├── Runtime State (/run/cocoon/ — configurable via RuntimeDir)
│   │
│   ├── OCI Rootfs Runtime (docs/04.1-oci-vm-images.md)
│   │   └── vms/{vm-id}/
│   │       └── virtiofsd.sock              # virtiofsd socket for rootfs-serving instance
│   │
│   ├── CNI Networking (docs/16-networking.md)
│   │   └── vms/{vm-id}/
│   │       ├── netns                           # Network namespace bind mount for CNI
│   │       └── network.lock                    # Per-VM network lock (L5, docs/06)
│   │
│   ├── dnsmasq Runtime (docs/16-networking.md)
│   │   └── dnsmasq/
│   │       └── dnsmasq.lock                    # Global dnsmasq lock (L6, docs/06)
│   │
│   └── Volume Passthrough Daemons (docs/17-volume-passthrough.md)
│       └── vms/{vm-id}/
│           ├── virtiofs-{tag}.sock             # virtiofsd socket per shared volume
│           └── virtiofs-{tag}.pid              # virtiofsd PID file per shared volume
│
├── Console (docs/12-console.md)
│   │   No new on-disk paths. The PTY device path (/dev/pts/N) is dynamically
│   │   discovered from the CH REST API (GET /api/v1/vm.info) at attach time.
│   │   Console mode is configured via --console pty in the CH VM config.
│   │
│   └── (Future: /run/cocoon/vms/{vm-id}/console.lock — advisory lock to
│         prevent concurrent attach; deferred beyond Phase 2 v1.0)
│
├── Pause/Resume (docs/13-pause-resume.md) — Phase 2, not yet implemented
│   │   No new on-disk paths planned. Pause state will be tracked in the
│   │   existing metadata.json via a paused_at field and state=PAUSED.
│   │   CH state queried via GET /api/v1/vm.info for reconciliation.
│   │
│   └── (No additional files)
│
└── External / System Paths (not managed by Cocoon)
    ├── /etc/cni/net.d/*.conflist              # CNI network configs (user-provided, docs/16)
    ├── /opt/cni/bin/                          # CNI plugin binaries (docs/16)
    └── /var/lib/cni/networks/{network}/{ip}   # host-local IPAM allocations (CNI-managed, docs/16)
```

Path notes:
- **dnsmasq state** lives under `/var/lib/cocoon/dnsmasq/` (persistent) rather than
  `/run/cocoon/` because DHCP lease files must survive dnsmasq restarts (SIGHUP
  re-reads hosts file). The PID file is co-located for simplicity.
- **virtiofsd sockets** (both user volume sockets `virtiofs-{tag}.sock` and the
  rootfs-serving socket `virtiofsd.sock`) are under `/run/cocoon/` (ephemeral),
  consistent with other runtime sockets (API socket, swtpm socket).
- **Checkpoint directories** use their own ID namespace (`ckpt-{ulid}`), separate from
  VM IDs (`vm-{ulid}`). The `ch-snapshot/` subdirectory name matches the
  `destination_url` parameter in Cloud Hypervisor's `PUT /api/v1/vm.snapshot` API.
- **OCI build cache** uses a shared blob store (`cache/oci/blobs/sha256/`) with
  hardlink-based deduplication across OCI layouts (`cache/oci/layouts/`).
  Blob reference counts are tracked in `db/oci-layer-refs.json`.
- **OCI pull cache** (Phase 2 planned) directories are keyed by manifest digest
  (content-addressable) and shared across all VMs created from the same OCI image.
- **Network namespace** files under `/run/cocoon/` are lost on reboot; the Start()
  flow transparently recreates them when needed (see docs/16 Section 2.4).

**Key rules**:
- Overlay is ALWAYS `overlay.qcow2` inside the VM directory (not `{vm_id}.qcow2`)
- Base images are ALWAYS `{checksum}_{arch}.qcow2` (content-addressed)
- `references.json` key is ALWAYS `{checksum}_{arch}` (NOT absolute path)
- Runtime sockets and PID files live under `/run/cocoon/` (ephemeral, cleared on reboot)
- Logs are separate under `/var/log/cocoon/` (persistent)

### JSON Schema Registry (Normative)

**This section is the single source of truth for all JSON file schemas.**
Other documents MUST reference these definitions rather than re-defining fields.

#### config.json (Immutable — set at VM creation, never modified)

| Field | Type | Example | Description |
|-------|------|---------|-------------|
| `vm_id` | string | `"vm-01HXYZ..."` | Internal primary key (`vm-{ulid}`), never reused |
| `name` | string | `"myvm"` | User-facing alias, globally unique, immutable |
| `image_ref` | string | `"myorg/ubuntu:22.04"` | Original image reference (path/URL/OCI ref) |
| `base_key` | string | `"a1b2c3d4e5f6a7b8_amd64"` | Content-addressed key: `{checksum_16}_{arch}` |
| `base_digest_full` | string | `"a1b2c3d4e5f6a7b8..."` (64 hex) | Full SHA-256 for collision audit |
| `arch` | string | `"amd64"` | Architecture |
| `image_type` | string | `""` | `"qcow2"` (default when omitted) or `"oci-vm"` (OCI VM direct boot) |
| `boot_strategy` | string | `"uefi"` | `"uefi"` (cloud images) or `"direct"` (OCI VM images) |
| `firmware_path` | string | `"/var/lib/cocoon/firmware/CLOUDHV.fd"` | Firmware path (CLOUDHV.fd for UEFI; empty for direct kernel boot) |
| `kernel_path` | string | `""` | Kernel path for direct boot (Phase 2, omitted when empty) |
| `initramfs_path` | string | `""` | Initramfs path for direct boot (Phase 2, omitted when empty) |
| `cmdline` | string | `""` | Kernel command line for direct boot (Phase 2, omitted when empty) |
| `virtiofs_tag` | string | `""` | virtiofs filesystem tag for OCI VM rootfs (e.g., `"cocoon-rootfs"`; Phase 2, omitted when empty) |
| `virtiofs_sock` | string | `""` | Rootfs-serving virtiofsd socket path for OCI VM (Phase 2, omitted when empty) |
| `tpm_socket_path` | string | `"/run/cocoon/vms/{vm_id}/swtpm.sock"` | swtpm TPM socket path (omitted if TPM not configured) |
| `cpus` | int | `2` | vCPU count |
| `memory_mb` | int64 | `2048` | Memory in MiB |
| `disk_size` | string | `"10G"` | Overlay disk size (human-readable) |
| `base_image_path` | string | `"/var/lib/cocoon/cache/images/..."` | Derived from `base_key` |
| `overlay_path` | string | `"/var/lib/cocoon/vms/{vm_id}/overlay.qcow2"` | COW overlay path |
| `serial_log` | string | `"/var/log/cocoon/{vm_id}-serial.log"` | Serial console log |
| `socket_path` | string | `"/run/cocoon/vms/{vm_id}/api.sock"` | CH API socket |
| `created_at` | string | `"2026-02-12T10:00:00Z"` | RFC 3339 creation timestamp |
| `schema_version` | int | `1` | Schema version for migration |

#### metadata.json (Mutable — updated at runtime)

| Field | Type | Example | Description |
|-------|------|---------|-------------|
| `vm_id` | string | `"vm-01HXYZ..."` | Matches config.json (for crash recovery) |
| `state` | string | `"RUNNING"` | Current VM state |
| `previous_state` | string | `"STARTING"` | State before last transition |
| `process_pid` | int | `12345` | CH process PID (0 if not running) |
| `boot_time` | string | `"2.3s"` | Duration string |
| `last_boot_mode` | string | `"uefi"` | Actual boot mode used (`"uefi"` / `"direct"`) |
| `last_firmware_path` | string | `"/var/lib/cocoon/firmware/CLOUDHV.fd"` | Actual firmware used (empty for direct kernel boot) |
| `last_error` | string | `""` | Last error message (empty if none) |
| `last_error_type` | string | `""` | Error classification (omitted if none) |
| `last_error_at` | string | `""` | RFC 3339 timestamp of last error (omitted if none) |
| `error_count` | int | `0` | Consecutive error count |
| `auto_remove` | bool | `false` | If true, VM is auto-deleted on stop (omitted if false) |
| `updated_at` | string | `"2026-02-12T10:01:30Z"` | Last metadata write |
| `started_at` | string | `"2026-02-12T10:01:00Z"` | Last start timestamp |
| `stopped_at` | string | `""` | Omitted when VM has not been stopped; RFC 3339 timestamp when set |
| `schema_version` | int | `1` | Schema version for migration |

#### references.json (Global — tracks base image usage)

| Field | Type | Example | Description |
|-------|------|---------|-------------|
| *key* | string | `"a1b2c3d4e5f6a7b8_amd64"` | Content-addressed: `{checksum_16}_{arch}` |
| `.path` | string | `"/var/lib/cocoon/cache/images/..."` | Cached qcow2 path |
| `.digest_full` | string | `"a1b2c3d4e5f6a7b8..."` (64 hex) | Full SHA-256 for collision detection |
| `.source_ref` | string | `"myorg/ubuntu:22.04"` | Original image ref (audit) |
| `.refs` | []string | `["vm-001", "vm-002"]` | VM IDs using this base |
| `.created_at` | string | `"2026-02-12T10:00:00Z"` | First cache timestamp |

#### name-index.json (Global — derived, can be rebuilt)

| Field | Type | Example | Description |
|-------|------|---------|-------------|
| *key* | string | `"myvm"` | User-assigned VM name |
| *value* | string | `"vm-01HXYZ..."` | Corresponding vm_id |

---

### Image Checksum Identity (Normative)

The `{checksum}` component used in cache filenames, `references.json` keys, and
conversion lock names is computed as follows. All checksums use **SHA-256**
truncated to **16 hex characters** (64 bits) for path brevity. The full
64-character digest is stored in `references.json` as the `digest_full` field
for collision detection (see [references.json Structure](#referencesjson-structure)).

**For OCI images** (the primary path):

```
checksum = SHA256(
    manifest.config.digest + "\n" +
    manifest.layers[*].digest.join("\n") + "\n" +
    platform_os + "/" + platform_arch       // e.g., "linux/amd64"
)[:16]
```

- Layer digests are joined in **manifest order** (the OCI spec guarantees layer
  ordering within a manifest is immutable and meaningful — it encodes the
  filesystem stacking sequence).
- For **multi-arch manifest lists**: resolve to the platform-specific manifest
  FIRST (using runtime `GOARCH`), then compute the checksum above.
- Cache filename: `{checksum}_{arch}.qcow2` (e.g., `a1b2c3d4e5f6a7b8_amd64.qcow2`)

**For cloud images** (raw qcow2/img files):

```
checksum = SHA256(file_content)[:16]
arch     = detect from image metadata, or default to runtime arch
```

- Cache filename: same pattern `{checksum}_{arch}.qcow2`

**For URL-based images**:

```
checksum = SHA256(downloaded_file_content)[:16]
arch     = detect or default to runtime arch
```

#### Collision Handling

Although 16 hex characters (64 bits) make accidental collisions extremely
unlikely (birthday-bound: ~2^32 ≈ 4 billion images before 50% collision
probability), the `digest_full` field in `references.json` provides a safety
net:

1. **On AddReference**: If a `base_key` already exists in `references.json`,
   compare the incoming `digest_full` against the stored `digest_full`.
2. **If they match**: Same image — proceed normally (increment refcount).
3. **If they differ**: **Collision detected** — return an error:
   `"checksum collision: base_key {key} already maps to a different image
   (stored: {stored_digest[:16]}…, incoming: {incoming_digest[:16]}…)"`
4. **Remediation**: This error is a hard failure. The operator must resolve it
   manually — either purge the stale entry (if the cached image is no longer
   needed) or increase the truncation length in a future schema migration.
   At 64 bits this scenario is effectively impossible under normal usage.

This identity contract is referenced by:
- [06-concurrency.md](./06-concurrency.md) (conversion lock keys)
- [04-oci-conversion.md](./04-oci-conversion.md) (image pipeline)

### Storage Configuration

The actual implementation uses `CocoonConfig` (in `config/config.go`), which holds
all global Cocoon configuration -- not just storage paths. Derived path helpers
(`CacheDir()`, `VMDir()`, etc.) compute subdirectories from `RootDir`.

```go
// CocoonConfig holds global Cocoon configuration (config/config.go).
type CocoonConfig struct {
    // Directory roots
    RootDir    string `json:"root_dir"`     // Persistent root (default: /var/lib/cocoon)
    RuntimeDir string `json:"runtime_dir"`  // Ephemeral/tmpfs (default: /run/cocoon)
    LogDir     string `json:"log_dir"`      // Persistent logs (default: /var/log/cocoon)

    // Binaries and firmware
    CHBinary         string `json:"ch_binary"`           // Cloud Hypervisor binary
    UEFIFirmwarePath string `json:"uefi_firmware_path"`  // UEFI firmware path
    BuildahRoot      string `json:"buildah_root"`        // Buildah storage root

    // Default VM resources
    DefaultCPUs     int    `json:"default_cpus"`       // Default: 2
    DefaultMemoryMB int64  `json:"default_memory_mb"`  // Default: 2048
    DefaultDiskSize string `json:"default_disk_size"`   // Default: "10G"

    // Timeouts
    BootTimeoutSeconds int `json:"boot_timeout_seconds"`  // Default: 60
    StopTimeoutSeconds int `json:"stop_timeout_seconds"`  // Default: 30

    // Boot detection patterns (regex; defaults used if empty)
    BootSuccessPatterns []string `json:"boot_success_patterns,omitempty"`
    BootFailurePatterns []string `json:"boot_failure_patterns,omitempty"`
}

// DefaultConfig returns a CocoonConfig with production defaults.
func DefaultConfig() *CocoonConfig { ... }

// Derived path helpers (selected):
func (c *CocoonConfig) CacheDir() string          // RootDir/cache
func (c *CocoonConfig) ImageCacheDir() string      // RootDir/cache/images
func (c *CocoonConfig) ManifestCacheDir() string   // RootDir/cache/manifests
func (c *CocoonConfig) ConversionLockDir() string  // RootDir/cache/locks
func (c *CocoonConfig) VMDir() string              // RootDir/vms
func (c *CocoonConfig) TempDir() string            // RootDir/temp
func (c *CocoonConfig) DBDir() string              // RootDir/db
func (c *CocoonConfig) OCICacheDir() string        // RootDir/cache/oci
func (c *CocoonConfig) OCIBlobDir() string         // RootDir/cache/oci/blobs/sha256
func (c *CocoonConfig) OCILayoutDir() string       // RootDir/cache/oci/layouts
func (c *CocoonConfig) OCIBuildTxnLock() string    // RootDir/db/oci-build-txn.lock
func (c *CocoonConfig) OCIBuildTagIndex() string   // RootDir/db/oci-build-tags.json
func (c *CocoonConfig) OCIBuildTagLock() string    // RootDir/db/oci-build-tags.lock
func (c *CocoonConfig) OCILayerRefsFile() string   // RootDir/db/oci-layer-refs.json
func (c *CocoonConfig) OCILayerRefsLock() string   // RootDir/db/oci-layer-refs.lock

// EnsureDirs creates all required directories (db, cache/images,
// cache/manifests, cache/locks, cache/oci/blobs/sha256, cache/oci/layouts,
// vms, temp, firmware, buildah, RuntimeDir/vms, LogDir).
func (c *CocoonConfig) EnsureDirs() error { ... }
```

#### Phase 2 Planned Path Helpers

The following path helpers are planned for Phase 2 features. They are not yet
implemented but are documented here to reserve the namespace and ensure
consistency across design documents. See the Phase 2 tree diagram above for
the full directory layout.

```go
// Phase 2 — Warm Start (docs/15-warm-start.md)
func (c *CocoonConfig) CheckpointsDir() string                      // RootDir/checkpoints
func (c *CocoonConfig) CheckpointDir(ckptID string) string          // RootDir/checkpoints/{ckptID}
func (c *CocoonConfig) CheckpointIndexPath() string                 // RootDir/checkpoints/checkpoint-index.json
func (c *CocoonConfig) CheckpointIndexLock() string                 // RootDir/checkpoints/checkpoint-index.lock
func (c *CocoonConfig) CheckpointSnapshotDir(ckptID string) string  // RootDir/checkpoints/{ckptID}/ch-snapshot

// OCI VM Image Build (docs/04.1-oci-vm-images.md) — Implemented
func (c *CocoonConfig) OCICacheDir() string                         // RootDir/cache/oci
func (c *CocoonConfig) OCIBlobDir() string                          // RootDir/cache/oci/blobs/sha256
func (c *CocoonConfig) OCILayoutDir() string                        // RootDir/cache/oci/layouts
func (c *CocoonConfig) OCIBuildTxnLock() string                     // RootDir/db/oci-build-txn.lock
func (c *CocoonConfig) OCIBuildTagIndex() string                    // RootDir/db/oci-build-tags.json
func (c *CocoonConfig) OCIBuildTagLock() string                     // RootDir/db/oci-build-tags.lock
func (c *CocoonConfig) OCILayerRefsFile() string                    // RootDir/db/oci-layer-refs.json
func (c *CocoonConfig) OCILayerRefsLock() string                    // RootDir/db/oci-layer-refs.lock

// OCI VM Boot (docs/04.1-oci-vm-images.md) — Implemented
func (c *CocoonConfig) OCICacheEntry(digest string) string                // RootDir/cache/oci/{digest}
func (c *CocoonConfig) VMOCIUpperDir(vmID string) string                  // RootDir/vms/{vmID}/upper
func (c *CocoonConfig) VMOCIWorkDir(vmID string) string                   // RootDir/vms/{vmID}/work
func (c *CocoonConfig) VMOCIMergedDir(vmID string) string                 // RootDir/vms/{vmID}/merged
func (c *CocoonConfig) VMOCIRootfsVirtioFSSocketPath(vmID string) string  // RuntimeDir/vms/{vmID}/virtiofsd.sock

// Phase 2 — Networking (docs/16-networking.md)
func (c *CocoonConfig) DnsmasqStateDir() string                     // RootDir/dnsmasq
func (c *CocoonConfig) DnsmasqBridgeDir(bridge string) string       // RootDir/dnsmasq/{bridge}
func (c *CocoonConfig) VMNetNSPath(vmID string) string              // RuntimeDir/vms/{vmID}/netns

// Phase 2 — Volume Passthrough (docs/17-volume-passthrough.md)
func (c *CocoonConfig) VMVirtiofsSocketPath(vmID, tag string) string // RuntimeDir/vms/{vmID}/virtiofs-{tag}.sock
func (c *CocoonConfig) VMVirtiofsPIDPath(vmID, tag string) string    // RuntimeDir/vms/{vmID}/virtiofs-{tag}.pid
func (c *CocoonConfig) SharesDir() string                            // RootDir/shares
```

`EnsureDirs()` already creates `RootDir/cache/oci/blobs/sha256` and
`RootDir/cache/oci/layouts` at startup. It will be extended to create
`RootDir/checkpoints` and `RootDir/shares` at startup. `RootDir/dnsmasq`
is created when the first networked VM is provisioned. Per-VM directories
(`checkpoints/{ckptID}`, `vms/{vmID}/upper`, `vms/{vmID}/work`,
`vms/{vmID}/merged`, network namespace bind mounts, virtiofsd sockets)
are created on demand during VM or checkpoint setup rather than at startup.

## Copy-on-Write (COW) Strategy

### qcow2 Backing Files

The core of the storage efficiency comes from qcow2's backing file feature. Multiple VMs can share a single base image, with each VM storing only its differences in a lightweight overlay.

```go
// fileCOWManager implements COWManager (storage/local/cow.go).
// It stores base images in cache/images/ and overlays under vms/{vmID}/overlay.qcow2.
type fileCOWManager struct {
    cfg *config.CocoonConfig
}

func NewCOWManager(cfg *config.CocoonConfig) storage.COWManager {
    return &fileCOWManager{cfg: cfg}
}

// CreateBaseImage copies srcPath into the image cache as {baseKey}.qcow2.
// Idempotent: if the destination already exists, returns nil.
// After copying, the base image is marked read-only (0o444) to enforce
// immutability -- VMs use COW overlays and never write to the base.
func (m *fileCOWManager) CreateBaseImage(srcPath, baseKey string) error {
    dstPath := m.cfg.BaseImagePath(baseKey)
    // ... atomic copy: temp + fsync + rename ...
    // Mark read-only: os.Chmod(dstPath, 0o444)
    return nil
}

// CreateOverlay creates a COW overlay backed by {baseKey}.qcow2.
// The baseKey is resolved to the cached base image path internally.
// Returns the absolute overlay path (e.g., /var/lib/cocoon/vms/{vmID}/overlay.qcow2).
//
// Two-step process:
//   1. qemu-img create -f qcow2 -F qcow2 -b <basePath> <overlayPath>
//   2. If diskSize != "", qemu-img resize <overlayPath> <diskSize>
//
// No global lock is required because each VM directory is unique.
func (m *fileCOWManager) CreateOverlay(baseKey, vmID, diskSize string) (string, error) {
    basePath := m.cfg.BaseImagePath(baseKey)  // derives path from baseKey
    overlayPath := m.cfg.VMOverlayPath(vmID)

    // Step 1: Create overlay with backing file (no size argument).
    // qemu-img create -f qcow2 -F qcow2 -b <basePath> <overlayPath>
    cmd := exec.Command("qemu-img", "create",
        "-f", "qcow2", "-F", "qcow2",
        "-b", basePath, overlayPath)
    cmd.CombinedOutput()

    // Step 2: Resize separately if a disk size was requested.
    if diskSize != "" {
        resizeCmd := exec.Command("qemu-img", "resize", overlayPath, diskSize)
        resizeCmd.CombinedOutput()
    }

    return overlayPath, nil
}

// RemoveOverlay permanently removes the overlay disk file for vmID.
// The VM directory itself is preserved for the caller to handle.
func (m *fileCOWManager) RemoveOverlay(vmID string) error {
    overlayPath := m.cfg.VMOverlayPath(vmID)
    return os.Remove(overlayPath)
}
```

### Space Efficiency Example

The COW strategy dramatically reduces disk space requirements:

```python
# Scenario: 100 VMs from the same bootable OCI image

# 1. Prepare base image (cached, done once)
base_image = await image_manager.prepare_base_image("myorg/ubuntu-bootable:22.04")
# Result: /var/lib/cocoon/cache/images/abc123...qcow2 (5GB)

# 2. Create overlay for VM-1
vm1_overlay = cow_manager.create_overlay(base_image, "vm-001")
# Result: /var/lib/cocoon/vms/vm-001/overlay.qcow2 (~200KB initially)

# 3. Create overlay for VM-2 (shares same base)
vm2_overlay = cow_manager.create_overlay(base_image, "vm-002")
# Result: /var/lib/cocoon/vms/vm-002/overlay.qcow2 (~200KB initially)

# ... repeat 98 more times ...

# Total disk space for 100 VMs:
# - 1 base image: 5GB
# - 100 overlays: 100 × 200KB = 20MB
# - Total: ~5.02GB (not 500GB!)
```

Each overlay only grows as the VM writes data. If each VM writes 100MB of unique data, the total becomes 5GB + 10GB = 15GB, still far less than 100 × 5GB = 500GB.

## Reference Counting

### Tracking Image Usage

Reference counting ensures base images are not deleted while VMs still depend on them. The system maintains a mapping of base images to the VMs using them.

```go
// fileReferenceCounter implements ReferenceCounter (storage/local/refcount.go).
//
// IMPORTANT: There is no in-memory cache. Every public method acquires
// references.lock (flock, Level 2), loads references.json fresh from disk,
// performs its read-modify-write, persists atomically, and releases the lock.
// This ensures cross-process safety without stale data.
type fileReferenceCounter struct {
    cfg *config.CocoonConfig
}

func NewReferenceCounter(cfg *config.CocoonConfig) storage.ReferenceCounter {
    return &fileReferenceCounter{cfg: cfg}
}

// withRefsLock acquires references.lock, runs fn, then releases the lock.
func (rc *fileReferenceCounter) withRefsLock(fn func() error) error {
    fl := flock.New(rc.cfg.ReferencesLock())
    fl.Lock()
    defer fl.Unlock()
    return fn()
}

// loadRefs reads and unmarshals references.json.
// If the file does not exist, returns an empty map (not an error).
func (rc *fileReferenceCounter) loadRefs() (types.ReferencesFile, error) { ... }

// saveRefs atomically persists refs (temp + fsync + rename).
func (rc *fileReferenceCounter) saveRefs(refs types.ReferencesFile) error { ... }

// AddReference pins vmID to baseKey. Collision detection compares digestFull
// when the key already exists. Returns types.ErrChecksumCollision on mismatch.
func (rc *fileReferenceCounter) AddReference(baseKey, vmID, digestFull, sourceRef string) error {
    return rc.withRefsLock(func() error {
        refs, _ := rc.loadRefs()   // fresh load from disk
        // ... collision check, append vmID, save ...
        return rc.saveRefs(refs)
    })
}

// RemoveReference unpins vmID from baseKey.
// Deletes the entry entirely when the last reference is removed.
func (rc *fileReferenceCounter) RemoveReference(baseKey, vmID string) error {
    return rc.withRefsLock(func() error {
        refs, _ := rc.loadRefs()   // fresh load from disk
        // ... remove vmID; delete(refs, baseKey) if empty ...
        return rc.saveRefs(refs)
    })
}

// GetReferences returns the VM IDs currently pinning baseKey.
// Returns an empty slice (not nil) when the key has no references.
func (rc *fileReferenceCounter) GetReferences(baseKey string) ([]string, error) { ... }

// IsReferenced reports whether baseKey has at least one VM reference.
func (rc *fileReferenceCounter) IsReferenced(baseKey string) (bool, error) { ... }

// GetUnreferencedImages scans cache/images/ and returns base_key strings
// (not file paths) that have no entry (or an empty refs list) in references.json.
func (rc *fileReferenceCounter) GetUnreferencedImages() ([]string, error) {
    // Returns []string of base_key values, e.g. ["a1b2c3d4e5f6a7b8_amd64"]
    // NOT file system paths.
    ...
}
```

### references.json Structure

The reference count file stores a mapping of `base_key` (`{checksum_16}_{arch}`) to
reference metadata. Keys are content-addressed identifiers, NOT absolute paths.

```json
{
  "a1b2c3d4e5f6a7b8_amd64": {
    "path": "/var/lib/cocoon/cache/images/a1b2c3d4e5f6a7b8_amd64.qcow2",
    "digest_full": "a1b2c3d4e5f6a7b8901234567890abcdef1234567890abcdef1234567890abcd",
    "source_ref": "myorg/ubuntu-bootable:22.04",
    "refs": ["vm-001", "vm-002", "vm-003"],
    "created_at": "2026-02-12T10:00:00Z"
  },
  "f7e8d9c0b1a2e3f4_amd64": {
    "path": "/var/lib/cocoon/cache/images/f7e8d9c0b1a2e3f4_amd64.qcow2",
    "digest_full": "f7e8d9c0b1a2e3f4567890abcdef1234567890abcdef1234567890abcdef1234",
    "source_ref": "https://cloud-images.ubuntu.com/.../ubuntu-22.04-cloudimg-amd64.img",
    "refs": ["vm-010", "vm-011"],
    "created_at": "2026-02-12T11:00:00Z"
  }
}
```

**Field definitions**:
- `path`: Derived filesystem path to the cached qcow2 (for fast lookup)
- `digest_full`: Full 64-character SHA-256 hex digest (for collision detection when two different images produce the same 16-char truncation)
- `source_ref`: Original image reference (OCI ref / URL / file path) for audit and human readability
- `refs`: List of vm_ids currently using this base image
- `created_at`: RFC 3339 timestamp of first cache entry

### Add/Remove Operations

Reference counting operations are performed during VM lifecycle events:

```python
# When creating a VM
async def create_vm(image: str, vm_id: str) -> Path:
    # ... prepare base image (returns base_key, digest_full, source_ref, path) ...
    image_info = await image_mgr.prepare_base_image(image)

    # Pin reference FIRST (short lock hold), then create overlay outside lock
    ref_counter.add_reference(image_info.base_key, vm_id,
                              image_info.digest_full, image_info.source_ref)

    # ... create overlay (outside references.lock) ...
    overlay = cow_mgr.create_overlay(image_info.path, vm_id)

    return overlay

# When deleting a VM
def delete_vm(vm_id: str):
    # ... load config ...
    config = json.loads((vm_dir / "config.json").read_text())
    base_key = config["base_key"]  # e.g., "a1b2c3d4e5f6a7b8_amd64"

    # Remove reference using base_key
    ref_counter.remove_reference(base_key, vm_id)

    # ... cleanup overlay ...
```

### Concurrency Considerations

Reference counting operations must be **cross-process safe**. See [06-concurrency.md](./06-concurrency.md) for details on:

- File-based locking (flock) for `references.json` updates at `/var/lib/cocoon/db/references.lock`
- Atomic read-modify-write operations using temp files and fsync
- Race condition prevention during simultaneous VM creation/deletion across multiple processes
- Crash recovery (locks auto-released by kernel on process crash)
- Lock hierarchy to prevent deadlocks (Reference Lock is Level 2)

## Garbage Collection

### Overview

Garbage collection automatically reclaims disk space from resources that are no longer needed. All GC deletions are permanent. See [18-garbage-collection.md](./18-garbage-collection.md) for the full GC design document.

```go
// fileGarbageCollector implements GarbageCollector (storage/local/gc.go).
//
// Locking order (docs/06-concurrency.md):
//   1. gc.lock       (Level 1) -- acquired once per GC phase.
//   2. references.lock (Level 2) -- acquired per-image for atomic check-and-delete.
// Never acquire Level 1 while holding Level 2.
type fileGarbageCollector struct {
    cfg *config.CocoonConfig
}

func NewGarbageCollector(cfg *config.CocoonConfig) storage.GarbageCollector {
    return &fileGarbageCollector{cfg: cfg}
}

// CollectUnreferencedImages permanently deletes base images with zero
// references. No grace period -- if an image has zero refs, it is collected.
//
// For each candidate image, atomically under references.lock (L2):
//   1. Check refs -- skip if still referenced.
//   2. os.Remove the image file.
//   3. Delete the entry from references.json (if a zero-ref entry exists).
//
// Returns collected base_key strings (not file paths).
func (gc *fileGarbageCollector) CollectUnreferencedImages() ([]string, error) { ... }

// CollectOrphanedOverlays finds VM directories where overlay.qcow2 exists
// but config.json is missing, and permanently deletes the VM directory.
//
// Lock: gc.lock (L1) only.
func (gc *fileGarbageCollector) CollectOrphanedOverlays() ([]string, error) { ... }

// CollectOrphanedOCILayouts removes OCI layout directories in
// cache/oci/layouts/ not referenced by any tag in oci-build-tags.json.
// Lock: gc.lock (L1) -> oci-build-txn.lock -> oci-build-tags.lock.
func (gc *fileGarbageCollector) CollectOrphanedOCILayouts() ([]string, error) { ... }

// CollectStaleOCITags removes tags whose layout path no longer exists.
// Cascades cleanup to orphaned manifest refs and zero-ref blobs.
// Lock: gc.lock (L1) -> oci-build-txn.lock -> tags.lock + layer-refs.lock.
func (gc *fileGarbageCollector) CollectStaleOCITags() ([]string, error) { ... }

// CollectOrphanedOCIManifestRefs removes manifest refs not associated
// with any live tag. Zero-ref blobs are deleted (5-min grace for builds).
// Lock: gc.lock (L1) -> oci-build-txn.lock -> tags.lock + layer-refs.lock.
func (gc *fileGarbageCollector) CollectOrphanedOCIManifestRefs() ([]string, error) { ... }

// CollectUnreferencedOCIBlobs removes blobs from cache/oci/blobs/sha256/
// with zero manifest references in oci-layer-refs.json.
// Lock: gc.lock (L1) -> oci-layer-refs.lock for atomic check-and-delete.
func (gc *fileGarbageCollector) CollectUnreferencedOCIBlobs() ([]string, error) { ... }

// CollectStaleConversionLocks removes stale conversion lock files from
// cache/locks/ where the corresponding base image no longer exists and
// the lock is not currently held.
// Lock: gc.lock (L1) only.
func (gc *fileGarbageCollector) CollectStaleConversionLocks(maxAge time.Duration) ([]string, error) { ... }

// CollectTempFiles removes files/directories in temp/ older than maxAge.
// Lock: gc.lock (L1) only.
func (gc *fileGarbageCollector) CollectTempFiles(maxAge time.Duration) ([]string, error) { ... }

// FullGC runs a complete garbage collection cycle.
// Each phase acquires gc.lock independently -- NOT atomic across the full
// cycle, but safe because each phase performs its own reference check.
func (gc *fileGarbageCollector) FullGC() error {
    tempMaxAge := 1 * time.Hour
    lockMaxAge := storage.OCIGCGracePeriod

    gc.CollectUnreferencedImages()
    gc.CollectOrphanedOverlays()
    gc.CollectOrphanedOCILayouts()
    gc.CollectStaleOCITags()
    gc.CollectOrphanedOCIManifestRefs()
    gc.CollectUnreferencedOCIBlobs()
    gc.CollectStaleConversionLocks(lockMaxAge)
    gc.CollectTempFiles(tempMaxAge)
    return nil
}
```

### Garbage Collection Locking

GC operations must coordinate with concurrent VM create/delete operations. See [06-concurrency.md](./06-concurrency.md) for details on:

- Global GC lock at `/var/lib/cocoon/db/gc.lock` (Level 1 in lock hierarchy)
- Atomic check-and-delete using reference counter lock (Level 2)
- `oci-layer-refs.lock` (Level 2, same level as `references.lock`) for OCI blob reference tracking
- Lock ordering to prevent deadlocks with VM operations
- Crash recovery and lock auto-release

**OCI lock note**: `oci-layer-refs.lock` is a Level 2 lock, at the same level as `references.lock`. These two locks protect different files and are never held simultaneously. During OCI GC phases, the lock acquisition order is: `gc.lock` (L1) -> `oci-build-txn.lock` -> `oci-build-tags.lock` / `oci-layer-refs.lock` (L2), which respects the hierarchy.

**Key guarantee**: GC cannot delete a base image while any VM references it, even under high concurrency or process crashes.

### Collection Categories

#### 1. Unreferenced Base Images

Base images with zero VM references are collected immediately.

**Action:** For each candidate, atomically under references.lock: permanently delete the image file, then delete the entry from `references.json`.

**Locking**: GC acquires both GC lock (L1) and reference lock (L2) to perform atomic check-and-delete.

#### 2. Orphaned Overlays

Overlay images whose parent VM configuration has been deleted or corrupted. These indicate incomplete cleanup operations.

**Detection:** `overlay.qcow2` exists but `config.json` is missing in the same VM directory.

**Action:** Permanently delete the entire VM directory with `os.RemoveAll`.

**Locking**: GC lock (L1) only, as this operates on already-deleted VMs.

#### 3. Orphaned OCI Layouts

OCI layout directories in `cache/oci/layouts/` that are not referenced by any tag in `oci-build-tags.json`. These indicate layouts from deleted or re-tagged builds.

**Detection:** Directory exists in `cache/oci/layouts/` but its path does not appear in any tag entry in the tag index.

**Action:** Remove the orphaned layout directory permanently with `os.RemoveAll`. Blob hardlinks in the layout are deleted, but the underlying shared blobs in `cache/oci/blobs/sha256/` are preserved.

**Locking**: GC lock (L1) -> `oci-build-txn.lock` -> `oci-build-tags.lock` for an atomic tag-index snapshot serialized with concurrent build finalize/save-tag. Layouts younger than 5 minutes are skipped as defense-in-depth.

#### 4. Stale OCI Tags

Tags in `oci-build-tags.json` whose `layout_path` no longer exists on disk. These indicate tags left behind after their layouts were deleted or GC'd.

**Detection:** Tag entry's `layout_path` does not exist on the filesystem.

**Action:** Remove stale tags from the tag index. For each stale tag, if its `manifest_digest` becomes orphaned (no other live tag shares it), cascade cleanup: remove the manifest digest from all blob entries in `oci-layer-refs.json` and delete zero-ref blobs.

**Locking**: GC lock (L1) -> `oci-build-txn.lock` -> `oci-build-tags.lock` + `oci-layer-refs.lock`.

#### 5. Orphaned OCI Manifest Refs

Manifest digests in `oci-layer-refs.json` blob entries that are not associated with any live tag in the tag index.

**Detection:** Manifest digest appears in a blob's `manifest_digests` array but no live tag references that manifest.

**Action:** Filter orphaned manifest digests from blob entries. Delete blobs that become zero-ref (with 5-minute grace period for in-progress builds).

**Locking**: GC lock (L1) -> `oci-build-txn.lock` -> `oci-build-tags.lock` + `oci-layer-refs.lock`.

#### 6. Unreferenced OCI Blobs

Blobs in `cache/oci/blobs/sha256/` with zero manifest references in `oci-layer-refs.json`.

**Detection:** Blob file exists on disk and either has no entry in `oci-layer-refs.json` or has an entry with an empty `manifest_digests` array.

**Action:** Remove the blob file permanently from the shared store. Clean up the corresponding entry from `oci-layer-refs.json`.

**Locking**: GC lock (L1) followed by `oci-layer-refs.lock` for atomic check-and-delete.

#### 7. Stale Conversion Locks

Lock files in `cache/locks/` left behind after image deletion. These are best-effort hygiene to clean lock files that are no longer needed.

**Detection:** Lock file mtime is older than `maxAge`, the corresponding base image qcow2 does not exist, and the lock is not currently held (TryLock succeeds).

**Action:** Remove the stale lock file while holding the lock, then release.

**Locking**: GC lock (L1) only. Active lock files are preserved.

#### 8. Temporary Entries

Files/directories in the `/var/lib/cocoon/temp/` directory older than a threshold (default: 1 hour).

**Source:** Failed image conversions, interrupted downloads, or crashed operations.

**Action:** Permanently delete expired temp entries with `os.RemoveAll`.

**Locking**: GC lock (L1) only, as temp entries are not referenced.

### Defense-in-Depth Grace Periods

OCI resources use a 5-minute defense-in-depth grace period to prevent races with concurrent builds. Resources younger than 5 minutes are skipped even if they appear unreferenced. This applies to:

- OCI layouts (Phase 3)
- OCI blobs (Phase 5, 6)

Temp entries use a 1-hour max age (hardcoded in `FullGC()`).

### Garbage Collection Invocation

**Current implementation**: GC is triggered manually via the CLI:

```bash
cocoon gc            # Runs FullGC() — permanently deletes all unreferenced resources
cocoon gc --dry-run  # Preview what would be collected without deleting
```

There is no background scheduler or daemon loop. Operators should run `cocoon gc`
via cron or a systemd timer if periodic cleanup is desired.

**Future Work / Not Yet Implemented**: A scheduled background GC loop (e.g.,
running every hour inside a long-lived daemon) is planned but not yet
implemented. The current manual-only approach keeps the architecture simple
and avoids daemon management complexity.

## Storage Quotas

### Future Implementation

Storage quotas will limit disk space usage per tenant or VM pool. Planned features include:

- **Per-tenant quotas**: Limit total disk space per customer
- **Per-VM quotas**: Restrict individual VM overlay growth
- **Pool quotas**: Limit shared base image cache size
- **Soft/hard limits**: Warnings before enforcement
- **Quota enforcement**: Reject VM creation or writes when exceeded

See [future/storage-quotas.md](./future/storage-quotas.md) for detailed design.

## Example Workflows

The canonical create/delete sequences are defined above in [Add/Remove Operations](#addremove-operations)
and in [09-cli-design.md § 6.1–6.3](./09-cli-design.md#61-vm-creation-flow-cocoon-run).
Refer to those sections for the authoritative ordering (pin reference → create overlay → boot)
and API signatures (`add_reference(base_key, vm_id, digest_full, source_ref)`).

## Performance Considerations

### Filesystem-Level COW

Use filesystem-level copy-on-write when available for even faster operations:

```bash
# Use reflink for instant copies on btrfs/xfs
cp --reflink=auto base.qcow2 copy.qcow2

# zstd compression for base images
qemu-img convert -f qcow2 -O qcow2 -o compression_type=zstd input.qcow2 output.qcow2
```

### Monitoring and Metrics

Track storage usage metrics:

- Base image cache size
- Total overlay size per VM
- Reference count per base image
- GC collection rates

## Summary

The storage management system provides:

1. **Efficient Layout**: Organized directories separating base images, overlays, and temp files
2. **Space Optimization**: COW overlays allow 100 VMs to use ~5GB instead of 500GB
3. **Safety**: Reference counting prevents premature deletion of in-use base images
4. **Automation**: Garbage collection permanently cleans up unreferenced resources
5. **Scalability**: Supports high-concurrency VM creation through shared base images

The combination of qcow2 backing files, checksum-based caching, and intelligent reference counting delivers an optimal storage solution for high-concurrency VM operations.

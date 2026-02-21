# RFC 0061: Unified Storage — EROFS + Guest-Side COW Overlay

- **RFC Number**: 0061
- **Title**: Unified Storage with EROFS Block Devices and Guest-Side COW Overlay
- **Author(s)**: CMGS (@CMGS), Claude (@anthropic)
- **Status**: Draft
- **Created**: 2026-02-21
- **Updated**: 2026-02-21
- **Issue**: https://github.com/CMGS/cocoon/issues/61

## Summary

Replace both boot paths (UEFI qcow2 and OCI virtiofs) with a single unified
architecture: all images (cloudimg and OCI) are normalized to content-addressed
layers, delivered as read-only EROFS block devices via virtio-blk, with a per-VM
raw COW disk. Guest-side overlayfs (assembled by a patched initramfs) replaces
both host-side qcow2 backing and host-side overlayfs + virtiofsd.

## Motivation

### Current Architecture Has Two Divergent Boot Paths

| | UEFI Boot (cloudimg) | Direct Boot (OCI VM) |
|---|---|---|
| Firmware | CLOUDHV.fd | None |
| Root delivery | qcow2 base + qcow2 COW overlay | virtiofsd + host overlayfs (N layers) |
| COW mechanism | qcow2 backing chain (host) | virtiofsd writes to overlay upper (host) |
| Shutdown | ACPI power-button → guest systemd | vm.shutdown API → SIGTERM |
| Per-VM processes | CH only | CH + virtiofsd |
| Boot latency | ~10s | ~30s (virtiofsd + host overlay overhead) |

This dual-path architecture doubles the code surface for storage management,
lifecycle, GC, and testing.

### OCI Direct Boot Is Slow

The 20s overhead vs UEFI boot comes entirely from host-side rootfs preparation:

- Host overlayfs mount with N lowerdirs: 5-10s
- virtiofsd startup + chroot sandbox: 3-5s
- Socket readiness polling: ~100ms

### qcow2 Adds Unnecessary Complexity

- qcow2 format driver in the I/O path adds latency and potential bugs
- Backing-file chain management (create, validate, resize) is complex
- Image conversion pipeline (download → convert → qcow2) is heavyweight

## Detailed Design

### Unified Layer Model

Both cloudimg and OCI images are normalized to the same 3-tier structure:

```
Tier 1: Kernel Layer    (files on host — vmlinuz + patched initramfs)
Tier 2: Rootfs Layer(s) (EROFS block device(s) — read-only, passed as vdX)
Tier 3: COW Disk        (raw ext4 block device — read-write, per-VM)
```

For cloudimg:
```
cloud-hypervisor \
  --kernel   cache/layers/{sha_k}/vmlinuz \
  --initramfs cache/layers/{sha_k}/initrd.img \
  --disk path=cache/layers/{sha_r}/rootfs.erofs,readonly=on \   # vda
  --disk path=vms/{vmID}/cow.raw \                               # vdb
  --cmdline "... cocoon.layers=vda cocoon.cow=vdb"
```

For OCI VM image (with user custom layer):
```
cloud-hypervisor \
  --kernel   cache/layers/{sha_k}/vmlinuz \
  --initramfs cache/layers/{sha_k}/initrd.img \
  --disk path=cache/layers/{sha_base}/rootfs.erofs,readonly=on \  # vda
  --disk path=cache/layers/{sha_user}/custom.erofs,readonly=on \  # vdb
  --disk path=vms/{vmID}/cow.raw \                                 # vdc
  --cmdline "... cocoon.layers=vda,vdb cocoon.cow=vdc"
```

Multiple VMs from the same image share kernel and rootfs layers (content-
addressed by SHA-256). Each VM only gets its own COW raw file.

### How EROFS Replaces qcow2 Backing

qcow2 backing is host-side block-level COW. The new architecture moves COW to
guest-side file-level overlayfs:

```
Before (qcow2):
  host:  base.qcow2 ← overlay.qcow2 (qcow2 internal COW)
  CH:    one virtual disk (overlay reads through to base)
  guest: /dev/vda — single disk, full filesystem

After (EROFS + COW):
  host:  rootfs.erofs (shared, immutable) + cow.raw (per-VM)
  CH:    two+ virtual disks (erofs readonly + raw writable)
  guest: initramfs assembles overlayfs from block devices
```

Every qcow2 operation maps to an EROFS equivalent:

| qcow2 Operation | EROFS Equivalent |
|---|---|
| `qemu-img convert` → base.qcow2 | `mkfs.erofs` → rootfs.erofs |
| `qemu-img create -b base overlay` | `truncate -s 20G cow.raw && mkfs.ext4` |
| CH: single disk (overlay backing base) | CH: N+1 disks (N erofs ro + 1 raw rw) |
| host qcow2 driver does COW | guest initramfs does overlayfs |
| base image refcount | layer refcount (same SHA model) |
| GC: refcount=0 → delete base.qcow2 | GC: refcount=0 → delete layer |
| `qemu-img resize overlay +10G` | `truncate -s +10G cow.raw` + guest `resize2fs` |

### Storage Layout

```
cache/
├── layers/                              # Content-addressed layer store
│   ├── {sha256_kernel}/                 # Kernel layer
│   │   ├── vmlinuz                      # Extracted from image
│   │   ├── initrd.img                   # Original + cocoon overlay hook + modules
│   │   └── meta.json                    # { type, source_ref, arch }
│   ├── {sha256_rootfs}/                 # Rootfs layer
│   │   ├── rootfs.erofs                 # mkfs.erofs from image rootfs
│   │   └── meta.json                    # { type, source_ref, size, compression }
│   └── {sha256_custom}/                 # User custom layer (OCI only)
│       ├── custom.erofs
│       └── meta.json
│
├── refs/
│   ├── layers.json                      # layer SHA → { refs: [vmID, ...] }
│   └── layers.lock
│
vms/
├── {vmID}/
│   ├── config.json                      # { kernel_layer, rootfs_layers: [], ... }
│   ├── cow.raw                          # Per-VM writable disk
│   └── metadata.json                    # Runtime state
```

### Initramfs Patching Strategy

**Key constraint**: vmlinuz is extracted from the source image (cloudimg or OCI).
The kernel's built-in vs module config is unknown and varies by distro/version.
A fully custom initramfs would need to solve kernel module dependencies
(erofs.ko, overlay.ko, and their transitive deps). Instead, we patch the
original initramfs.

**Decision**: patch the original initramfs, don't replace it.

Rationale:
1. Original initramfs contains all modules matching the extracted vmlinuz
2. Original udev rules handle virtio device discovery
3. No need for depmod analysis or module extraction
4. Distro-specific quirks (device naming, module loading) are pre-handled

However, the original initramfs likely does NOT contain `erofs.ko` or
`overlay.ko` (since the original rootfs is ext4). These modules must be
extracted from the image's `/lib/modules/<ver>/` and injected.

#### Materialize Pipeline

```
Source Image (cloudimg.img or OCI layers)
  │
  ├─ Mount rootfs (read-only)
  │
  ├─ Extract /boot/vmlinuz-<ver>                    → vmlinuz (as-is)
  │
  ├─ Extract /boot/initrd.img-<ver>                 → original initramfs
  │
  ├─ Extract kernel modules:
  │   ├─ Check /lib/modules/<ver>/modules.builtin   → skip if built-in
  │   ├─ Parse /lib/modules/<ver>/modules.dep       → dependency graph
  │   ├─ Resolve transitive deps for: erofs, overlay
  │   │   (e.g., erofs → lz4_decompress, lz4hc_compress)
  │   └─ Extract all .ko files in dependency chain
  │
  ├─ mkfs.erofs rootfs.erofs (from mount point, unmodified)
  │
  └─ Unmount
  │
  ├─ Patch initramfs:
  │   ├─ Unpack (cpio + gzip/zstd, handle microcode preamble)
  │   ├─ Detect initramfs format (initramfs-tools vs dracut)
  │   ├─ Inject /cocoon-modules/*.ko (erofs, overlay, deps)
  │   ├─ Inject hook script at distro-specific path:
  │   │   ├─ initramfs-tools: /scripts/local-bottom/cocoon
  │   │   └─ dracut: /lib/dracut/hooks/mount/99-cocoon-mount.sh
  │   └─ Repack → initrd.img
  │
  └─ Store: cache/layers/{sha_kernel}/ and cache/layers/{sha_rootfs}/
```

#### Distro Detection (initramfs format)

The unpacked initramfs is probed to determine the hook system. The Go
`PatchInitramfs` function detects the format by filesystem layout:

```go
func detectInitramfsFormat(unpackDir string) InitramfsFormat {
    // initramfs-tools: has /scripts/local or /scripts/functions
    if exists(filepath.Join(unpackDir, "scripts", "local")) ||
       exists(filepath.Join(unpackDir, "scripts", "functions")) {
        return FormatInitramfsTools
    }
    // dracut: has /lib/dracut/ or /usr/lib/dracut/
    if exists(filepath.Join(unpackDir, "lib", "dracut")) ||
       exists(filepath.Join(unpackDir, "usr", "lib", "dracut")) {
        return FormatDracut
    }
    return FormatUnknown // fallback — try initramfs-tools convention
}
```

| Distro Family | Format | Hook Path | Root Variable |
|---|---|---|---|
| Debian / Ubuntu | initramfs-tools | `/scripts/local-bottom/cocoon` | `${rootmnt}` |
| RHEL / CentOS / Fedora | dracut | `/lib/dracut/hooks/mount/99-cocoon-mount.sh` | `$NEWROOT` |

#### Guest Boot Hook — initramfs-tools (Debian/Ubuntu)

Injected at `/scripts/local-bottom/cocoon`:

```sh
#!/bin/sh
PREREQ=""
prereqs() { echo "$PREREQ"; }
case $1 in prereqs) prereqs; exit 0;; esac
. /scripts/functions

mountroot() {
    log_begin_msg "Cocoon: mounting overlay rootfs"

    # Load injected modules (insmod, no modprobe dependency).
    # Guard: if all modules are built-in, /cocoon-modules/ won't exist.
    if [ -d /cocoon-modules ]; then
        for mod in /cocoon-modules/*.ko; do
            [ -e "$mod" ] || continue
            insmod "$mod" 2>/dev/null   # already-builtin → harmless EEXIST
        done
    fi

    # Parse topology from kernel cmdline (cocoon controls this)
    LAYERS=$(cat /proc/cmdline | tr ' ' '\n' | \
             grep '^cocoon\.layers=' | cut -d= -f2)
    COW=$(cat /proc/cmdline | tr ' ' '\n' | \
          grep '^cocoon\.cow=' | cut -d= -f2)

    [ -z "$LAYERS" ] && panic "cocoon.layers= not set"
    [ -z "$COW" ]    && panic "cocoon.cow= not set"

    # Mount under /run — survives switch_root via mount --move
    COCOON="/run/cocoon/storage"
    mkdir -p "$COCOON"

    # Mount read-only layers
    LOWER=""
    for dev in $(echo "$LAYERS" | tr ',' ' '); do
        mnt="${COCOON}/layers/${dev}"
        mkdir -p "$mnt"
        mount -t erofs -o ro "/dev/${dev}" "$mnt" || panic "mount ${dev} failed"
        [ -n "$LOWER" ] && LOWER="${LOWER}:"
        LOWER="${LOWER}${mnt}"
    done

    # Mount COW (pre-formatted by cocoon create on host)
    mkdir -p "${COCOON}/cow"
    mount -t ext4 "/dev/${COW}" "${COCOON}/cow" || panic "mount COW failed"
    mkdir -p "${COCOON}/cow/upper"
    rm -rf "${COCOON}/cow/work" && mkdir -p "${COCOON}/cow/work"

    # Assemble overlay
    mount -t overlay overlay \
      -o "lowerdir=${LOWER},upperdir=${COCOON}/cow/upper,workdir=${COCOON}/cow/work" \
      "${rootmnt}" || panic "overlay failed"

    mkdir -p "${rootmnt}/dev" "${rootmnt}/proc" "${rootmnt}/sys" "${rootmnt}/run"

    # --- Systemd compatibility patching (written to COW upper, EROFS untouched) ---
    # Without these, systemd will hang or fail on boot. See "Systemd
    # Compatibility" section below for the full rationale.

    # 1. Clear fstab — original references a physical UUID that doesn't exist.
    #    Systemd would wait 90s for it, then emergency-shell.
    : > "${rootmnt}/etc/fstab"

    # 2. Mask fsck + remount — no fsck.overlay binary exists; remount with
    #    fstab-derived options fails on overlay.
    ln -sf /dev/null "${rootmnt}/etc/systemd/system/systemd-fsck-root.service"
    ln -sf /dev/null "${rootmnt}/etc/systemd/system/systemd-remount-fs.service"

    log_success_msg "Cocoon: overlay rootfs ready"
}
```

#### Guest Boot Hook — dracut (RHEL/CentOS/Fedora)

Injected at `/lib/dracut/hooks/mount/99-cocoon-mount.sh`:

```sh
#!/bin/sh
# Cocoon overlay rootfs hook for dracut-based initramfs.
# dracut requires rootok=1 to signal that a mount handler exists.

# Signal to dracut that we will handle rootfs mounting.
rootok=1

# Load injected modules (insmod, no modprobe dependency).
# Guard: if all modules are built-in, /cocoon-modules/ won't exist.
if [ -d /cocoon-modules ]; then
    for mod in /cocoon-modules/*.ko; do
        [ -e "$mod" ] || continue
        insmod "$mod" 2>/dev/null   # already-builtin → harmless EEXIST
    done
fi

# Parse topology from kernel cmdline (cocoon controls this)
LAYERS=$(cat /proc/cmdline | tr ' ' '\n' | \
         grep '^cocoon\.layers=' | cut -d= -f2)
COW=$(cat /proc/cmdline | tr ' ' '\n' | \
      grep '^cocoon\.cow=' | cut -d= -f2)

if [ -z "$LAYERS" ] || [ -z "$COW" ]; then
    die "cocoon: cocoon.layers= or cocoon.cow= not set on cmdline"
fi

# Mount under /run — survives switch_root via mount --move
COCOON="/run/cocoon/storage"
mkdir -p "$COCOON"

# Mount read-only layers
LOWER=""
for dev in $(echo "$LAYERS" | tr ',' ' '); do
    mnt="${COCOON}/layers/${dev}"
    mkdir -p "$mnt"
    mount -t erofs -o ro "/dev/${dev}" "$mnt" || die "cocoon: mount ${dev} failed"
    [ -n "$LOWER" ] && LOWER="${LOWER}:"
    LOWER="${LOWER}${mnt}"
done

# Mount COW (pre-formatted by cocoon create on host)
mkdir -p "${COCOON}/cow"
mount -t ext4 "/dev/${COW}" "${COCOON}/cow" || die "cocoon: mount COW failed"
mkdir -p "${COCOON}/cow/upper"
rm -rf "${COCOON}/cow/work" && mkdir -p "${COCOON}/cow/work"

# Assemble overlay — dracut uses $NEWROOT as the sysroot target
mount -t overlay overlay \
  -o "lowerdir=${LOWER},upperdir=${COCOON}/cow/upper,workdir=${COCOON}/cow/work" \
  "$NEWROOT" || die "cocoon: overlay mount failed"

mkdir -p "$NEWROOT/dev" "$NEWROOT/proc" "$NEWROOT/sys" "$NEWROOT/run"

# --- Systemd compatibility patching (written to COW upper, EROFS untouched) ---
: > "$NEWROOT/etc/fstab"
ln -sf /dev/null "$NEWROOT/etc/systemd/system/systemd-fsck-root.service"
ln -sf /dev/null "$NEWROOT/etc/systemd/system/systemd-remount-fs.service"
```

Key differences from initramfs-tools:
- Uses `$NEWROOT` instead of `${rootmnt}`
- Sets `rootok=1` to tell dracut a mount handler is present
- Uses `die` instead of `panic` for fatal errors
- No `mountroot()` function wrapper — dracut hooks execute as plain scripts
- No `prereqs` / PREREQ / `. /scripts/functions` preamble

#### Systemd Compatibility Patching

After assembling the overlay but before `switch_root`, the hook performs
three **mandatory** patches. Without these, systemd (PID 1) will hang or
crash on boot. All writes land in the COW upper layer — the EROFS base
remains byte-for-byte identical to the source image.

**Problem 1: `/etc/fstab` references a non-existent physical disk**

The original cloudimg fstab contains entries like:

```
UUID=xxxx-xxxx  /  ext4  defaults  0  1
```

Systemd reads fstab and tries to locate that UUID. Since we never passed
that physical disk to the VM (the root is an overlay), systemd triggers
`systemd-fsck-root.service` which waits up to 90 seconds for the disk,
then drops to emergency shell.

Fix: `> /etc/fstab` — clear the file so systemd has no expectations.

**Problem 2: `systemd-fsck-root.service` — no `fsck.overlay`**

Even if systemd detects the root is overlay, it tries to run `fsck.overlay`
before mounting. This binary does not exist in any Linux distribution
(overlayfs is a union filesystem, not a block filesystem — traditional
block-level fsck is meaningless). Missing binary → dependency failure →
boot blocked.

Fix: `ln -sf /dev/null /etc/systemd/system/systemd-fsck-root.service`

**Problem 3: `systemd-remount-fs.service` — overlay remount fails**

The standard Linux boot sequence has initramfs mount root as read-only,
then systemd remounts it read-write after fsck. `systemd-remount-fs`
reads fstab to get mount options and calls `mount -o remount`. Our overlay
is already rw, and remounting with ext4-style fstab options on an overlay
filesystem fails with parameter mismatch.

Fix: `ln -sf /dev/null /etc/systemd/system/systemd-remount-fs.service`

**Why this is safe**:
- These patches are the **minimum necessary set** — only services that are
  structurally incompatible with overlay root. No cloud-init disabling, no
  optional service masking.
- Writes go to COW upper layer only. The EROFS rootfs is never modified.
- On VM deletion, the COW disk is deleted. No persistent side effects.
- `boot-efi.mount` is not masked — with no UEFI firmware there is no ESP
  partition, so systemd skips it automatically (no fstab entry, no device).

#### Guest Visibility

After boot, mount points are hidden under `/run/cocoon/storage/`:

| Guest action | Visible? |
|---|---|
| `ls /` | No (nothing in root dir) |
| `ls -a /` | No (mount points are under /run) |
| `ls /run/cocoon/` | Yes (for debugging) |
| `mount`, `/proc/mounts` | Yes (kernel exposes all mounts) |
| `lsblk` | Yes (kernel exposes block devices) |

This is the standard behavior for VM sandboxes. `/run` is FHS-compliant
for runtime state, and `/run/cocoon/storage/` serves as a debug inspection
point.

### Topology Declaration

Cocoon controls the CH kernel cmdline. The boot hook reads topology from
two custom parameters:

- `cocoon.layers=vda,vdb` — ordered list of read-only EROFS devices
  (leftmost = highest priority in overlayfs lowerdir)
- `cocoon.cow=vdc` — the writable COW device

This is explicit and deterministic, unlike filesystem-type probing (blkid)
which has issues:
- blkid may not exist in all initramfs environments
- Heuristic detection of COW disk is fragile (unformatted disks, data volumes)
- Device enumeration timing is non-deterministic in edge cases

### Cloudimg Materialize Pipeline

```
cloudimg.img (download)
  → Parse GPT in Go → compute ext4 rootfs partition offset
  → mount -o ro,loop,offset=<N> (requires root)
  → Extract: vmlinuz, initrd.img, kernel modules (erofs/overlay dep chain)
  → mkfs.erofs rootfs.erofs (from mount point, no guest modification)
  → Unmount
  → Patch initramfs: unpack → detect format → inject modules + hook → repack
  → SHA-256 hash kernel layer and rootfs layer
  → Store in cache/layers/
```

No dependency on libguestfs, virt-customize, losetup, or kpartx. Tools needed:
- `mount` (cocoon runs as root — loop + offset mount is always available)
- `mkfs.erofs` (erofs-utils package)
- Go stdlib for GPT parsing and cpio/gzip unpack-repack

### OCI Image Materialize Pipeline

```
OCI image layers (pulled or built)
  → Flatten all rootfs layers to temp directory
  → Extract: vmlinuz, initrd.img, kernel modules
  → mkfs.erofs rootfs.erofs (from flattened rootfs)
  → Patch initramfs (same as cloudimg — auto-detect format)
  → SHA-256 hash layers
  → Store in cache/layers/
```

For OCI images with distinct user layers:
```
  → Base layers → flatten → base.erofs
  → User layers → flatten → custom.erofs
  → Both passed as separate vdX devices
```

### Per-VM COW Disk Management

COW disks are raw ext4 files, created at `cocoon create` time:

```go
// In create stage (host-side)
func createCOWDisk(path string, size int64) error {
    // Sparse file — instant creation, no space allocation until written
    f, _ := os.Create(path)
    f.Truncate(size)
    f.Close()
    // Format ext4 (host-side, not guest-side)
    return exec.Command("mkfs.ext4", "-F", "-m", "0", "-q", path).Run()
}
```

Resize: `truncate -s +10G cow.raw` on host, then `resize2fs /dev/vdX` in guest.

### Reference Counting

Layers are content-addressed by SHA-256. Reference counting works identically
to the current base-image model:

```json
// cache/refs/layers.json
{
  "sha256_aaaa": {
    "type": "kernel",
    "refs": ["vm-1", "vm-2", "vm-3"],
    "source_ref": "noble-server-cloudimg-amd64"
  },
  "sha256_bbbb": {
    "type": "rootfs",
    "refs": ["vm-1", "vm-2"],
    "source_ref": "noble-server-cloudimg-amd64"
  }
}
```

GC: layer with `refs: []` and grace period expired → delete.

### Code Impact

| Module | Change |
|---|---|
| `image/pipeline/` | **Rewrite** — new materialize: extract kernel + mkfs.erofs + patch initramfs |
| `storage/local/` | **Rewrite** — COW from qcow2 to raw; refcount from base-image to layer |
| `vm/engine/manager.go` | **Simplify** — single direct boot path, no UEFI, no virtiofsd |
| `vm/engine/create.go` | **Simplify** — create COW raw, pin layers, no overlay mount |
| `oci/` | **Adapt** — layer extraction produces erofs instead of unpacked dirs |
| `hypervisor/` | **Simplify** — remove UEFI/ACPI shutdown, remove virtiofsd management |
| `config/` | **Update** — new path helpers for layer store |

**Removed entirely**:
- `vm/engine/overlay_runtime_linux.go` / `overlay_runtime_other.go`
- `vm/engine/virtiofsd_*.go`
- `image/pipeline/convert_linux.go` (qcow2 conversion)
- UEFI firmware lookup and ACPI power-button shutdown path

### Implementation Pitfalls

Three low-level hazards that will bite during Go implementation of the
materialize pipeline. Documented here so implementors don't rediscover
them the hard way.

#### Pitfall 1: Concatenated initramfs (microcode preamble)

Ubuntu (and many other distros) ship `initrd.img` as a **concatenated
file**, not a single compressed archive:

```
┌──────────────────────────────┬─────────────────────────────────┐
│ Segment 1: uncompressed cpio │ Segment 2: gzip/zstd-compressed │
│ (CPU microcode:              │ cpio (the real initramfs)        │
│  AuthenticAMD.bin or         │                                  │
│  GenuineIntel.bin)           │                                  │
└──────────────────────────────┴─────────────────────────────────┘
```

The Go unpacker **must not** feed the entire file to `gzip.NewReader`.
Correct approach:

1. Open the file as a raw `io.Reader`.
2. Read the first cpio archive (uncompressed). Detect EOF by the cpio
   trailer record (`TRAILER!!!`).
3. Probe the next bytes for a compression magic number (`1f 8b` for gzip,
   `28 b5 2f fd` for zstd, `fd 37 7a 58 5a` for xz).
4. Decompress the second segment, which yields the real cpio archive.
5. Unpack, inject modules + hook, repack the second segment.
6. **Reassemble**: concatenate segment 1 (preserved verbatim) + recompressed
   segment 2. The kernel bootloader expects this exact layout.

#### Pitfall 2: `modules.builtin` — don't inject what's already in the kernel

Some distros compile `erofs` or `overlay` directly into the kernel
(`CONFIG_EROFS_FS=y` rather than `=m`). When Go resolves module
dependencies:

1. First check `/lib/modules/<ver>/modules.builtin` — a plain text file
   listing all built-in module paths (one per line, e.g.
   `kernel/fs/erofs/erofs.ko`).
2. If a required module appears in `modules.builtin`, skip extraction.
   The `insmod` in the hook script will get `EEXIST` which is harmless,
   but there's no `.ko` file to extract in the first place.
3. If the module is not in `modules.builtin` AND not in `modules.dep`,
   the image's kernel genuinely lacks support — fail the materialize with
   a clear error.

#### Pitfall 3: CPIO repack must preserve permissions and ownership

When using Go's `archive/cpio` (or equivalent) to repack the initramfs:

- Every injected file's `cpio.Header` must have correct `Mode`, `Uid`,
  and `Gid`. In particular, the hook script (`/scripts/local-bottom/cocoon`
  or `/lib/dracut/hooks/mount/99-cocoon-mount.sh`) **must be mode `0755`**.
  If the executable bit is missing, the initramfs framework silently skips
  it and the overlay never gets mounted.
- Injected `.ko` module files should be mode `0644`, owned by `root:root`.
- When repacking existing files from the original initramfs, copy the
  original `cpio.Header` fields verbatim — do not let Go defaults zero
  out the permission bits.

## Drawbacks

1. **Host dependency on erofs-utils** — `mkfs.erofs` must be installed.
   Not available in all distro default repos (but widely packaged).

2. **initramfs patching is distro-format-dependent** — initramfs-tools
   (Debian/Ubuntu) and dracut (RHEL/CentOS/Fedora) have different hook
   formats. Both are supported via auto-detection (see "Distro Detection"),
   but exotic initramfs systems (e.g., mkinitcpio on Arch) are not yet
   covered. The cpio unpack/repack is universal.

3. **Guest-visible mount points** — `/proc/mounts` and `lsblk` expose the
   overlay structure. Acceptable for VM sandboxes but less opaque than qcow2.

4. **No snapshot support** — qcow2 has built-in snapshot. Raw COW + overlay
   requires external mechanisms (filesystem-level or btrfs reflink).

5. **COW disk resize requires guest cooperation** — `resize2fs` must run
   inside the guest. qcow2 resize is transparent to the guest.

## Alternatives

### Keep qcow2 for cloudimg, EROFS for OCI only

Lower risk (no cloudimg pipeline change) but maintains the dual-path
architecture that this RFC aims to eliminate.

### squashfs instead of EROFS

Original issue 61 proposed squashfs. EROFS is preferred because:
- Better random-read performance (no block decompression overhead for uncompressed regions)
- Finer-grained compression (per-extent vs per-block)
- Actively developed in upstream Linux (EROFS is the chosen format for Android system partitions)
- `mkfs.erofs` supports LZ4HC compression for fast decompression

### Fully custom initramfs (Go-based init)

Eliminates distro initramfs dependency entirely but requires:
- Kernel module dependency analysis (depmod) for every kernel version
- Reimplementing device discovery (udev rules)
- Maintaining PID 1 robustness (crash = kernel panic, no recovery)

The patched-initramfs approach gets 90% of the benefit at 10% of the risk.
A fully custom init can be a future optimization if the patching approach
proves insufficient.

### virtiofs with EROFS backing (no guest overlay)

Use virtiofsd to serve EROFS content. This keeps the host-side architecture
but doesn't eliminate virtiofsd overhead. Rejected because the primary
motivation is removing virtiofsd.

## Prior Art

- **Kata Containers**: ships a custom `kata-agent` initramfs with its own
  kernel. Full BYOI approach. Cocoon's patched-initramfs is a lighter variant.
- **Firecracker (Lambda)**: uses a minimal custom kernel + initramfs with
  ext4 root passed as block device.
- **Android**: uses EROFS for system partition (read-only) with f2fs/ext4
  userdata partition. Same COW-on-block-device model.
- **WSL2**: ships a custom Microsoft kernel + initramfs. Mounts host
  filesystems via Plan 9 (9p) but the init model is identical.
- **ChromeOS**: uses dm-verity protected squashfs/erofs root with a
  separate writable partition for user data.

## Unresolved Questions

1. **EROFS compression**: lz4hc (fast decompress) vs zstd (better ratio)?
   Recommend lz4hc for boot latency.

2. **~~initramfs-tools vs dracut hook format~~**: resolved — both formats are
   specified above. `PatchInitramfs` auto-detects the format by probing the
   unpacked initramfs layout and injects the appropriate hook script.

3. **~~cloudimg partition discovery~~**: resolved. Parse GPT in Go to
   compute ext4 partition offset, then `mount -o ro,loop,offset=<N>`.
   Cocoon runs as root (no rootless mode), so `mount` is available.
   No dependency on `losetup` or `kpartx`.

4. **Multi-arch support**: amd64 and arm64 cloudimgs have different kernel
   configs. Module availability (erofs=m vs erofs=y) may differ.

5. **Migration path**: existing VMs use qcow2. Options:
   - Re-materialize on next `cocoon start` (slow first start)
   - Parallel support during transition (defeats the purpose)
   - Breaking change with major version bump

6. **COW disk default size**: inherit from `--disk-size` flag or separate?

7. **~~Distro-specific guest patches~~**: resolved. Minimal mandatory set
   identified: clear fstab + mask fsck-root + mask remount-fs. These are
   structurally required (systemd cannot boot overlay root without them).
   No optional service masking (cloud-init, boot-efi.mount, etc.) — those
   are left to the user if needed.

## Implementation Phases

### Phase 1: OCI VM Path (lowest risk)

Cocoon already controls OCI image initramfs. Replace virtiofsd + host
overlay with EROFS block device + guest overlay.

- Materialize: flatten layers → mkfs.erofs
- Initramfs: inject cocoon overlay hook + modules
- Create: raw COW disk, no host overlay mount
- Start: direct boot with EROFS vda + COW vdb
- Remove virtiofsd lifecycle management

### Phase 2: Cloudimg Path

Replace qcow2 pipeline with EROFS.

- Materialize: extract kernel + initramfs + mkfs.erofs from cloudimg
- Patch initramfs: inject hook + erofs/overlay modules
- Create: raw COW disk
- Remove qcow2 conversion, UEFI firmware, ACPI shutdown

### Phase 3: Cleanup

- Remove dual-path code (UEFI vs direct boot branching)
- Simplify GC (single layer model)
- Update all docs
- Migration tooling for existing VMs

## Future Possibilities

1. **Custom cocoon kernel**: optional `cocoon kernel build` with `EROFS=y
   OVERLAY=y` built-in. Eliminates module injection, works with any rootfs.

2. **Incremental layer updates**: EROFS layers enable docker-like layer
   sharing. Pull only changed layers on image update.

3. **dm-verity**: verify EROFS integrity at block level. Detect rootfs
   tampering without guest cooperation.

4. **Live migration**: raw COW disk is simpler to migrate than qcow2
   (no backing-file chain to transfer).

5. **Warm start / checkpoint-restore**: overlay structure makes it easy to
   snapshot COW state and restore.

## References

- [Issue #61: RFC: squashfs block device for OCI VM rootfs](https://github.com/CMGS/cocoon/issues/61)
- [EROFS filesystem documentation](https://docs.kernel.org/filesystems/erofs.html)
- [Kata Containers architecture](https://github.com/kata-containers/kata-containers/blob/main/docs/design/architecture/README.md)
- [Cloud Hypervisor disk configuration](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/disks.md)
- [initramfs-tools hook documentation](https://manpages.ubuntu.com/manpages/noble/man8/initramfs-tools.8.html)
- [Linux overlayfs documentation](https://docs.kernel.org/filesystems/overlayfs.html)

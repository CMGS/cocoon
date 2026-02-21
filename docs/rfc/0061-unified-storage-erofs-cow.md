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
| Shutdown | ACPI power-button → guest systemd → SIGTERM | vm.shutdown API → SIGTERM → SIGKILL |
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
  --disk path=cache/layers/{sha_r}/rootfs.erofs,readonly=on,serial=cocoon-layer0 \
  --disk path=vms/{vmID}/cow.raw,serial=cocoon-cow \
  --cmdline "... boot=cocoon cocoon.layers=cocoon-layer0 cocoon.cow=cocoon-cow"
```

For OCI VM image (with user custom layer):
```
cloud-hypervisor \
  --kernel   cache/layers/{sha_k}/vmlinuz \
  --initramfs cache/layers/{sha_k}/initrd.img \
  --disk path=cache/layers/{sha_user}/custom.erofs,readonly=on,serial=cocoon-layer0 \
  --disk path=cache/layers/{sha_base}/rootfs.erofs,readonly=on,serial=cocoon-layer1 \
  --disk path=vms/{vmID}/cow.raw,serial=cocoon-cow \
  --cmdline "... boot=cocoon cocoon.layers=cocoon-layer0,cocoon-layer1 cocoon.cow=cocoon-cow"
```

**Layer ordering**: `cocoon.layers` follows overlayfs `lowerdir=` semantics.
Per the kernel documentation (`Documentation/filesystems/overlayfs.rst`):
*"Multiple lower layers can now be given as the mount option
`lowerdir=lower1:lower2:lower3`, where the rightmost lower layer is at the
bottom."* In other words, leftmost = highest priority (searched first for
file lookup). For OCI, the custom (user) layer comes first so it overrides
the base layer. Cocoon constructs the `--disk` flags
in the same order, and assigns deterministic serial IDs for stable guest
device identification.

**Layer count limit**: Cocoon caps the number of rootfs layers at **8**.
Images exceeding this limit are flattened into a single EROFS during
materialize. This avoids hitting the overlayfs lowerdir argument length
limit, keeps the kernel cmdline within safe bounds (the `cocoon.layers=`
parameter lives on the kernel cmdline, which has a finite maximum length
that varies by bootloader/kernel configuration), and reduces the number
of `--disk` entries passed to CH. The flattened EROFS gets its own
content-address (SHA-256 of the output file); original per-layer SHAs are
not stored or referenced. Refcount and GC operate on the flattened layer
as a single unit — no relationship to the original OCI layers is tracked.

Multiple VMs from the same image share kernel and rootfs layers (content-
addressed by SHA-256). Each VM only gets its own COW raw file.

**Layer hash definition** (what is hashed for content-addressing):
- Rootfs layer: `SHA-256(rootfs.erofs file bytes)`
- Kernel layer: `SHA-256(SHA-256(vmlinuz) || SHA-256(initrd.img))` —
  each file is hashed independently, then the two hashes are
  concatenated and hashed again. This avoids ambiguity from raw
  byte concatenation (where different split points could collide)
  and allows streaming computation. Any change to injected
  modules/hooks produces a new kernel layer hash.
- `meta.json` is **not** included in the hash — it is metadata for
  diagnostics and GC only. This prevents metadata format upgrades
  from invalidating cached layers.
- `cocoon-buildinfo.json` (injected into initrd) must **not** contain
  the current kernel layer SHA — that would create a circular
  dependency (hash depends on its own value). It also should **not**
  contain `rootfs_layer_shas` — embedding rootfs-specific data in the
  initrd would defeat kernel layer dedup across images sharing the
  same distro kernel. Rootfs layer SHAs belong in the host-side
  `meta.json` (not hashed, not inside initrd). The initrd buildinfo
  contains only kernel-scoped data: mkfs flags, mkfs version, kernel
  version, and kernel config check results.

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
3. No need for full `depmod` rebuild — we parse `modules.dep` selectively
4. Distro-specific quirks (device naming, module loading) are pre-handled

However, the original initramfs likely does NOT contain `erofs.ko` or
`overlay.ko` (since the original rootfs is ext4/xfs). These modules must
be extracted from the image's `/lib/modules/<ver>/` and injected.

**ext4 module**: the COW disk is ext4. On most Debian/Ubuntu images, `ext4`
is built-in (`CONFIG_EXT4_FS=y`) or already present in the initramfs (since
the original root is ext4). However, on **RHEL/CentOS images where root is
xfs**, the initramfs may only include the xfs module chain — `ext4`, `jbd2`,
and `mbcache` might be absent. Therefore, `ext4` (and its dependencies) must
be included in the dependency resolution chain alongside `erofs` and
`overlay`. If `ext4` is built-in (`modules.builtin`), injection is skipped.

**virtio driver chain**: the hook resolves devices via `/sys/block/vd*`,
which requires virtio-blk to be loaded. Standard cloudimg initramfs images
include `virtio_blk`, `virtio_pci`, `virtio_ring`, and `virtio` (since
the original root is also on virtio-blk). However, OCI VM images with
custom kernels may not. Therefore, these four modules are included in
the dependency resolution chain. As with other modules: if built-in, skip;
if absent from both `modules.builtin` and `modules.dep`, fail materialize.

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
  │   ├─ Resolve transitive deps for: erofs, overlay, ext4,
  │   │     virtio_blk, virtio_pci, virtio_ring, virtio
  │   │   (e.g., erofs → lz4_decompress, lz4hc_compress)
  │   ├─ Extract all .ko/.ko.xz/.ko.zst files in dependency chain
  │   │   (decompress .ko.xz/.ko.zst → .ko during extraction)
  │   └─ Generate load.order (topological sort of dependency chain)
  │       Layout: all .ko files are placed flat in /cocoon-modules/
  │       using their basename (e.g., erofs.ko, lz4_compress.ko).
  │       load.order lists basenames. Materialize fails if two modules
  │       in the dependency chain have the same basename (collision).
  │       **Ordering constraint**: load.order uses a full topological
  │       sort over `modules.dep` edges (symbol dependencies). When
  │       multiple modules have no dependency ordering between them
  │       (same topological level), ties are broken by stable
  │       lexicographic order of module basenames.
  │       No hard prefix reordering is applied — the dependency
  │       graph is the single source of truth. We only require that
  │       all modules are loaded before `resolve_disk` runs; virtio
  │       subsystem internal ordering (virtio → virtio_ring →
  │       virtio_pci → virtio_blk) is guaranteed by `modules.dep`
  │       edges, not by name ordering. `resolve_disk` still polls
  │       with timeout to handle device enumeration latency.
  │
  ├─ mkfs.erofs rootfs.erofs (from mount point, unmodified)
  │
  └─ Unmount
  │
  ├─ Patch initramfs:
  │   ├─ Unpack (cpio segments — see Pitfall 1 for multi-segment handling)
  │   ├─ Detect initramfs format (initramfs-tools vs dracut; unknown → fail)
  │   ├─ Tool availability gate: verify the unpacked initramfs contains
  │   │     the required external commands (insmod, mount, cat, mkdir,
  │   │     rm, ln, mknod, sleep, uname).
  │   │     Detection algorithm (PATH = /bin:/sbin:/usr/bin:/usr/sbin):
  │   │       For each tool, search PATH dirs inside the unpacked tree.
  │   │       Accept if the entry is a regular file with executable bit,
  │   │       or a symlink (e.g. `mount -> busybox` — do not attempt to
  │   │       resolve or execute the target, because the unpacked tree
  │   │       may be cross-architecture and not runnable on the host).
  │   │       If no entry found for any tool →
  │   │          PermanentError("initramfs too minimal: missing <tool>")
  │   ├─ Inject /cocoon-modules/*.ko + load.order
  │   ├─ Inject /cocoon-buildinfo.json (read-only, immutable build-time data):
  │   │     { mkfs_erofs_flags, mkfs_erofs_version,
  │   │       kernel_version, kernel_config_checks }
  │   │   Note: rootfs_layer_shas are stored in host-side meta.json
  │   │   (not inside initrd) to preserve kernel layer dedup.
  │   │   Runtime boot results (overlay_opts_effective, etc.) are written
  │   │   separately to /run/cocoon/boot.env at boot time (key=value format).
  │   │   cocoon_fatal() prints buildinfo + /proc state on fatal errors.
  │   ├─ Inject hook script(s) at distro-specific path:
  │   │   ├─ initramfs-tools: /scripts/cocoon (custom boot script)
  │   │   └─ dracut: /lib/dracut/hooks/cmdline/01-cocoon-cmdline.sh
  │   │            + /lib/dracut/hooks/mount/01-cocoon-mount.sh
  │   ├─ Verify: injected hooks have mode 0755, modules have mode 0644
  │   │   (post-injection check — fail materialize if permissions are wrong)
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
    return FormatUnknown // hard fail — do not guess
}
```

If `FormatUnknown` is returned, the materialize pipeline **must fail** with
a clear error. Silently falling back to initramfs-tools would produce an
initrd that builds successfully but fails to boot — the worst kind of bug.

| Distro Family | Format | Hook Path | Root Variable |
|---|---|---|---|
| Debian / Ubuntu | initramfs-tools | `/scripts/cocoon` (custom boot script) | `${rootmnt}` |
| RHEL / CentOS / Fedora | dracut | `/lib/dracut/hooks/cmdline/01-cocoon-cmdline.sh` + `/lib/dracut/hooks/mount/01-cocoon-mount.sh` | `$NEWROOT` |

#### Guest Boot Hook — initramfs-tools (Debian/Ubuntu)

Injected at `/scripts/cocoon` as a **custom boot script**. Cocoon adds
`boot=cocoon` to the kernel cmdline. The initramfs-tools `/init` script
(see [Debian source](https://sources.debian.org/src/initramfs-tools/0.140/init/))
sources `/scripts/${BOOT}` (where `BOOT` defaults to `local`), so with
`boot=cocoon` it sources `/scripts/cocoon` which defines our `mountroot()`.
The framework then calls `mountroot()` — this is the **official**
initramfs-tools mechanism for replacing the default root mount logic.
The same `/init` also performs `mount --move /run ${rootmnt}/run` during
`switch_root`, ensuring our `/run/cocoon/storage/` mounts survive.

**Why not `/scripts/local-bottom/`?** `local-bottom` runs *after* the
default root mount. If we place our hook there, initramfs-tools would
first try (and fail) to mount root via the default `local_mount_root()`
path, potentially panic before ever reaching our hook.

```sh
# /scripts/cocoon — custom boot script for initramfs-tools
# Sourced by /init when boot=cocoon is set on kernel cmdline.
# The framework calls mountroot() after sourcing this file.

. /scripts/functions

# Resolve a virtio-blk device by serial (stable across disk reordering).
#
# Initramfs runtime tool requirements (complete list):
#   Shell builtins only: case, while, for, local, echo, printf, read, return,
#     break, unset, [, :, parameter expansion (${var##*/}, ${var%%:*}, etc.)
#   External commands: cat, insmod, mount, mkdir, rm, ln, mknod, sleep, uname
# All of the above are provided by busybox (initramfs-tools) or dracut's
# minimal coreutils set. The hooks do NOT depend on: tr, sed, grep, awk,
# basename, ls, or any other text-processing tools.

# Polls /sys/block/vd* checking both possible sysfs serial paths,
# with configurable timeout (default 10s, override via cocoon.timeout=).
# Note: mknod fallback requires CAP_MKNOD (standard in initramfs root
# context). If a hardened initrd restricts this, devtmpfs/udev must
# provide /dev nodes — mknod failure is silently ignored (|| true
# guards against set -e environments).
resolve_disk() {
    local serial="$1"
    local timeout="${COCOON_TIMEOUT:-10}" i=0
    case "$timeout" in ''|*[!0-9]*) timeout=10 ;; esac
    while [ $i -lt $timeout ]; do
        for sysdev in /sys/block/vd*; do
            [ -d "$sysdev" ] || continue
            local s=""
            # Kernel exposes serial at different paths depending on version.
            # virtio-blk ID is 20 bytes (VIRTIO_BLK_ID_BYTES); sysfs may
            # pad with trailing spaces. We do not rely on NUL stripping
            # (sysfs text attributes typically do not expose raw NUL to
            # userspace); we only trim trailing whitespace.
            if [ -f "$sysdev/serial" ]; then
                s=$(cat "$sysdev/serial")
            elif [ -f "$sysdev/device/serial" ]; then
                s=$(cat "$sysdev/device/serial")
            fi
            # Trim trailing whitespace (pure POSIX shell, no external tools).
            while :; do
                case "$s" in
                    *[[:space:]]) s="${s%[[:space:]]}" ;;
                    *) break ;;
                esac
            done
            if [ "$s" = "$serial" ]; then
                local devname="/dev/${sysdev##*/}"
                # Fallback: if devtmpfs/udev hasn't created the node yet,
                # create it from /sys/block/vdX/dev (major:minor).
                if [ ! -e "$devname" ] && [ -f "$sysdev/dev" ]; then
                    local majmin=$(cat "$sysdev/dev")
                    mknod "$devname" b "${majmin%%:*}" "${majmin##*:}" 2>/dev/null || true
                fi
                echo "$devname"
                return 0
            fi
        done
        sleep 1
        i=$((i + 1))
    done
    return 1
}

mountroot() {
    log_begin_msg "Cocoon: mounting overlay rootfs"

    # Diagnostic helper: dump buildinfo + kernel state before fatal exit.
    # All diagnostic commands use || true to survive set -e environments.
    cocoon_fatal() {
        echo "COCOON FATAL: $1" >&2
        echo "--- buildinfo ---" >&2
        cat /cocoon-buildinfo.json >&2 2>/dev/null || true
        echo "--- kernel: $(uname -r) ---" >&2
        cat /proc/modules >&2 2>/dev/null || true
        echo "--- /proc/self/status ---" >&2
        cat /proc/self/status >&2 2>/dev/null || true
        echo "--- load.order ---" >&2
        cat /cocoon-modules/load.order >&2 2>/dev/null || echo "(no load.order)" >&2
        echo "--- sysfs block devices ---" >&2
        for _sb in /sys/block/vd*; do
            [ -d "$_sb" ] || continue
            _sd=""
            _sd=$(cat "$_sb/serial" 2>/dev/null || cat "$_sb/device/serial" 2>/dev/null) || true
            echo "$_sb: dev=$(cat "$_sb/dev" 2>/dev/null || true) serial=${_sd}" >&2
        done
        panic "$1"
    }

    # Load injected modules in dependency order.
    # insmod errors: "File exists" = already loaded (built-in or earlier hook),
    # harmless. Any other failure (bad format, missing symbol) is fatal —
    # a missing fs module means the overlay mount will fail later with a
    # cryptic error, so we fail early with a clear message.
    if [ -d /cocoon-modules ] && [ -f /cocoon-modules/load.order ]; then
        while read -r mod; do
            [ -e "/cocoon-modules/${mod}" ] || continue
            _err=$(insmod "/cocoon-modules/${mod}" 2>&1) || {
                case "$_err" in
                    *"File exists"*|*EEXIST*) ;;  # already loaded — harmless
                    *) cocoon_fatal "insmod ${mod} failed: ${_err}" ;;
                esac
            }
        done < /cocoon-modules/load.order
    fi

    # Parse topology from kernel cmdline (cocoon controls this).
    # Values are stable serial IDs, not device names.
    for x in $(cat /proc/cmdline); do
        case $x in
            cocoon.layers=*) LAYERS="${x#cocoon.layers=}" ;;
            cocoon.cow=*)    COW="${x#cocoon.cow=}" ;;
            cocoon.timeout=*) COCOON_TIMEOUT="${x#cocoon.timeout=}" ;;
        esac
    done

    [ -z "$LAYERS" ] && cocoon_fatal "cocoon.layers= not set"
    [ -z "$COW" ]    && cocoon_fatal "cocoon.cow= not set"

    # Mount under /run — initramfs-tools automatically does
    # "mount --move /run ${rootmnt}/run" during switch_root,
    # so our mounts survive the transition.
    COCOON="/run/cocoon/storage"
    mkdir -p "$COCOON" || cocoon_fatal "mkdir $COCOON failed"

    # Mount read-only layers (resolve serial → /dev/vdX)
    LOWER=""
    IFS=,
    for serial in $LAYERS; do
        dev=$(resolve_disk "$serial") || cocoon_fatal "device ${serial} not found"
        mnt="${COCOON}/layers/${serial}"
        mkdir -p "$mnt" || cocoon_fatal "mkdir ${mnt} failed"
        mount -t erofs -o ro "$dev" "$mnt" || cocoon_fatal "mount ${serial} failed"
        [ -n "$LOWER" ] && LOWER="${LOWER}:"
        LOWER="${LOWER}${mnt}"
    done
    unset IFS

    # Mount COW (pre-formatted by cocoon create on host)
    cow_dev=$(resolve_disk "$COW") || cocoon_fatal "COW device ${COW} not found"
    mkdir -p "${COCOON}/cow" || cocoon_fatal "mkdir ${COCOON}/cow failed"
    mount -t ext4 "$cow_dev" "${COCOON}/cow" || cocoon_fatal "mount COW failed"
    mkdir -p "${COCOON}/cow/upper" || cocoon_fatal "mkdir cow/upper failed"
    rm -rf "${COCOON}/cow/work" && mkdir -p "${COCOON}/cow/work" \
        || cocoon_fatal "prepare cow/work failed"

    # Assemble overlay.
    # Explicit options for predictable behavior across kernels:
    #   index=off      — no index dir (avoids NFS export issues, simpler)
    #   metacopy=off   — always full copy-up (no partial-copy bugs)
    #   redirect_dir=off — no redirect on rename (avoids xattr deps)
    # Two-stage: try with explicit options first; if EINVAL (option not
    # supported by this kernel), fall back to minimal options and log.
    OVL_OPTS="lowerdir=${LOWER},upperdir=${COCOON}/cow/upper,workdir=${COCOON}/cow/work"
    OVL_FULL="${OVL_OPTS},index=off,metacopy=off,redirect_dir=off"
    _ovl_mode="full"
    _ovl_err=$(mount -t overlay overlay -o "$OVL_FULL" "${rootmnt}" 2>&1) || {
        # Match known "unsupported option" error strings across mount
        # implementations (util-linux, busybox). Glob character classes
        # [Ii] etc. handle case variation in error messages.
        case "$_ovl_err" in
            *[Ii]nvalid\ argument*|*[Uu]nknown\ option*|*[Bb]ad\ option*)
                echo "cocoon: overlay explicit opts unsupported (err: ${_ovl_err}), falling back" >&2
                _ovl_fb_err=$(mount -t overlay overlay -o "$OVL_OPTS" "${rootmnt}" 2>&1) \
                    || cocoon_fatal "overlay mount failed (first: ${_ovl_err}) (fallback: ${_ovl_fb_err})"
                _ovl_mode="fallback"
                ;;
            *) cocoon_fatal "overlay mount failed: $_ovl_err" ;;
        esac
    }

    # Write runtime boot results to /run (separate from immutable buildinfo).
    # Acceptance tests and CI depend on this file — hard-fail if write fails.
    # Format: key=value env file (one per line, parse by splitting on first '=').
    # NOT JSON — avoids escaping issues with raw mount error messages that
    # may contain quotes, backslashes, newlines, or control characters.
    mkdir -p /run/cocoon || cocoon_fatal "mkdir /run/cocoon failed"
    {
        printf 'overlay_opts_effective=%s\n' "$_ovl_mode"
        [ -n "${_ovl_err:-}" ] && printf 'overlay_mount_first_error=%s\n' "$_ovl_err"
        [ -n "${_ovl_fb_err:-}" ] && printf 'overlay_mount_fallback_error=%s\n' "$_ovl_fb_err"
        true  # ensure block exits 0 even if both vars are empty
    } > /run/cocoon/boot.env \
        || cocoon_fatal "write /run/cocoon/boot.env failed"

    mkdir -p "${rootmnt}/dev" "${rootmnt}/proc" "${rootmnt}/sys" "${rootmnt}/run"

    # --- Systemd compatibility patching (written to COW upper, EROFS untouched) ---
    # Without these, systemd will hang or fail on boot. See "Systemd
    # Compatibility" section below for the full rationale.
    #
    # Note: this clears ALL fstab entries, not just root. Data disk mounts
    # must be handled separately by cocoon (e.g. injected systemd mount units).
    #
    # Every step is checked — failure here means the overlay upper layer
    # is not writable (broken workdir, fs feature mismatch, etc.), and
    # systemd would hang for 90s+ with a cryptic job timeout.
    [ -d "${rootmnt}/etc" ] || cocoon_fatal "/etc missing after overlay mount"
    # rm -f first: if /etc/fstab is a symlink (some images), : > would follow
    # it and modify the target. We want a regular file in the overlay upper.
    rm -f "${rootmnt}/etc/fstab" || cocoon_fatal "remove /etc/fstab failed"
    : > "${rootmnt}/etc/fstab" \
        || cocoon_fatal "create empty /etc/fstab failed (overlay upper not writable?)"
    mkdir -p "${rootmnt}/etc/systemd/system" \
        || cocoon_fatal "mkdir /etc/systemd/system failed"
    rm -f "${rootmnt}/etc/systemd/system/systemd-fsck-root.service" 2>/dev/null
    ln -sf /dev/null "${rootmnt}/etc/systemd/system/systemd-fsck-root.service" \
        || cocoon_fatal "mask systemd-fsck-root.service failed"
    rm -f "${rootmnt}/etc/systemd/system/systemd-remount-fs.service" 2>/dev/null
    ln -sf /dev/null "${rootmnt}/etc/systemd/system/systemd-remount-fs.service" \
        || cocoon_fatal "mask systemd-remount-fs.service failed"

    # VM identity isolation: ensure each VM gets a unique machine-id.
    # If the base image ships a non-empty machine-id, multiple VMs would
    # share it (breaks journald, dbus, systemd-networkd, licensing agents).
    # Truncating to empty triggers systemd-machine-id-setup on first boot.
    #
    # rm -f first: /etc/machine-id may be a symlink to
    # /var/lib/dbus/machine-id — writing through a symlink on overlayfs
    # has unpredictable copy-up behavior. Force a regular file in upper.
    rm -f "${rootmnt}/etc/machine-id" 2>/dev/null || true
    : > "${rootmnt}/etc/machine-id" || cocoon_fatal "create empty /etc/machine-id failed"
    # Also cover the dbus alias path (some distros symlink the other way).
    if [ -d "${rootmnt}/var/lib/dbus" ] || [ -L "${rootmnt}/var/lib/dbus/machine-id" ]; then
        mkdir -p "${rootmnt}/var/lib/dbus" 2>/dev/null || true
        rm -f "${rootmnt}/var/lib/dbus/machine-id" 2>/dev/null || true
        : > "${rootmnt}/var/lib/dbus/machine-id" 2>/dev/null || true
    fi

    log_success_msg "Cocoon: overlay rootfs ready"
}
```

#### Guest Boot Hook — dracut (RHEL/CentOS/Fedora)

Dracut requires **two** injected hooks. The `rootok=1` flag **must** be set
during the `cmdline` stage — if dracut reaches the end of cmdline processing
without `rootok=1`, it aborts before ever reaching mount hooks. This is
enforced in dracut's `init` script (see
[dracut-init.sh, `rootok` check](https://github.com/dracut-ng/dracut-ng/blob/main/modules.d/99base/init.sh)):
if no cmdline hook sets `rootok=1`, dracut prints a diagnostic and drops
to an emergency shell.

**Hook 1** — `/lib/dracut/hooks/cmdline/01-cocoon-cmdline.sh`:

```sh
#!/bin/sh
# Dracut hooks are sourced by init (not executed as subprocesses),
# so `return` is valid here. See dracut(8) §HOOKS.
#
# Tell dracut we will handle root mounting. Both `rootok` and `root`
# must be set — dracut halts at cmdline stage if either is empty.
#
# We use a synthetic "cocoon:" scheme intentionally — no standard dracut
# module recognizes it, so nothing tries to mount root before our mount
# hook. Using a real device path (e.g. /dev/disk/by-id/virtio-*) would
# risk a race with dracut's standard root-finding modules.
#
# IMPORTANT: If a future distro adds a dracut module that parses the
# "cocoon:" scheme, it could pre-empt our mount hook. This scheme is
# private to Cocoon — no third-party module should attempt to resolve it.

# Only activate if cocoon.layers is present and non-empty on cmdline.
_layers=""
_layers_seen=0
for x in $(cat /proc/cmdline); do
    case $x in cocoon.layers=*) _layers="${x#cocoon.layers=}"; _layers_seen=1 ;; esac
done
[ "$_layers_seen" = "0" ] && return 0
if [ -z "$_layers" ]; then
    echo "cocoon: error: cocoon.layers= present but empty" >&2
    # Do not set rootok — dracut will halt at cmdline stage.
    return 1
fi

_base="${_layers##*,}"
root="cocoon:${_base}"
rootok=1
fstype="overlay"
rflags="rw"
export root rootok fstype rflags
```

**Hook 2** — `/lib/dracut/hooks/mount/01-cocoon-mount.sh`:

```sh
#!/bin/sh
# /lib/dracut/hooks/mount/01-cocoon-mount.sh
# Cocoon overlay rootfs mount handler for dracut-based initramfs.

. /lib/dracut-lib.sh   # provides die(), getarg(), etc.

# Resolve a virtio-blk device by serial (same logic as initramfs-tools hook).
resolve_disk() {
    local serial="$1"
    local timeout="${COCOON_TIMEOUT:-10}" i=0
    case "$timeout" in ''|*[!0-9]*) timeout=10 ;; esac
    while [ $i -lt $timeout ]; do
        for sysdev in /sys/block/vd*; do
            [ -d "$sysdev" ] || continue
            local s=""
            if [ -f "$sysdev/serial" ]; then
                s=$(cat "$sysdev/serial")
            elif [ -f "$sysdev/device/serial" ]; then
                s=$(cat "$sysdev/device/serial")
            fi
            while :; do
                case "$s" in
                    *[[:space:]]) s="${s%[[:space:]]}" ;;
                    *) break ;;
                esac
            done
            if [ "$s" = "$serial" ]; then
                local devname="/dev/${sysdev##*/}"
                if [ ! -e "$devname" ] && [ -f "$sysdev/dev" ]; then
                    local majmin=$(cat "$sysdev/dev")
                    mknod "$devname" b "${majmin%%:*}" "${majmin##*:}" 2>/dev/null || true
                fi
                echo "$devname"
                return 0
            fi
        done
        sleep 1
        i=$((i + 1))
    done
    return 1
}

# Diagnostic helper: dump buildinfo + kernel state before fatal exit.
# All diagnostic commands use || true to survive set -e environments.
cocoon_fatal() {
    echo "COCOON FATAL: $1" >&2
    echo "--- buildinfo ---" >&2
    cat /cocoon-buildinfo.json >&2 2>/dev/null || true
    echo "--- kernel: $(uname -r) ---" >&2
    cat /proc/modules >&2 2>/dev/null || true
    echo "--- /proc/self/status ---" >&2
    cat /proc/self/status >&2 2>/dev/null || true
    echo "--- load.order ---" >&2
    cat /cocoon-modules/load.order >&2 2>/dev/null || echo "(no load.order)" >&2
    echo "--- sysfs block devices ---" >&2
    for _sb in /sys/block/vd*; do
        [ -d "$_sb" ] || continue
        _sd=""
        _sd=$(cat "$_sb/serial" 2>/dev/null || cat "$_sb/device/serial" 2>/dev/null) || true
        echo "$_sb: dev=$(cat "$_sb/dev" 2>/dev/null || true) serial=${_sd}" >&2
    done
    die "$1"
}

# Load injected modules in dependency order (see initramfs-tools hook for rationale).
if [ -d /cocoon-modules ] && [ -f /cocoon-modules/load.order ]; then
    while read -r mod; do
        [ -e "/cocoon-modules/${mod}" ] || continue
        _err=$(insmod "/cocoon-modules/${mod}" 2>&1) || {
            case "$_err" in
                *"File exists"*|*EEXIST*) ;;  # already loaded — harmless
                *) cocoon_fatal "cocoon: insmod ${mod} failed: ${_err}" ;;
            esac
        }
    done < /cocoon-modules/load.order
fi

# Parse topology from kernel cmdline.
for x in $(cat /proc/cmdline); do
    case $x in
        cocoon.layers=*) LAYERS="${x#cocoon.layers=}" ;;
        cocoon.cow=*)    COW="${x#cocoon.cow=}" ;;
        cocoon.timeout=*) COCOON_TIMEOUT="${x#cocoon.timeout=}" ;;
    esac
done

if [ -z "$LAYERS" ] || [ -z "$COW" ]; then
    cocoon_fatal "cocoon: cocoon.layers= or cocoon.cow= not set on cmdline"
fi

# Mount under /run — dracut automatically moves /run during switch_root.
COCOON="/run/cocoon/storage"
mkdir -p "$COCOON" || cocoon_fatal "cocoon: mkdir $COCOON failed"

# Mount read-only layers (resolve serial → /dev/vdX)
LOWER=""
IFS=,
for serial in $LAYERS; do
    dev=$(resolve_disk "$serial") || cocoon_fatal "cocoon: device ${serial} not found"
    mnt="${COCOON}/layers/${serial}"
    mkdir -p "$mnt" || cocoon_fatal "cocoon: mkdir ${mnt} failed"
    mount -t erofs -o ro "$dev" "$mnt" || cocoon_fatal "cocoon: mount ${serial} failed"
    [ -n "$LOWER" ] && LOWER="${LOWER}:"
    LOWER="${LOWER}${mnt}"
done
unset IFS

# Mount COW (pre-formatted by cocoon create on host)
cow_dev=$(resolve_disk "$COW") || cocoon_fatal "cocoon: COW device ${COW} not found"
mkdir -p "${COCOON}/cow" || cocoon_fatal "cocoon: mkdir ${COCOON}/cow failed"
mount -t ext4 "$cow_dev" "${COCOON}/cow" || cocoon_fatal "cocoon: mount COW failed"
mkdir -p "${COCOON}/cow/upper" || cocoon_fatal "cocoon: mkdir cow/upper failed"
rm -rf "${COCOON}/cow/work" && mkdir -p "${COCOON}/cow/work" \
    || cocoon_fatal "cocoon: prepare cow/work failed"

# Assemble overlay (same EINVAL-only fallback as initramfs-tools hook).
OVL_OPTS="lowerdir=${LOWER},upperdir=${COCOON}/cow/upper,workdir=${COCOON}/cow/work"
OVL_FULL="${OVL_OPTS},index=off,metacopy=off,redirect_dir=off"
_ovl_mode="full"
_ovl_err=$(mount -t overlay overlay -o "$OVL_FULL" "$NEWROOT" 2>&1) || {
    case "$_ovl_err" in
        *[Ii]nvalid\ argument*|*[Uu]nknown\ option*|*[Bb]ad\ option*)
            echo "cocoon: overlay explicit opts unsupported (err: ${_ovl_err}), falling back" >&2
            _ovl_fb_err=$(mount -t overlay overlay -o "$OVL_OPTS" "$NEWROOT" 2>&1) \
                || cocoon_fatal "cocoon: overlay mount failed (first: ${_ovl_err}) (fallback: ${_ovl_fb_err})"
            _ovl_mode="fallback"
            ;;
        *) cocoon_fatal "cocoon: overlay mount failed: $_ovl_err" ;;
    esac
}

# Write runtime boot results to /run (separate from immutable buildinfo).
# Format: key=value env file (see initramfs-tools hook for rationale).
mkdir -p /run/cocoon || cocoon_fatal "cocoon: mkdir /run/cocoon failed"
{
    printf 'overlay_opts_effective=%s\n' "$_ovl_mode"
    [ -n "${_ovl_err:-}" ] && printf 'overlay_mount_first_error=%s\n' "$_ovl_err"
    [ -n "${_ovl_fb_err:-}" ] && printf 'overlay_mount_fallback_error=%s\n' "$_ovl_fb_err"
    true
} > /run/cocoon/boot.env \
    || cocoon_fatal "cocoon: write /run/cocoon/boot.env failed"

mkdir -p "$NEWROOT/dev" "$NEWROOT/proc" "$NEWROOT/sys" "$NEWROOT/run"

# --- Systemd compatibility patching ---
# Every step is checked — see initramfs-tools hook for full rationale.
[ -d "$NEWROOT/etc" ] || cocoon_fatal "cocoon: /etc missing after overlay mount"
rm -f "$NEWROOT/etc/fstab" || cocoon_fatal "cocoon: remove /etc/fstab failed"
: > "$NEWROOT/etc/fstab" \
    || cocoon_fatal "cocoon: create empty /etc/fstab failed (overlay upper not writable?)"
mkdir -p "$NEWROOT/etc/systemd/system" \
    || cocoon_fatal "cocoon: mkdir /etc/systemd/system failed"
rm -f "$NEWROOT/etc/systemd/system/systemd-fsck-root.service" 2>/dev/null
ln -sf /dev/null "$NEWROOT/etc/systemd/system/systemd-fsck-root.service" \
    || cocoon_fatal "cocoon: mask systemd-fsck-root.service failed"
rm -f "$NEWROOT/etc/systemd/system/systemd-remount-fs.service" 2>/dev/null
ln -sf /dev/null "$NEWROOT/etc/systemd/system/systemd-remount-fs.service" \
    || cocoon_fatal "cocoon: mask systemd-remount-fs.service failed"

# VM identity isolation (see initramfs-tools hook for rationale).
# rm -f first: /etc/machine-id may be a symlink — force regular file in upper.
rm -f "$NEWROOT/etc/machine-id" 2>/dev/null || true
: > "$NEWROOT/etc/machine-id" || cocoon_fatal "cocoon: create empty /etc/machine-id failed"
if [ -d "$NEWROOT/var/lib/dbus" ] || [ -L "$NEWROOT/var/lib/dbus/machine-id" ]; then
    mkdir -p "$NEWROOT/var/lib/dbus" 2>/dev/null || true
    rm -f "$NEWROOT/var/lib/dbus/machine-id" 2>/dev/null || true
    : > "$NEWROOT/var/lib/dbus/machine-id" 2>/dev/null || true
fi
```

Key differences from initramfs-tools:
- Uses `$NEWROOT` instead of `${rootmnt}`
- Requires **two hooks**: cmdline hook sets `rootok=1` (must happen before
  mount stage), mount hook does the actual overlay assembly
- Uses `die` instead of `panic` for fatal errors
- No `mountroot()` function wrapper — dracut hooks execute as plain scripts
- No `boot=cocoon` cmdline needed — dracut uses hook directories, not boot scripts

#### Overlay Mount Options

Both hooks attempt `mount -t overlay` with explicit options
`index=off,metacopy=off,redirect_dir=off`. This is a **predictability-
over-performance** trade-off:

- `index=off` disables the overlay index directory. Slightly reduces
  NFS export correctness and `nlink` accuracy, but avoids index
  incompatibilities across kernel versions.
- `metacopy=off` forces full copy-up (entire file, not just metadata).
  Prevents partial-copy bugs at the cost of more I/O on first write.
- `redirect_dir=off` disables directory rename redirection. Simplifies
  semantics and avoids `trusted.overlay.redirect` xattr dependency.

If the kernel rejects these options (`EINVAL`), the hooks fall back to
default overlay options and log a warning. This ensures bootability on
kernels that don't support all options, while preserving diagnostics
about which options were actually in effect.

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

**Known limitation**: clearing fstab removes ALL mount entries, not just
the root entry. This means any non-root mounts declared in the original
image's fstab (swap, data partitions, NFS) will be silently disabled. If
Cocoon adds data disk support in the future, those mounts must be injected
via **systemd `.mount` units or a systemd generator** (not by restoring
fstab entries — fstab cannot express `root=overlay` correctly). Cocoon
would generate mount units at create time and inject them into the COW
upper layer.

#### `/run` Survival Across `switch_root`

Both hooks mount layer devices under `/run/cocoon/storage/`. These mounts
survive `switch_root` because both initramfs-tools and dracut perform
`mount --move /run ${rootmnt}/run` (or equivalent) as part of their
standard `switch_root` sequence. This is a documented behavior of both
frameworks — not an assumption.

If a future exotic initramfs framework does not move `/run`, the mounts
would be lost. The hook scripts would need to explicitly `mount --move`
the cocoon mount points into the new root before `switch_root`. This is
tracked as a known dependency on the initramfs framework contract.

**Post-boot self-check (Phase 1 gate)**: the implementation **must** inject
a systemd oneshot service (in the COW upper) that verifies
`/run/cocoon/storage/layers/*` are still mountpoints after boot. If any
expected mountpoint is missing, the service logs a fatal diagnostic via
`systemd-cat` and triggers `systemctl isolate emergency.target`. This
converts a silent mount loss into an observable, actionable failure.

The check is gated by the kernel cmdline parameter `cocoon.selfcheck=`
(default `1`). Setting `cocoon.selfcheck=0` disables it. CI and debug
profiles should always leave it enabled. Without this check, a non-standard
initramfs framework that silently drops `/run` contents would produce a VM
that appears to boot but has missing layer mounts — an extremely hard-to-
diagnose failure mode.

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

Cocoon controls both the CH disk config and the kernel cmdline. Each
virtio-blk disk is assigned a **stable serial ID** via CH's `serial`
field. The boot hook resolves serial → `/dev/vdX` by scanning
`/sys/block/vd*/serial` and `/sys/block/vd*/device/serial` (both paths
are checked — kernel version determines which is populated), with a
timeout-based wait for device readiness.

Two custom cmdline parameters declare the topology:

- `cocoon.layers=cocoon-layer0,cocoon-layer1` — ordered list of EROFS
  layer serial IDs (leftmost = highest priority in overlayfs lowerdir)
- `cocoon.cow=cocoon-cow` — the writable COW device serial ID

In addition, `boot=cocoon` is set on the kernel cmdline. This is
**only used by initramfs-tools** (tells `/init` to source `/scripts/cocoon`
instead of `/scripts/local`). Dracut ignores unknown cmdline parameters,
so `boot=cocoon` is harmless on dracut-based images. Cocoon injects it
unconditionally to avoid needing to know the initramfs format at boot time.

Optional parameters:
- `cocoon.timeout=N` — device wait timeout in seconds (default: 10).
  Useful for slow hardware or heavily loaded hosts.

**Hard constraint: virtio-blk only.** Cocoon **chooses** virtio-blk as
the sole supported backend for rootfs layers and the COW disk, to
guarantee stable serial→device mapping. All rootfs layers and the COW
disk **must** be attached as **virtio-blk** devices (`--disk` /
`DiskConfig`). The initramfs hook scans `/sys/block/vd*` exclusively.
virtio-scsi (`sdX`) and NVMe (`nvmeXnY`) are **not supported** for
rootfs/COW — the hook will not discover them. Other backends are
outside this RFC's scope; future data disk support may use different
backends independently of the rootfs/COW path.

**Why serial IDs instead of `/dev/vdX` names?** Device letter assignment
(`vda`, `vdb`, ...) depends on `--disk` flag ordering and can shift when
disks are added, removed, or reordered (e.g. adding a data disk). Serial
IDs are set by Cocoon and are stable regardless of disk ordering.

**CH serial requirements**:
- Cloud Hypervisor supports `serial` on disk configurations (virtio-blk).
  Requires CH >= v35 (serial field added in
  [cloud-hypervisor/cloud-hypervisor#5765](https://github.com/cloud-hypervisor/cloud-hypervisor/pull/5765),
  released in v35.0). Cocoon verifies CH version at launch; if the version
  does not support `serial`, Cocoon **fails fast** with a clear error
  asking to upgrade CH. There is no fallback to positional device naming.
- Serial length: **max 20 bytes** (not characters — but since Cocoon
  restricts serials to ASCII, bytes = characters). This is a virtio-blk
  protocol constraint (the `VIRTIO_BLK_ID_BYTES` field in `GET_ID` is
  fixed at 20 bytes per the virtio spec §5.2.6.1). Cocoon's naming
  convention (`cocoon-layer0` = 14 bytes, `cocoon-cow` = 10 bytes)
  stays well within this. Cocoon validates `len(serial) <= 20` before
  launch and fails fast if exceeded.
- Character set: `[A-Za-z0-9_-]` only. No spaces, NUL bytes, or special
  characters. Because the host-side serial is restricted to this charset
  and is shorter than 20 bytes, the guest-side sysfs value will not
  contain embedded NUL or space padding — the trailing-whitespace trim
  in `resolve_disk` is a defensive measure only, not a functional
  requirement for Cocoon-generated serials.
  Serials exceeding the limit are **rejected** (not truncated) — silent
  truncation would cause guest-visible ID to differ from the expected value.
- **Interface**: Cocoon configures disks via the CH **REST API** (`PUT
  /api/v1/vm.create` with `DiskConfig` JSON), not CLI flags. This avoids
  CLI syntax drift across CH versions (e.g. v36 changed `--disk` syntax).
  The `serial` field is part of `DiskConfig`. RFC examples show `--disk`
  for readability, but the implementation uses the API exclusively.

**Why not blkid/filesystem-type probing?**
- blkid may not exist in all initramfs environments
- Heuristic detection of COW disk is fragile (unformatted disks, data volumes)
- Device enumeration timing is non-deterministic in edge cases

### Cloudimg Materialize Pipeline

```
cloudimg.img (download — may be qcow2 or raw)
  → Detect format (qcow2 magic "QFI\xfb" at offset 0)
  → If qcow2: qemu-img convert -f qcow2 -O raw → cloudimg.raw
  → Parse GPT in Go → select rootfs partition → compute offset
      Selection rule:
        1. Filter out partitions by GPT type GUID:
           - ESP: C12A7328-F81F-11D2-BA4B-00A0C93EC93B
           - BIOS boot: 21686148-6449-6E6F-744E-656564454649
        2. Candidate set (GUID comparison is case-insensitive):
           - Linux filesystem: 0FC63DAF-8483-4772-8E79-3D69D8477DE4
           - Linux root (x86-64): 4F68BCE3-E8CD-4DB1-96E7-FBCAF984B709
           - Linux root (ARM-64): B921B045-1DF0-41C3-AF44-4C6F280D3FAE
           (systemd discoverable partitions spec — many distro cloudimgs
           use arch-specific root GUIDs instead of the generic one)
        3. Sort candidates by size descending, try each:
           a. mount -o ro,loop,offset=<N> (requires root)
           b. Check /etc/os-release exists → accept this partition
           c. If missing, unmount and try next candidate
        4. No valid rootfs found → PermanentError listing all
           candidate partitions with: index, type GUID, start LBA,
           size, and per-partition failure reason (mount error /
           /etc/os-release not found)
        Non-GPT, LVM, LUKS → PermanentError with descriptive message
  → Extract: vmlinuz, initrd.img, kernel modules (erofs/overlay dep chain)
      Note: assumes /boot is inside the rootfs partition (not a separate
      partition). If /boot is empty or missing after mounting the rootfs,
      materialize fails with PermanentError listing the mounted partition
      contents. Separate /boot partition support is out of scope.
  → mkfs.erofs rootfs.erofs (from mount point, no guest modification)
  → Unmount
  → Patch initramfs: unpack → detect format → inject modules + hook → repack
  → SHA-256 hash kernel layer and rootfs layer
  → Store in cache/layers/
  → Delete intermediate raw file
```

**Note**: Ubuntu cloudimgs (e.g. `*-cloudimg-amd64.img`) are typically
**qcow2**, not raw. The `qemu-img convert` step is required for these.
Raw images (e.g. some minimal cloud images) skip the conversion.

Tools needed:
- `qemu-img` (qemu-utils package — only for qcow2→raw conversion)
- `mount` (cocoon runs as root — loop + offset mount is always available)
- `mkfs.erofs` (erofs-utils package)
- Go for GPT parsing; **third-party libraries** required:
  - cpio: Go stdlib has no `archive/cpio` — use e.g. `github.com/cavaliergopher/cpio`
    or implement newc format reader/writer
  - zstd: `github.com/klauspost/compress/zstd` (initramfs segments, `.ko.zst` modules)
  - xz: `github.com/ulikunitz/xz` (initramfs segments, `.ko.xz` modules)
  - gzip: Go stdlib `compress/gzip` (sufficient)

### OCI Image Materialize Pipeline

```
OCI image layers (pulled or built)
  → Flatten all rootfs layers to temp directory
      (apply OCI whiteout semantics during flatten — see below)
      **Unpack fidelity constraint**: layer extraction MUST preserve:
        - xattrs (security.capability, trusted.*, security.selinux)
        - file capabilities (stored as security.capability xattr)
        - hardlinks (inode sharing, not copy)
        - device nodes (char/block)
        - uid/gid/mode (mtime is NOT preserved — see tar stream determinism below)
      Go's stdlib archive/tar does NOT handle this correctly:
      it exposes PAXRecords but does not setxattr, and SCHILY.xattr.*
      capability encoding is binary (not trivial to reimplement).
      Implementation MUST reuse a mature OCI layer applier (e.g.
      containerd's archive/apply, or buildkit's snapshot apply)
      rather than hand-rolling tar extraction. Rolling your own
      xattr/capability handling is a high-risk path that produces
      the worst failure mode: images that boot but have subtly
      wrong permissions, broken capabilities, or missing security
      labels. Materialize must verify xattr preservation after
      extraction (spot-check security.capability on known binaries)
      and fail if xattrs were silently dropped.
  → Extract: vmlinuz, initrd.img, kernel modules
  → mkfs.erofs rootfs.erofs (from flattened rootfs)
  → Patch initramfs (same as cloudimg — auto-detect format)
  → SHA-256 hash layers
  → Store in cache/layers/
```

For OCI images with distinct user layers:
```
  → Base layers → flatten (consume whiteouts: delete targeted files) → base.erofs
  → User layers → flatten among themselves (consume intra-user whiteouts),
      then convert remaining .wh.* to native overlayfs whiteouts
      (char dev 0,0 / opaque xattr) for cross-layer deletions → custom.erofs
  → Both passed as separate vdX devices
```

The distinction matters: when user layers delete files that exist only in
the base, those deletions must be preserved as native overlayfs tombstones
in `custom.erofs`, so overlayfs hides them from the base lowerdir.

#### OCI Whiteout Semantics

OCI layers use `.wh.filename` marker files and `.wh..wh..opq` opaque
directory markers to represent deletions. These are **OCI-specific** and
**not understood by Linux overlayfs** (which uses character device `(0,0)`
for whiteouts and the `trusted.overlay.opaque=y` xattr for opaque dirs).

When flattening layers into a single directory for EROFS:
- `.wh.filename` → delete `filename` from the accumulated output
- `.wh..wh..opq` → delete all prior contents of that directory

When keeping layers separate (multi-EROFS lowerdir):
- Convert `.wh.filename` → character device `(0,0)` named `filename`
- Convert `.wh..wh..opq` → set `trusted.overlay.opaque=y` xattr on dir
- This allows overlayfs to process the deletions natively

**Host requirements for whiteout conversion**:
- Creating character devices requires `CAP_MKNOD` (cocoon runs as root).
- Writing `trusted.overlay.opaque=y` xattr requires `CAP_SYS_ADMIN`
  (cocoon runs as root). Additionally, the build workspace filesystem
  **must support `trusted.*` xattrs** (ext4/xfs/btrfs — yes; tmpfs — no;
  overlayfs on overlayfs — no). If running inside a restricted container
  without `CAP_SYS_ADMIN`, xattr writes will fail silently and opaque
  dir semantics will break.
- **Materialize-time xattr probe** (required before multi-layer
  conversion): create a temp directory in the build workspace, set
  `trusted.overlay.opaque=y` xattr on it (Go: `unix.Setxattr`),
  read it back and verify the value. If the probe fails, emit
  PermanentError including: workspace filesystem type (`statfs`),
  effective capabilities (`/proc/self/status` CapEff), and the
  specific xattr error. This catches "silent opaque failure" before
  any EROFS is produced.
- The `mkfs.erofs` tool preserves device nodes and xattrs.

**Kernel dependency**: cross-layer deletion in multi-EROFS lowerdir relies
on the Linux kernel's overlayfs implementation of whiteout (`c 0 0`) and
opaque (`trusted.overlay.opaque=y`) semantics. This is the standard
overlayfs contract documented in `Documentation/filesystems/overlayfs.rst`
and is stable across all supported kernel versions (4.x+).

### Per-VM COW Disk Management

COW disks are raw ext4 files, created at `cocoon create` time:

```go
// In create stage (host-side)
func createCOWDisk(path string, size int64) error {
    // Sparse file — instant creation, no space allocation until written
    f, _ := os.Create(path)
    f.Truncate(size)
    f.Close()
    // Format ext4 with conservative features (host-side, not guest-side).
    // Modern e2fsprogs enables features (metadata_csum_seed, orphan_file)
    // that older guest kernels reject at mount time. Pin to ext4 baseline
    // features to ensure compatibility across all supported guest kernels.
    return exec.Command("mkfs.ext4", "-F", "-m", "0", "-q",
        "-O", "^metadata_csum_seed,^orphan_file",
        path).Run()
}
// Cocoon records `mkfs.ext4` version in COW meta for diagnostics
// (same pattern as mkfs.erofs version in layer meta.json).
// If future e2fsprogs versions add new incompatible default features,
// the version record enables diagnosis and the acceptance gate
// ("COW ext4 mounts on oldest supported guest kernel") catches it.
//
// Host e2fsprogs compatibility: older e2fsprogs may not recognize
// newer feature names (e.g. `orphan_file` added in e2fsprogs 1.47).
// Strategy: try with full disable list first; if mkfs.ext4 fails and
// stderr contains "invalid/unknown feature", strip the unrecognized
// feature(s) and retry (max 2 retries). If the host e2fsprogs is too
// old for any safe feature set, fail with PermanentError asking to
// upgrade. This is simpler and more robust than parsing help output.

```

Resize: `truncate -s +10G cow.raw` on host, then `resize2fs` on the COW
device in guest (identified by serial: scan `/sys/block/vd*/serial` and
`/sys/block/vd*/device/serial` for `"cocoon-cow"`).

**COW health recovery**: after an unclean VM shutdown (SIGKILL, host crash),
the ext4 journal should handle recovery automatically on next mount. If the
journal is corrupted, Cocoon runs `e2fsck -y cow.raw` on the host before
starting the VM. This is a host-side operation (no guest cooperation needed).
The boot hook in initramfs does NOT run fsck — it mounts directly with
`mount -t ext4`, relying on the journal or host-side pre-check.

**Concurrency safety**: `e2fsck` runs only during `cocoon start`, after
verifying the VM is in CREATED or STOPPED state (not running). The VM
metadata lock (`{vmID}-meta.lock`) serializes start operations, preventing
concurrent `e2fsck` + CH launch on the same COW file.

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

`layers.json` uses the same persistence pattern as existing Cocoon state
files: flock-protected, atomic write (write to temp → fsync → rename),
crash-safe. The `layers.lock` file protects concurrent access. This is
the same pattern used by `jsonstore.Store[T]` (see `lock/jsonstore/`).

### Code Impact

| Module | Change |
|---|---|
| `image/pipeline/` | **Rewrite** — new materialize: extract kernel + mkfs.erofs + patch initramfs |
| `storage/local/` | **Rewrite** — COW from qcow2 to raw; refcount from base-image to layer |
| `vm/engine/manager.go` | **Simplify** — single direct boot path, no UEFI, no virtiofsd |
| `vm/engine/create.go` | **Simplify** — create COW raw, pin layers, no overlay mount |
| `oci/` | **Adapt** — layer extraction produces erofs instead of unpacked dirs |
| `hypervisor/` | **Simplify** — remove UEFI firmware paths, unify to single shutdown path, remove virtiofsd management |
| `config/` | **Update** — new path helpers for layer store |

**Removed entirely**:
- `vm/engine/overlay_runtime_linux.go` / `overlay_runtime_other.go`
- `vm/engine/virtiofsd_*.go`
- UEFI firmware lookup and dual shutdown path branching

**Note on ACPI**: removing UEFI firmware does **not** remove ACPI. Cloud
Hypervisor generates ACPI tables (including power button) for direct-boot
VMs independently of firmware. The unified shutdown sequence is:

1. `PUT /api/v1/vm.power-button` — injects ACPI power button event; guest
   systemd performs orderly shutdown (unmount filesystems, sync disks).
2. Wait for CH process exit (with configurable timeout).
3. Fallback: `PUT /api/v1/vm.shutdown` — VMM-level forced stop of vCPUs
   and backends (no guest cooperation).
4. Last resort: `SIGTERM` → `SIGKILL` the CH process.

This is the **same graceful shutdown mechanism** as the current UEFI boot
path — ACPI is retained. What is removed is only the UEFI firmware binary
(`CLOUDHV.fd`) and the separate `Shutdown` vs `ShutdownDirect` code paths.

**Note**: `image/pipeline/convert_linux.go` (qcow2 conversion) is
**retained** — it changes from qcow2→qcow2 base to qcow2→raw (for
cloudimg input), using the same `qemu-img convert` tool.

### Implementation Pitfalls

Low-level hazards that will bite during Go implementation of the
materialize pipeline. Documented here so implementors don't rediscover
them the hard way.

#### Pitfall 1: Concatenated initramfs (multi-segment)

Many distros ship `initrd.img` as a **concatenated file** with multiple
cpio segments, not a single compressed archive. The most common layout:

```
┌──────────────────────────────┬─────────────────────────────────┐
│ Segment 1: uncompressed cpio │ Segment 2: gzip/zstd-compressed │
│ (CPU microcode:              │ cpio (the real initramfs)        │
│  AuthenticAMD.bin or         │                                  │
│  GenuineIntel.bin)           │                                  │
└──────────────────────────────┴─────────────────────────────────┘
```

However, some images may have **more than two segments** (e.g. firmware
blobs in a third uncompressed cpio). The Go unpacker must handle the
general case.

Correct approach:

1. Open the file as a raw `io.Reader`.
2. **Loop**: detect the next segment type by probing magic bytes:
   - `070701` or `070702` (newc cpio) → uncompressed cpio segment
   - `1f 8b` → gzip-compressed cpio
   - `28 b5 2f fd` → zstd-compressed cpio
   - `fd 37 7a 58 5a` → xz-compressed cpio
3. Read/decompress each segment. Detect segment end by cpio trailer
   record (`TRAILER!!!`) followed by padding.
4. The **last compressed segment** is the real initramfs. Earlier
   uncompressed segments (microcode, firmware) are preambles.
5. Unpack the real initramfs segment, inject modules + hook, repack.
6. **Reassemble**: concatenate all preamble segments (preserved verbatim)
   + recompressed main segment. The kernel bootloader expects this layout.

**Hard rule — preserve compression algorithm**: the main segment MUST be
recompressed with the **same algorithm** as the original (gzip→gzip,
zstd→zstd, xz→xz, uncompressed→uncompressed). The kernel only supports
decompressing algorithms enabled at build time (`CONFIG_RD_GZIP`,
`CONFIG_RD_ZSTD`, `CONFIG_RD_XZ`, etc.). Since the original initrd boots,
its algorithm is known-supported; switching to a different algorithm risks
the kernel failing to decompress the initramfs entirely (before any hook
code runs). Using gzip as a "universal output" is explicitly **rejected**.

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

#### Pitfall 3: Compressed kernel modules (`.ko.xz`, `.ko.zst`)

Many distros ship kernel modules in compressed form (`.ko.xz` for
Debian/Ubuntu, `.ko.zst` for Fedora/RHEL). The `modules.dep` file
references paths without the compression extension, but the actual files
on disk have it.

During materialize, the Go code must:
1. When scanning `modules.dep`, look for the `.ko` path but find the
   file as `.ko.xz`, `.ko.zst`, or `.ko.gz` on disk.
2. **Decompress** to plain `.ko` before injecting into the initramfs.
   `insmod` in the boot hook does not handle compressed modules.
3. The `load.order` file should list plain `.ko` filenames.

#### Pitfall 4: CPIO repack must preserve permissions and ownership

Go stdlib has no `archive/cpio` package. Use a third-party library (e.g.
`github.com/cavaliergopher/cpio`) or implement newc cpio format directly.
The chosen implementation must support: newc format, hardlinks, device
nodes (`c 0 0` for whiteouts), uid/gid/mode preservation, and
deterministic directory traversal order (see Pitfall 6).
When repacking the initramfs:

- Every injected file's `cpio.Header` must have correct `Mode`, `Uid`,
  and `Gid`. In particular, the hook script (`/scripts/cocoon`
  or `/lib/dracut/hooks/mount/01-cocoon-mount.sh`) **must be mode `0755`**.
  If the executable bit is missing, the initramfs framework silently skips
  it and the overlay never gets mounted.
- Injected `.ko` module files should be mode `0644`, owned by `root:root`.
- When repacking existing files from the original initramfs, copy the
  original `cpio.Header` fields verbatim — do not let Go defaults zero
  out the permission bits.

#### Pitfall 5: xattrs and SELinux labels in CPIO

Some initramfs files (especially on RHEL/SELinux-enforcing systems) carry
extended attributes and SELinux security labels. Go's standard `archive`
packages do not handle xattrs. Repacking will silently drop them.

**Current stance**: xattr preservation is best-effort. RHEL/SELinux
enforcing images may require additional implementation work (e.g. using
the newc cpio format with xattr extensions). This is acceptable for the
initial implementation; document as a known limitation.

#### Pitfall 6: Kernel layer repack determinism

EROFS reproducibility is handled by `-T0 -Uclear`, but the **kernel
layer** (patched initrd) also needs deterministic output for content-
addressing to work. The cpio repacker must:

- Traverse directories in sorted order (not filesystem/map iteration order)
- Use fixed timestamps (e.g. mtime=0) for all injected entries
- Use deterministic compression: repack with the **same algorithm** as the
  original segment (see Pitfall 1 hard rule), and use **fixed compression
  parameters** to ensure bit-identical output across runs:
  - **gzip**: `mtime=0` in gzip header (`gzip.Header.ModTime = time.Time{}`),
    fixed compression level (e.g. `gzip.BestCompression`), single-threaded
  - **zstd**: fixed compression level (e.g. `zstd.SpeedDefault`),
    single-threaded (`zstd.WithEncoderConcurrency(1)`) — multi-threaded
    zstd can produce non-deterministic output
  - **xz**: fixed preset (e.g. `-6`), single-threaded (`--threads=1`) —
    multi-threaded xz uses non-deterministic block splitting

Without these, the same input produces different SHA-256 hashes across
runs, defeating kernel layer dedup.

## Drawbacks

1. **Host dependencies** — `mkfs.erofs` (erofs-utils) and `qemu-img`
   (qemu-utils, for cloudimg qcow2→raw conversion) must be installed.
   Both are widely packaged but not in all distro default installs.

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

3. **~~cloudimg partition discovery~~**: resolved. Detect qcow2 format by
   magic bytes, convert to raw via `qemu-img` if needed, parse GPT in Go
   for partition offset, then `mount -o ro,loop,offset=<N>`. Cocoon runs
   as root, so `mount` is available. Non-GPT images fail fast with a
   clear error.

4. **Multi-arch support**: amd64 and arm64 cloudimgs have different kernel
   configs. Module availability (erofs=m vs erofs=y) may differ.

5. **~~Migration path~~**: resolved. No migration from existing qcow2 VMs.
   This is a clean-break architecture change. Existing VMs must be deleted
   and recreated. Cocoon will bump the config schema version; VMs with
   the old schema are rejected at startup with a clear error.

6. **COW disk default size**: inherit from `--disk-size` flag or separate?

7. **~~Distro-specific guest patches~~**: resolved. Minimal mandatory set
   identified: clear fstab + mask fsck-root + mask remount-fs. These are
   structurally required (systemd cannot boot overlay root without them).
   No optional service masking (cloud-init, boot-efi.mount, etc.) — those
   are left to the user if needed.

8. **~~`mkfs.erofs` reproducibility~~**: resolved as acceptance gate.
   Cocoon pins explicit `mkfs.erofs` flags: `-zlz4hc` (compression),
   `-C65536` (cluster size), `-T0` (fixed timestamp), `-Uclear` (zero
   UUID — without this, a random UUID is generated per build, breaking
   reproducibility). Content-addressing hashes the **output EROFS file**,
   not the input. Same input + same flags + same `mkfs.erofs` version =
   same output (upstream documents this as a reproducible-build guarantee).

   **Directory traversal determinism**: `mkfs.erofs <dir>` depends on
   the underlying filesystem's `readdir` order, which can vary across
   machines, kernel versions, or filesystem types. To eliminate this
   source of non-determinism, Cocoon **requires** `mkfs.erofs --tar=`
   mode: generate a deterministic tar stream in Go, pipe it to
   `mkfs.erofs --tar=f -`. The tar stream must:
   - Sort entries lexicographically (deterministic traversal order)
   - Use fixed mtime (e.g. `mtime=0`) for all entries
   - **Preserve original uid/gid/mode** (do NOT use fixed uid/gid —
     that would break image ownership semantics)
   - Encode xattrs via PAX/SCHILY headers (`SCHILY.xattr.*`),
     including `security.capability` (binary) and
     `trusted.overlay.opaque` — consistent with containerd/buildkit
     tar format. Implementation must reuse a mature tar writer
     (e.g. containerd's `archive` package) for xattr encoding.
   - Encode device nodes (char/block, including `c 0 0` whiteouts)
     and hardlinks as standard tar entries
   This completely decouples EROFS output from host filesystem behavior
   while preserving full fidelity of the original rootfs content.

   Cocoon's acceptance tests verify hash stability across repeated builds.
   Cocoon records `mkfs.erofs` version in `meta.json` for diagnostics.
   Minimum required version: **erofs-utils >= 1.7** (`--tar=` support).

   **Oldest supported guest kernel**: **5.13**. This is the floor for
   the ext4 feature compatibility gate, EROFS PCLUSTER, and XATTR
   requirements. Kernels older than 5.13 are not tested or supported.
   This covers all actively maintained distro LTS releases (Ubuntu
   22.04+ = 5.15, RHEL 9+ = 5.14, Debian 12+ = 6.1). Debian 11
   (kernel 5.10) reached EOL in 2024 and is excluded.

   **Kernel config verification (materialize-time hard gate)**:
   Materialize **must** parse the guest kernel config and verify
   required options before producing EROFS layers. Config sources
   (checked in order, from the mounted rootfs):
   1. `/boot/config-<ver>` (most distros ship this)
   2. `/usr/lib/modules/<ver>/config` (some distros)
   3. If no config found → **fail materialize** with PermanentError
      (do not guess — an unverifiable kernel is not supportable)
   Note: `/proc/config.gz` is a runtime procfs node, not a file on
   disk — it is not available when the rootfs is mounted offline.

   **This is a deliberate design trade-off**: Cocoon chooses to only
   support images with verifiable kernel configuration, rejecting
   images that cannot be validated. This narrows the supported image
   set but prevents the worst failure mode: images that build
   successfully but fail at boot or exhibit silent semantic errors.
   The error output must include: kernel version, config paths
   attempted, and which required options were missing or disabled.

   Required kernel config options:
   - `CONFIG_EROFS_FS=y|m` — EROFS filesystem support.
   - `CONFIG_EROFS_FS_PCLUSTER=y` — required for 64KB pcluster (`-C65536`).
     Available since Linux 5.13 — exactly the supported floor.
   - `CONFIG_EROFS_FS_XATTR=y` — required for reading `trusted.overlay.opaque`
     xattrs from EROFS. Without it, opaque directory semantics in multi-layer
     OCI images break silently (cross-layer deletions are not applied).
   - `CONFIG_OVERLAY_FS=y|m` — overlayfs support.

   Any missing or disabled option → **fail materialize** with a clear
   error listing the missing config options and the kernel version.
   This ensures failures are caught at build time, not boot time.
   Modern distro kernels (5.15+, Ubuntu 22.04+, RHEL 9+) pass all checks.

9. **~~OCI whiteout semantics~~**: resolved. Whiteout handling is defined
   in the OCI Image Materialize Pipeline section. Flatten applies OCI
   whiteout deletion semantics; multi-EROFS lowerdir converts `.wh.*`
   to native overlayfs whiteout devices. Acceptance test coverage is
   required before Phase 1 completion.

10. **~~Shutdown semantics~~**: resolved. ACPI is **retained** even without
    UEFI firmware — CH generates ACPI tables for direct-boot VMs. Unified
    sequence: `vm.power-button` (ACPI) → wait → `vm.shutdown` (VMM forced
    stop) → `SIGTERM` → `SIGKILL`. See "Note on ACPI" in Code Impact.
    The two existing code paths (`Shutdown` and `ShutdownDirect`) merge
    into a single path.

11. **Kernel/initrd selection**: images with multiple kernels (e.g.
    `/boot/vmlinuz-*`) — Cocoon selects the **latest version** using
    **version-aware natural sorting** (numeric segments compared as
    integers, not lexically — e.g. `5.15 > 5.9`). Do NOT use lexical
    sort (`sort.Strings`), which gives `5.9 > 5.15`. The Go
    implementation should parse version segments and compare
    numerically (similar to `dpkg --compare-versions`).
    **Paired matching**: vmlinuz and initrd.img are paired by their
    exact version suffix (e.g. `vmlinuz-5.15.0-100-generic` pairs with
    `initrd.img-5.15.0-100-generic`). Unpaired kernels (no matching
    initrd) are skipped. If no valid pair is found, materialize fails.
    **Symlink fallback**: some images provide `/boot/vmlinuz` and
    `/boot/initrd.img` as symlinks to the current kernel. If no
    versioned `vmlinuz-*` files are found, Cocoon follows these
    symlinks and uses the resolved targets. This handles minimal
    images that only ship the current kernel without versioned names.

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

- Materialize: qcow2→raw conversion, extract kernel + initramfs, mkfs.erofs
- Patch initramfs: inject hook + erofs/overlay modules
- Create: raw COW disk
- Remove UEFI firmware (`CLOUDHV.fd`), qcow2 overlay chain
- Unify shutdown path (ACPI power button works for both boot modes)
- Retain `qemu-img` dependency (repurposed: qcow2→raw instead of qcow2→qcow2)

### Phase 3: Cleanup

- Remove dual-path code (UEFI vs direct boot branching)
- Simplify GC (single layer model)
- Update all docs

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

## Acceptance Criteria

Each phase gate requires passing the following test matrix:

### Phase 1 Gate (OCI VM Path)

| Test | amd64 | arm64 |
|---|---|---|
| OCI image boot (initramfs-tools) | Required | Required |
| OCI image boot (dracut) | Required | Required |
| Multi-layer OCI with whiteout files | Required | Required |
| COW write persistence across reboot | Required | Required |
| Unclean shutdown → COW recovery | Required | Required |
| COW ext4 mounts on oldest supported guest kernel | Required | Required |
| Layer dedup (two VMs, same image) | Required | - |
| GC: delete VM → layer refcount→0 → layer removed | Required | - |
| Guest sysfs serial matches host-set serial | Required | Required |
| dracut image boots with `boot=cocoon` (no side effects) | Required | - |
| Cross-layer whiteout: user layer deletes base file → file invisible | Required | Required |
| Serial 20-byte edge case: 20-char serial resolves correctly | Required | - |
| EROFS xattr: `trusted.overlay.opaque` readable on oldest kernel (5.13) | Required | Required |
| Opaque dir: user layer marks dir opaque → base dir contents invisible | Required | Required |
| Kernel config gate: materialize fails if EROFS_FS_PCLUSTER/XATTR missing | Required | Required |
| EROFS mount smoke: `mount -t erofs` succeeds in initramfs; failure log includes mkfs flags + kernel version + loaded modules | Required | Required |
| OCI unpack fidelity: `security.capability` xattrs preserved (e.g. `getcap /bin/ping`) | Required | Required |
| OCI unpack fidelity: hardlinks preserved (inode/link count match source) | Required | - |
| Composite behavior: base has `ping` with cap + `dirA/basefile`; user layer deletes `basefile`, marks `dirA` opaque → boot, verify ping works + basefile invisible + dirA has only user content | Required | - |
| systemd compat: machine-id unique per VM (empty at overlay mount, regenerated on first boot; two VMs from same image must have different IDs), journald writes to `/var/log/journal`, cloud-init (if present) does not block boot | Required | Required |
| Overlay copy-up: modify a file from EROFS lowerdir → verify copy-up to COW upper succeeds, original unchanged | Required | - |
| Overlay rename: rename a directory from EROFS lowerdir → verify rename succeeds in COW. Check `/run/cocoon/boot.env`: if `overlay_opts_effective=full`, assert `redirect_dir=off` behavior; if `fallback`, only assert rename is non-fatal | Required | - |

### Phase 2 Gate (Cloudimg Path)

| Test | amd64 | arm64 |
|---|---|---|
| Ubuntu cloudimg (qcow2) boot | Required | Required |
| Debian cloudimg boot | Required | - |
| CentOS/RHEL cloudimg (dracut, GPT+xfs) boot | Best-effort | - |
| Multi-kernel image: correct vmlinuz selection | Required | - |
| systemd starts cleanly (no fsck hang, no remount fail) | Required | Required |

### Known Limitations (not blocking)

- SELinux enforcing images: cpio repack may lose SELinux labels (best-effort)
- mkinitcpio (Arch Linux): not supported (FormatUnknown → hard fail)
- `boot=cocoon` cmdline: injected unconditionally (required by initramfs-tools,
  harmless on dracut). If a future distro uses `boot=` for a conflicting
  purpose, that distro requires adaptation (not currently known to exist).
- Rootfs mtime canonicalization: all file mtimes in EROFS images are set to 0
  (epoch) for deterministic builds. Workloads relying on original mtimes
  (e.g. incremental build systems, cache invalidation based on timestamps)
  may observe differences compared to running the same image via Docker.
- Overlay option fallback: when explicit overlay mount options
  (`index=off,metacopy=off,redirect_dir=off`) trigger EINVAL, the hook
  falls back to kernel defaults. The effective mode is recorded in
  `/run/cocoon/boot.env` for diagnostics. The EINVAL detection is based
  on error message string matching, which is inherently fragile; the
  implementation should prefer checking the kernel's exit code where
  feasible and use string matching only as a supplementary signal.
- Module basename collision: kernel modules are placed flat in
  `/cocoon-modules/` using their basename. If two modules in the
  dependency chain share the same basename (from different subdirectories
  in `/lib/modules/`), materialize fails with a `PermanentError` listing
  the conflicting paths. This is rare but can occur with out-of-tree or
  vendor modules.
- Cloudimg formats: GPT+ext4 is the primary supported partition layout.
  GPT+xfs (RHEL/CentOS) is best-effort — requires xfs read support on
  the host for `mount -o ro,loop`. MBR/LVM fail fast with descriptive error.

### Tool Preflight

Cocoon validates tool availability on demand (not globally at startup):
- `mkfs.erofs` >= 1.7: checked at first materialize (`mkfs.erofs -V`).
  Version 1.7+ is required for `--tar=` mode (deterministic builds).
  Reference: `--tar` support added in
  [erofs-utils v1.7](https://git.kernel.org/pub/scm/linux/kernel/git/xiang/erofs-utils.git/tag/?h=v1.7).
  Version comparison must be **semantic** (not string-based).
- `mkfs.ext4` + `e2fsck` (e2fsprogs): checked at first `cocoon create`
- `qemu-img` (any version): checked only for cloudimg materialize
  (`qemu-img --version`). OCI-only users do not need `qemu-img`.
- CH >= v35 (for serial support): version check at VM launch.
  Reference: [cloud-hypervisor/cloud-hypervisor#5765](https://github.com/cloud-hypervisor/cloud-hypervisor/pull/5765)

Missing tools → `types.PermanentError` with install instructions.

## References

- [Issue #61: RFC: squashfs block device for OCI VM rootfs](https://github.com/CMGS/cocoon/issues/61)
- [EROFS filesystem documentation](https://docs.kernel.org/filesystems/erofs.html)
- [Kata Containers architecture](https://github.com/kata-containers/kata-containers/blob/main/docs/design/architecture/README.md)
- [Cloud Hypervisor disk configuration](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/disks.md)
- [initramfs-tools `/init` source (Debian)](https://sources.debian.org/src/initramfs-tools/0.140/init/)
- [initramfs-tools hook documentation](https://manpages.ubuntu.com/manpages/noble/man8/initramfs-tools.8.html)
- [dracut hook documentation](https://wwoods.fedorapeople.org/doc/dracut-notes.html)
- [Cloud Hypervisor v35 — serial support](https://www.cloudhypervisor.org/blog/cloud-hypervisor-v35.0-released/)
- [Linux overlayfs documentation](https://docs.kernel.org/filesystems/overlayfs.html)

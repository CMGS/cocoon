# Garbage Collection

**Version**: 1.0
**Status**: Implemented
**Phase**: Phase 1
**Last Updated**: 2026-02-18

## Overview

Garbage collection (GC) reclaims disk space from resources that are no longer needed. All deletions are **permanent** -- there is no trash directory or soft-delete mechanism.

Design principles:

- **Permanent deletion**: All GC operations permanently remove files. There is no recovery mechanism for GC'd resources.
- **No grace period for images**: Unreferenced base images are collected immediately. The reference counter ensures in-use images are never collected.
- **Defense-in-depth grace for OCI**: OCI layouts and blobs younger than 5 minutes are skipped to prevent races with concurrent builds that have not yet recorded their references.
- **Build staging isolation**: OCI build temp layout directories are created under `temp/oci-layout-builds/` (not under `cache/oci/layouts/`) so Phase 3 only scans finalized layout directories.
- **Dry-run support**: `cocoon gc --dry-run` previews what would be collected without making changes.
- **Lock-safe**: Each phase acquires the appropriate locks to prevent races with concurrent VM creation/deletion.

## Resource Types

GC manages 7 categories of resources:

| # | Resource | Detection Rule | Action |
|---|----------|---------------|--------|
| 1 | Unreferenced base images | `cache/images/*.qcow2` with zero refs in `references.json` | `os.Remove` |
| 2 | Orphaned overlays | VM dir has `overlay.qcow2` but no `config.json` | `os.RemoveAll(vmDir)` |
| 3 | Orphaned OCI layouts | Finalized directory in `cache/oci/layouts/` not referenced by any tag | `os.RemoveAll(layoutDir)` |
| 4 | Stale OCI tags | Tag in `oci-build-tags.json` whose `layout_path` does not exist | Remove from index; cascade to orphaned manifests/blobs |
| 5 | Orphaned OCI manifest refs | Manifest digest in `oci-layer-refs.json` not associated with any live tag | Remove from blob entries; delete zero-ref blobs |
| 6 | Unreferenced OCI blobs | `cache/oci/blobs/sha256/` file with zero manifest refs | `os.Remove` |
| 7 | Temp entries | Files/directories in `temp/` older than 1 hour | `os.RemoveAll` |

## Phase Execution Order

```
Phase 1: Unreferenced cloud images     → permanent delete
Phase 2: Orphaned overlays             → permanent delete (entire VM dir)
Phase 3: Orphaned OCI layouts          → permanent delete
Phase 4: Stale OCI tags                → remove from tag index, cascade cleanup
Phase 5: Orphaned OCI manifest refs    → remove from layer-refs, delete zero-ref blobs
Phase 6: Unreferenced OCI blobs        → permanent delete
Phase 7: Temp entries                  → permanent delete
```

Phases 3-6 form a cascade:
1. **Phase 3** deletes orphaned layouts (directories on disk not referenced by any tag).
2. **Phase 4** cleans stale tags (tag entries pointing to missing layouts), which may orphan manifest digests.
3. **Phase 5** removes orphaned manifest refs from blob entries, deleting blobs that become zero-ref.
4. **Phase 6** catches any remaining unreferenced blobs (e.g., untracked blobs not in layer-refs at all).

Each phase acquires `gc.lock` independently. The cycle is NOT atomic across phases, but safe because each phase performs its own reference check under lock.

## Locking

Lock acquisition order (see [06-concurrency.md](./06-concurrency.md)):

```
gc.lock (Level 1)
  ├── references.lock (Level 2) — for Phase 1 (per-image atomic check-and-delete)
  ├── oci-build-txn.lock        — for Phase 3 (serialize with finalize/save-tag)
  │   └── oci-build-tags.lock   — for Phase 3 (held for entire orphan scan)
  ├── oci-build-txn.lock        — for Phase 4, 5 (serialize cross-index updates)
  │   ├── oci-build-tags.lock   — for Phase 4, 5 (read tag index)
  │   └── oci-layer-refs.lock   — for Phase 4, 5 (modify blob refs)
  └── oci-layer-refs.lock       — for Phase 6 (atomic check-and-delete)
```

Never acquire Level 1 while holding Level 2. Within a single phase, locks are acquired in the order shown above and released in reverse order.

## OCI Reference Chain

```
Tag (oci-build-tags.json)
  ├── layout_path      → cache/oci/layouts/{hash}/
  └── manifest_digest  → used as key in oci-layer-refs.json

Layer Refs (oci-layer-refs.json)
  └── blobs[digest].manifest_digests[]  → which manifests reference this blob

Blob Store (cache/oci/blobs/sha256/)
  └── {hex}  → content-addressed blob file
```

A blob is safe to delete when it has zero manifest references AND is older than the 5-minute grace period.

## CLI

### `cocoon gc`

Runs all 7 phases and permanently deletes all collectable resources.

```
$ cocoon gc
collected image: a1b2c3d4e5f6a7b8_amd64
collected orphaned overlay: vm-01HABC...
collected stale OCI tag: myregistry.io/old-image:v1

Collected 3 item(s): 1 images, 1 overlays, 0 OCI layouts, 1 stale tags, 0 orphaned manifests, 0 OCI blobs, 0 temp files.
```

### `cocoon gc --dry-run`

Previews what would be collected without making any changes.

```
$ cocoon gc --dry-run
Dry run

Unreferenced images (candidates for collection):
  a1b2c3d4e5f6a7b8_amd64

No orphaned overlays found.
...

Use 'cocoon gc' without --dry-run to perform collection.
```

## Delete vs GC

| Operation | Trigger | Scope |
|-----------|---------|-------|
| `cocoon delete` / `cocoon image rm` | User-initiated | Deletes specific VM/image and its resources |
| `cocoon gc` | Operator-initiated | Cleans up orphaned/unreferenced resources system-wide |

`delete` and `image rm` are targeted operations on user-specified resources. GC is a system-wide sweep that finds and removes resources that are no longer needed by any VM or tag.

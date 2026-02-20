// Package storage provides interfaces and implementations for base image
// reference counting, copy-on-write overlay management, and garbage collection.
//
// Concurrency model (see docs/06-concurrency.md):
//   - All reference mutations hold references.lock (flock, Level 2).
//   - GC acquires gc.lock (Level 1) THEN references.lock (Level 2) -- never reverse.
//   - JSON persistence uses atomic write: temp + fsync + rename.
//   - Pin pattern: AddReference immediately (short lock hold), then slow I/O outside lock.
package storage

import (
	"context"
	"time"
)

// OCIGCGracePeriod is the defense-in-depth grace period for OCI GC operations.
// OCI layouts and blobs younger than this are skipped to prevent races with
// concurrent builds that have not yet recorded their tag/blob references.
const OCIGCGracePeriod = 5 * time.Minute

// ReferenceCounter manages base image reference counts.
//
// All mutations (AddReference, RemoveReference) MUST be called while holding
// references.lock (flock, Level 2). Implementations handle lock acquisition
// internally.
//
// Keys are content-addressed: {checksum_16}_{arch} (e.g., "a1b2c3d4e5f6a7b8_amd64").
// See docs/05-storage-management.md for the checksum identity contract.
type ReferenceCounter interface {
	// AddReference pins a VM to a base image.
	// If baseKey already exists, the incoming digestFull is compared against
	// the stored value; a mismatch returns types.ErrChecksumCollision.
	// baseKey: content-addressed key ({checksum_16}_{arch}).
	// vmID: the vm-{ulid} being created.
	// digestFull: full 64-char SHA-256 hex for collision detection.
	// sourceRef: original image reference (OCI ref / URL / file path) for audit.
	AddReference(baseKey, vmID, digestFull, sourceRef string) error

	// RemoveReference unpins a VM from a base image.
	// When the last reference is removed the entry is deleted from references.json.
	RemoveReference(baseKey, vmID string) error

	// GetReferences returns the VM IDs currently pinning baseKey.
	// Returns an empty slice (not nil) when the key has no references.
	GetReferences(baseKey string) ([]string, error)

	// IsReferenced reports whether baseKey has at least one VM reference.
	IsReferenced(baseKey string) (bool, error)

	// GetUnreferencedImages returns base_keys present on disk in
	// cache/images/ that have zero entries in references.json.
	GetUnreferencedImages() ([]string, error)
}

// COWManager manages copy-on-write overlay images backed by qcow2.
//
// Overlay creation does NOT require any global lock -- each overlay lives in
// its own VM directory and uses a distinct file path.
type COWManager interface {
	// CreateBaseImage copies (or converts) a source qcow2 into the image cache
	// directory using the content-addressed name {baseKey}.qcow2.
	// This is idempotent: if the target already exists it returns nil.
	CreateBaseImage(srcPath, baseKey string) error

	// CreateOverlay creates a thin COW overlay backed by the base image
	// identified by baseKey.  Returns the absolute overlay path
	// (/var/lib/cocoon/vms/{vmID}/overlay.qcow2).
	CreateOverlay(ctx context.Context, baseKey, vmID, diskSize string) (overlayPath string, err error)

	// RemoveOverlay removes the VM overlay disk file for vmID.
	// It does not remove the VM directory or config/metadata files.
	RemoveOverlay(vmID string) error

	// GetOverlayInfo returns metadata about an existing overlay.
	GetOverlayInfo(ctx context.Context, vmID string) (*OverlayInfo, error)
}

// GarbageCollector reclaims unreferenced storage resources.
//
// All deletions are permanent. There is no trash or grace period for
// unreferenced images — if an image has zero references it is collected.
//
// Locking order (docs/06-concurrency.md):
//  1. Acquire gc.lock (Level 1).
//  2. For each image, acquire references.lock (Level 2) for atomic
//     check-and-delete inside the critical section.
//  3. Never acquire locks in the reverse order.
type GarbageCollector interface {
	// CollectUnreferencedImages permanently deletes base images that have
	// zero references. Returns the base_keys that were collected.
	CollectUnreferencedImages() ([]string, error)

	// CollectOrphanedOverlays finds VM directories where overlay.qcow2 exists
	// but config.json is missing (crash point 1). Permanently deletes the
	// VM directory. Returns the VM IDs that were collected.
	CollectOrphanedOverlays() ([]string, error)

	// CollectOrphanedOCILayouts removes OCI layout directories not referenced
	// by the tag index. Returns collected layout directory names.
	CollectOrphanedOCILayouts() ([]string, error)

	// CollectStaleOCITags removes tags whose layout path no longer exists
	// on disk. Cascades cleanup to orphaned manifest refs and zero-ref blobs.
	// Returns collected stale tag names.
	CollectStaleOCITags() ([]string, error)

	// CollectOrphanedOCIManifestRefs removes manifest digest references from
	// the layer-refs index that are not associated with any live tag.
	// Zero-ref blobs are deleted (with a 5-min grace for in-progress builds).
	// Returns orphaned manifest digests cleaned.
	CollectOrphanedOCIManifestRefs() ([]string, error)

	// CollectUnreferencedOCIBlobs removes blobs from the shared blob store
	// that have zero manifest references. Returns collected blob digests.
	CollectUnreferencedOCIBlobs() ([]string, error)

	// CollectUnreferencedOCIRuntimeCaches removes OCI runtime cache directories
	// that have no active pins. Returns collected runtime key names.
	CollectUnreferencedOCIRuntimeCaches() ([]string, error)

	// CollectStaleConversionLocks removes stale image-conversion lock files
	// older than maxAge. Returns the filenames that were collected.
	CollectStaleConversionLocks(maxAge time.Duration) ([]string, error)

	// CollectTempFiles removes files in temp/ older than maxAge.
	// Returns the filenames that were collected.
	CollectTempFiles(maxAge time.Duration) ([]string, error)

	// FullGC runs a complete garbage collection cycle:
	// CollectUnreferencedImages + CollectOrphanedOverlays +
	// CollectOrphanedOCILayouts + CollectStaleOCITags +
	// CollectOrphanedOCIManifestRefs + CollectUnreferencedOCIBlobs +
	// CollectUnreferencedOCIRuntimeCaches + CollectStaleConversionLocks +
	// CollectTempFiles.
	FullGC() error
}

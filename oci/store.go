package oci

import (
	"crypto/sha256"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/lock/flock"
	"github.com/CMGS/cocoon/lock/jsonstore"
)

// Store manages local OCI VM image builds and their tag index.
type Store struct {
	cfg              *config.CocoonConfig
	removeBlobRefsFn func(cfg *config.CocoonConfig, manifestDigest string) ([]string, error)
	tagStore         *jsonstore.Store[TagIndex]
}

var blobRefCleanupFailures atomic.Uint64

// NewStore creates a Store for the given config.
func NewStore(cfg *config.CocoonConfig) *Store {
	return &Store{
		cfg:              cfg,
		removeBlobRefsFn: RemoveBlobRefs,
		tagStore: jsonstore.New(
			cfg.OCIBuildTagLock(),
			cfg.OCIBuildTagIndex(),
			func() *TagIndex { return &TagIndex{Tags: make(map[string]TagEntry)} },
		).WithEnsureDir(cfg.DBDir()),
	}
}

// BlobRefCleanupFailureCount returns the number of best-effort cleanup failures
// observed while rewriting tags. This is a diagnostic counter for leak risk.
func BlobRefCleanupFailureCount() uint64 {
	return blobRefCleanupFailures.Load()
}

// LayoutDir returns the directory path for an OCI layout keyed by layoutKey.
// It uses the full sha256(layoutKey) hex digest to avoid truncated-hash
// collisions between different tag+manifest combinations.
func (s *Store) LayoutDir(layoutKey string) string {
	h := sha256.Sum256([]byte(layoutKey))
	keyHash := fmt.Sprintf("%x", h[:]) // 64 hex chars
	return filepath.Join(s.cfg.OCILayoutDir(), keyHash)
}

// SaveTag records a tag-to-layout mapping in the tag index.
// If the tag already exists with a different manifest digest, the old
// manifest's blob references are cleaned up via RemoveBlobRefs to prevent
// reference leaks.
func (s *Store) SaveTag(tag, layoutPath, manifestDigest string) error {
	if err := os.MkdirAll(s.cfg.DBDir(), 0o700); err != nil {
		return fmt.Errorf("create db dir: %w", err)
	}
	return flock.WithLock(s.cfg.OCIBuildTxnLock(), func() error {
		return s.saveTagTxnLocked(tag, layoutPath, manifestDigest)
	})
}

// HasTag reports whether the tag exists in the local OCI build tag index.
// Unlike ResolveTag, this does not verify layout path existence on disk.
func (s *Store) HasTag(tag string) (bool, error) {
	exists := false
	err := s.tagStore.Read(func(idx *TagIndex) error {
		_, exists = idx.Tags[tag]
		return nil
	})
	if err != nil {
		return false, err
	}
	return exists, nil
}

// GetTag returns the tag entry from the local OCI build tag index.
func (s *Store) GetTag(tag string) (TagEntry, error) {
	var entry TagEntry
	err := s.tagStore.Read(func(idx *TagIndex) error {
		found, ok := idx.Tags[tag]
		if !ok {
			return fmt.Errorf("tag %q not found in local builds", tag)
		}
		entry = found
		return nil
	})
	if err != nil {
		return TagEntry{}, err
	}
	return entry, nil
}

// ResolveTag looks up a tag in the index and returns the layout path.
func (s *Store) ResolveTag(tag string) (string, error) {
	var layoutPath string
	err := s.tagStore.Read(func(idx *TagIndex) error {
		entry, ok := idx.Tags[tag]
		if !ok {
			return fmt.Errorf("tag %q not found in local builds", tag)
		}
		layoutPath = entry.LayoutPath
		return nil
	})
	if err != nil {
		return "", err
	}
	// Verify outside the lock to avoid holding it during I/O.
	if _, statErr := os.Stat(layoutPath); os.IsNotExist(statErr) {
		return "", fmt.Errorf("layout directory for tag %q no longer exists: %s", tag, layoutPath)
	}
	return layoutPath, nil
}

// ListTags returns all local build tags, sorted by creation time (newest first).
func (s *Store) ListTags() ([]TagEntry, error) {
	var result []TagEntry
	err := s.tagStore.Read(func(idx *TagIndex) error {
		result = slices.Collect(maps.Values(idx.Tags))
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(result, func(a, b TagEntry) int {
		return b.CreatedAt.Compare(a.CreatedAt)
	})
	return result, nil
}

// RemoveTag removes a tag from the index, its layout directory, and
// cleans up blob references. Returns the manifest digest and any blob
// digests that are now unreferenced (zero refs remaining).
//
// Blob references are only removed when no other tag shares the same
// manifest digest, preventing premature GC of blobs still in use.
func (s *Store) RemoveTag(tag string) (string, []string, error) {
	var manifestDigest string
	var layoutPath string
	var layoutStillUsed bool
	var zeroRefBlobs []string
	if err := os.MkdirAll(s.cfg.DBDir(), 0o700); err != nil {
		return "", nil, fmt.Errorf("create db dir: %w", err)
	}
	err := flock.WithLock(s.cfg.OCIBuildTxnLock(), func() error {
		manifestStillUsed := false
		lockErr := s.tagStore.Update(func(idx *TagIndex) error {
			entry, ok := idx.Tags[tag]
			if !ok {
				return fmt.Errorf("tag %q not found", tag)
			}
			manifestDigest = entry.ManifestDigest
			layoutPath = entry.LayoutPath

			// Refuse removal when running VMs still reference this image's
			// OCI runtime cache entry.
			if err := checkRuntimeRefsForRemoval(s.cfg, tag, manifestDigest); err != nil {
				return err
			}

			delete(idx.Tags, tag)
			for _, other := range idx.Tags {
				if manifestDigest != "" {
					if other.ManifestDigest == manifestDigest {
						manifestStillUsed = true
					}
				}
				if layoutPath != "" && other.LayoutPath == layoutPath {
					layoutStillUsed = true
				}
			}
			return nil
		})
		if lockErr != nil {
			return lockErr
		}

		// Serialize cross-index changes with oci-build-txn.lock while still
		// keeping same-level locks disjoint (tag lock and layer-refs lock are
		// never held simultaneously).
		if manifestDigest != "" && !manifestStillUsed {
			var refErr error
			zeroRefBlobs, refErr = s.removeBlobRefsFn(s.cfg, manifestDigest)
			if refErr != nil {
				return fmt.Errorf("remove blob refs for %s: %w", manifestDigest, refErr)
			}
		}
		// Remove layout directory while still holding txn lock so concurrent
		// SaveTag/Tag operations cannot re-point a tag to this path mid-delete.
		if layoutPath != "" && !layoutStillUsed {
			safeRemoveLayoutDir(s.cfg, layoutPath)
		}
		return nil
	})
	if err != nil {
		return "", nil, err
	}

	return manifestDigest, zeroRefBlobs, nil
}

// CheckTagOverwriteSafe checks whether the given tag can be safely overwritten
// (or created) without conflicting with running VMs. If the tag does not exist,
// it returns nil (new tag, no conflict). If the tag exists and its current
// manifest is still referenced by running VMs, it returns an error. This is
// intended as a fast pre-check before starting an expensive build, so the user
// learns early that the tag cannot be overwritten. The existing check in
// SaveTag is kept as a safety net for race conditions.
func (s *Store) CheckTagOverwriteSafe(tag string) error {
	var manifestDigest string
	err := s.tagStore.Read(func(idx *TagIndex) error {
		entry, ok := idx.Tags[tag]
		if !ok {
			return nil // new tag, no conflict
		}
		manifestDigest = entry.ManifestDigest
		return nil
	})
	if err != nil {
		return err
	}
	if manifestDigest == "" {
		return nil
	}
	return checkRuntimeRefsForRemoval(s.cfg, tag, manifestDigest)
}

func (s *Store) saveTagTxnLocked(tag, layoutPath, manifestDigest string) error {
	var oldManifestDigest string
	var oldLayoutPath string
	oldManifestStillUsed := false
	oldLayoutStillUsed := false
	err := s.tagStore.Update(func(idx *TagIndex) error {
		if idx.Tags == nil {
			idx.Tags = make(map[string]TagEntry)
		}
		// Track old manifest and layout for cleanup when overwriting a tag.
		if old, exists := idx.Tags[tag]; exists && old.ManifestDigest != manifestDigest {
			oldManifestDigest = old.ManifestDigest
			oldLayoutPath = old.LayoutPath
		}
		// Refuse overwrite when running VMs still reference the old image's
		// OCI runtime cache entry — mirrors the check in RemoveTag.
		if oldManifestDigest != "" {
			if err := checkRuntimeRefsForRemoval(s.cfg, tag, oldManifestDigest); err != nil {
				return err
			}
		}
		idx.Tags[tag] = TagEntry{
			Tag:            tag,
			LayoutPath:     layoutPath,
			ManifestDigest: manifestDigest,
			CreatedAt:      time.Now().UTC(),
		}
		if oldManifestDigest != "" {
			for _, entry := range idx.Tags {
				if entry.ManifestDigest == oldManifestDigest {
					oldManifestStillUsed = true
				}
				if oldLayoutPath != "" && entry.LayoutPath == oldLayoutPath {
					oldLayoutStillUsed = true
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Cleanup old manifest resources when overwriting a tag.
	// RemoveBlobRefs is called outside the tag lock but still inside the txn
	// lock. This is safe because the txn lock serializes all cross-index
	// operations (SaveTag, RemoveTag), so no concurrent SaveTag or RemoveTag
	// can run while we modify blob refs. Keeping the tag lock and layer-refs
	// lock disjoint (never held simultaneously) avoids lock-ordering deadlocks.
	if oldManifestDigest != "" && !oldManifestStillUsed {
		zeroRefBlobs, refErr := s.removeBlobRefsFn(s.cfg, oldManifestDigest)
		if refErr != nil {
			failures := blobRefCleanupFailures.Add(1)
			log.Printf("WARNING: failed to clean old manifest %s blob refs (will be reclaimed by GC): %v (cleanup_failures=%d)", oldManifestDigest, refErr, failures)
		}
		// Eagerly remove zero-referenced blobs instead of waiting for GC.
		blobStore := NewBlobStore(s.cfg)
		for _, digest := range zeroRefBlobs {
			if blobErr := blobStore.RemoveBlob(digest); blobErr != nil {
				log.Printf("warning: remove unreferenced blob %s after tag overwrite: %v", digest, blobErr)
			}
		}
	}
	// Remove old layout directory if no other tag shares it.
	if oldLayoutPath != "" && !oldLayoutStillUsed {
		safeRemoveLayoutDir(s.cfg, oldLayoutPath)
	}
	return nil
}

// checkRuntimeRefsForRemoval returns an error if the given manifest digest
// has an OCI runtime cache entry that is still pinned by at least one VM.
// manifestDigest may be in "sha256:<hex>" or bare "<hex>" format.
func checkRuntimeRefsForRemoval(cfg *config.CocoonConfig, tag, manifestDigest string) error {
	if manifestDigest == "" {
		return nil
	}
	// Normalize: build stores bare hex, pull stores "sha256:" prefix.
	runtimeKey, parseErr := ParseSHA256Digest(manifestDigest)
	if parseErr != nil {
		// Try bare hex (build path stores without sha256: prefix).
		runtimeKey, parseErr = ParseSHA256Digest("sha256:" + manifestDigest)
		if parseErr != nil {
			return nil // genuinely non-standard format — nothing to check
		}
	}
	referenced, err := IsRuntimeReferenced(cfg, runtimeKey)
	if err != nil {
		return fmt.Errorf("check OCI runtime refs for %s: %w", tag, err)
	}
	if !referenced {
		return nil
	}
	refs, _ := GetRuntimeRefs(cfg, runtimeKey)
	return fmt.Errorf("image %s is still referenced by VMs: %v", tag, refs)
}

// safeRemoveLayoutDir removes the layout directory only if it is located
// under the OCI layout root. This prevents accidental deletion of unrelated
// directories when index data is corrupted.
func safeRemoveLayoutDir(cfg *config.CocoonConfig, layoutPath string) {
	if layoutPath == "" {
		return
	}
	layoutRoot := cfg.OCILayoutDir()
	if strings.HasPrefix(filepath.Clean(layoutPath), filepath.Clean(layoutRoot)+string(filepath.Separator)) {
		_ = os.RemoveAll(layoutPath)
	} else {
		log.Printf("WARNING: refusing to remove layout path outside OCI root: %s", layoutPath)
	}
}

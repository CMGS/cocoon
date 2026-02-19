package oci

import (
	"crypto/sha256"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"time"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/lock/flock"
	"github.com/CMGS/cocoon/utils"
)

// Store manages local OCI VM image builds and their tag index.
// Follows the flock + JSON pattern from image/refcache/index.go.
type Store struct {
	cfg              *config.CocoonConfig
	removeBlobRefsFn func(cfg *config.CocoonConfig, manifestDigest string) ([]string, error)
}

var blobRefCleanupFailures atomic.Uint64

// NewStore creates a Store for the given config.
func NewStore(cfg *config.CocoonConfig) *Store {
	return &Store{
		cfg:              cfg,
		removeBlobRefsFn: RemoveBlobRefs,
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
	return s.withTxnLock(func() error {
		return s.saveTagTxnLocked(tag, layoutPath, manifestDigest)
	})
}

func (s *Store) saveTagTxnLocked(tag, layoutPath, manifestDigest string) error {
	var oldManifestDigest string
	oldManifestStillUsed := false
	err := s.withLock(func(idx *TagIndex) error {
		if idx.Tags == nil {
			idx.Tags = make(map[string]TagEntry)
		}
		// Track old manifest for blob ref cleanup when overwriting a tag.
		if old, exists := idx.Tags[tag]; exists && old.ManifestDigest != manifestDigest {
			oldManifestDigest = old.ManifestDigest
		}
		idx.Tags[tag] = TagEntry{
			Tag:            tag,
			LayoutPath:     layoutPath,
			ManifestDigest: manifestDigest,
			CreatedAt:      time.Now().UTC(),
		}
		if err := s.save(idx); err != nil {
			return err
		}
		if oldManifestDigest != "" {
			for _, entry := range idx.Tags {
				if entry.ManifestDigest == oldManifestDigest {
					oldManifestStillUsed = true
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	// RemoveBlobRefs is called outside the tag lock but still inside the txn
	// lock. This is safe because the txn lock serializes all cross-index
	// operations (SaveTag, RemoveTag), so no concurrent SaveTag or RemoveTag
	// can run while we modify blob refs. Keeping the tag lock and layer-refs
	// lock disjoint (never held simultaneously) avoids lock-ordering deadlocks.
	if oldManifestDigest != "" && !oldManifestStillUsed {
		if _, refErr := s.removeBlobRefsFn(s.cfg, oldManifestDigest); refErr != nil {
			failures := blobRefCleanupFailures.Add(1)
			log.Printf("WARNING: failed to clean old manifest %s blob refs (will be reclaimed by GC): %v (cleanup_failures=%d)", oldManifestDigest, refErr, failures)
		}
	}
	return nil
}

// HasTag reports whether the tag exists in the local OCI build tag index.
// Unlike ResolveTag, this does not verify layout path existence on disk.
func (s *Store) HasTag(tag string) (bool, error) {
	exists := false
	err := s.withLock(func(idx *TagIndex) error {
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
	err := s.withLock(func(idx *TagIndex) error {
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
	err := s.withLock(func(idx *TagIndex) error {
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
	err := s.withLock(func(idx *TagIndex) error {
		for _, entry := range idx.Tags {
			result = append(result, entry)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt.After(result[j].CreatedAt)
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
	err := s.withTxnLock(func() error {
		manifestStillUsed := false
		lockErr := s.withLock(func(idx *TagIndex) error {
			entry, ok := idx.Tags[tag]
			if !ok {
				return fmt.Errorf("tag %q not found", tag)
			}
			manifestDigest = entry.ManifestDigest
			layoutPath = entry.LayoutPath
			delete(idx.Tags, tag)
			if err := s.save(idx); err != nil {
				return err
			}
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
			_ = os.RemoveAll(layoutPath)
		}
		return nil
	})
	if err != nil {
		return "", nil, err
	}

	return manifestDigest, zeroRefBlobs, nil
}

func (s *Store) load() (*TagIndex, error) {
	idx := &TagIndex{Tags: make(map[string]TagEntry)}
	path := s.cfg.OCIBuildTagIndex()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return idx, nil
	}
	if err := utils.ReadJSON(path, idx); err != nil {
		return nil, fmt.Errorf("read OCI build tag index: %w", err)
	}
	if idx.Tags == nil {
		idx.Tags = make(map[string]TagEntry)
	}
	return idx, nil
}

func (s *Store) save(idx *TagIndex) error {
	return utils.AtomicWriteJSON(s.cfg.OCIBuildTagIndex(), idx)
}

func (s *Store) withLock(fn func(*TagIndex) error) error {
	if err := os.MkdirAll(s.cfg.DBDir(), 0o755); err != nil { //nolint:gosec // G301: cocoon db dirs are shared runtime state
		return fmt.Errorf("create db dir: %w", err)
	}
	fl := flock.New(s.cfg.OCIBuildTagLock())
	if err := fl.Lock(); err != nil {
		return fmt.Errorf("acquire OCI build tag lock: %w", err)
	}
	defer fl.Unlock() //nolint:errcheck

	idx, err := s.load()
	if err != nil {
		return err
	}
	return fn(idx)
}

func (s *Store) withTxnLock(fn func() error) error {
	if err := os.MkdirAll(s.cfg.DBDir(), 0o755); err != nil { //nolint:gosec // G301: cocoon db dirs are shared runtime state
		return fmt.Errorf("create db dir: %w", err)
	}
	fl := flock.New(s.cfg.OCIBuildTxnLock())
	if err := fl.Lock(); err != nil {
		return fmt.Errorf("acquire OCI build txn lock: %w", err)
	}
	defer fl.Unlock() //nolint:errcheck
	return fn()
}

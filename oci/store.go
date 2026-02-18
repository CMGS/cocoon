package oci

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/lock/flock"
	"github.com/CMGS/cocoon/utils"
)

// Store manages local OCI VM image builds and their tag index.
// Follows the flock + JSON pattern from image/refcache/index.go.
type Store struct {
	cfg *config.CocoonConfig
}

// NewStore creates a Store for the given config.
func NewStore(cfg *config.CocoonConfig) *Store {
	return &Store{cfg: cfg}
}

// LayoutDir returns the directory path for an OCI layout keyed by tag.
// Uses first 16 chars of sha256(tag) to avoid filesystem issues with
// registry references containing slashes and colons.
func (s *Store) LayoutDir(tag string) string {
	h := sha256.Sum256([]byte(tag))
	prefix := fmt.Sprintf("%x", h[:8]) // 16 hex chars
	return filepath.Join(s.cfg.OCILayoutDir(), prefix)
}

// SaveTag records a tag-to-layout mapping in the tag index.
// If the tag already exists with a different manifest digest, the old
// manifest's blob references are cleaned up via RemoveBlobRefs to prevent
// reference leaks.
func (s *Store) SaveTag(tag, layoutPath, manifestDigest string) error {
	var oldManifestDigest string
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
		return s.save(idx)
	})
	if err != nil {
		return err
	}
	// Clean up old manifest's blob refs outside the tag lock.
	if oldManifestDigest != "" {
		if _, refErr := RemoveBlobRefs(s.cfg, oldManifestDigest); refErr != nil {
			return fmt.Errorf("clean old manifest %s blob refs: %w", oldManifestDigest, refErr)
		}
	}
	return nil
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
func (s *Store) RemoveTag(tag string) (string, []string, error) {
	var manifestDigest string
	var layoutPath string
	err := s.withLock(func(idx *TagIndex) error {
		entry, ok := idx.Tags[tag]
		if !ok {
			return fmt.Errorf("tag %q not found", tag)
		}
		manifestDigest = entry.ManifestDigest
		layoutPath = entry.LayoutPath
		delete(idx.Tags, tag)
		return s.save(idx)
	})
	if err != nil {
		return "", nil, err
	}

	// Remove layout directory (best-effort, outside lock).
	if layoutPath != "" {
		_ = os.RemoveAll(layoutPath)
	}

	// Clean up blob references for this manifest.
	var zeroRefBlobs []string
	if manifestDigest != "" {
		var refErr error
		zeroRefBlobs, refErr = RemoveBlobRefs(s.cfg, manifestDigest)
		if refErr != nil {
			return manifestDigest, nil, fmt.Errorf("remove blob refs for %s: %w", manifestDigest, refErr)
		}
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

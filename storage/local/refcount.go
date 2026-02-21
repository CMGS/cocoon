package local

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/lock/jsonstore"
	"github.com/CMGS/cocoon/storage"
	"github.com/CMGS/cocoon/types"
)

// Compile-time interface check.
var _ storage.ReferenceCounter = (*fileReferenceCounter)(nil)

// fileReferenceCounter implements ReferenceCounter using flock-protected
// atomic JSON persistence.
//
// Locking: every public method acquires references.lock (Level 2) for the
// duration of its read-modify-write cycle.  The lock is released before
// returning, keeping hold times to a few milliseconds.
type fileReferenceCounter struct {
	cfg       *config.CocoonConfig
	refsStore *jsonstore.Store[types.ReferencesFile]
}

// NewReferenceCounter creates a ReferenceCounter backed by references.json.
func NewReferenceCounter(cfg *config.CocoonConfig) storage.ReferenceCounter {
	return &fileReferenceCounter{
		cfg: cfg,
		refsStore: jsonstore.New(
			cfg.ReferencesLock(),
			cfg.ReferencesFile(),
			func() *types.ReferencesFile {
				m := make(types.ReferencesFile)
				return &m
			},
		),
	}
}

// AddReference pins vmID to baseKey.  Collision detection compares digestFull
// when the key already exists.  Returns types.ErrChecksumCollision on mismatch.
func (rc *fileReferenceCounter) AddReference(baseKey, vmID, digestFull, sourceRef string) error {
	return rc.refsStore.Update(func(refsPtr *types.ReferencesFile) error {
		refs := *refsPtr

		entry := refs[baseKey]
		if entry != nil {
			// Collision check: same truncated key but different full digest.
			if digestFull != "" && entry.DigestFull != "" && entry.DigestFull != digestFull {
				return fmt.Errorf(
					"checksum collision: base_key %s already maps to a different image "+
						"(stored: %s..., incoming: %s...): %w",
					baseKey, safePrefix(entry.DigestFull, 16), safePrefix(digestFull, 16),
					types.ErrChecksumCollision,
				)
			}
		} else {
			entry = &types.ReferenceEntry{
				Path:       rc.cfg.BaseImagePath(baseKey),
				DigestFull: digestFull,
				SourceRef:  sourceRef,
				Refs:       []string{},
				CreatedAt:  time.Now().UTC().Format(time.RFC3339),
			}
			refs[baseKey] = entry
		}

		// Idempotent: skip if vmID is already present.
		if slices.Contains(entry.Refs, vmID) {
			return nil
		}
		entry.Refs = append(entry.Refs, vmID)

		return nil
	})
}

// RemoveReference unpins vmID from baseKey.  Deletes the entry when the last
// reference is removed.
func (rc *fileReferenceCounter) RemoveReference(baseKey, vmID string) error {
	return rc.refsStore.Update(func(refsPtr *types.ReferencesFile) error {
		refs := *refsPtr

		entry := refs[baseKey]
		if entry == nil {
			return nil // nothing to do
		}

		filtered := make([]string, 0, len(entry.Refs))
		for _, id := range entry.Refs {
			if id != vmID {
				filtered = append(filtered, id)
			}
		}

		if len(filtered) == 0 {
			delete(refs, baseKey)
		} else {
			entry.Refs = filtered
		}

		return nil
	})
}

// GetReferences returns the VM IDs currently pinning baseKey.
func (rc *fileReferenceCounter) GetReferences(baseKey string) ([]string, error) {
	var result []string

	err := rc.refsStore.Read(func(refsPtr *types.ReferencesFile) error {
		refs := *refsPtr
		entry := refs[baseKey]
		if entry != nil && len(entry.Refs) > 0 {
			result = make([]string, len(entry.Refs))
			copy(result, entry.Refs)
		} else {
			result = []string{}
		}
		return nil
	})

	return result, err
}

// IsReferenced reports whether baseKey has at least one VM reference.
func (rc *fileReferenceCounter) IsReferenced(baseKey string) (bool, error) {
	var referenced bool

	err := rc.refsStore.Read(func(refsPtr *types.ReferencesFile) error {
		refs := *refsPtr
		entry := refs[baseKey]
		referenced = entry != nil && len(entry.Refs) > 0
		return nil
	})

	return referenced, err
}

// GetUnreferencedImages scans cache/images/ and returns base_keys that have
// no entry (or an empty refs list) in references.json.
func (rc *fileReferenceCounter) GetUnreferencedImages() ([]string, error) {
	var unreferenced []string

	err := rc.refsStore.Read(func(refsPtr *types.ReferencesFile) error {
		refs := *refsPtr

		// Scan image cache directory.
		pattern := filepath.Join(rc.cfg.ImageCacheDir(), "*.qcow2")
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return fmt.Errorf("glob image cache: %w", err)
		}

		for _, path := range matches {
			// Derive base_key from filename: strip directory and .qcow2 extension.
			baseKey := strings.TrimSuffix(filepath.Base(path), ".qcow2")
			entry := refs[baseKey]
			if entry == nil || len(entry.Refs) == 0 {
				unreferenced = append(unreferenced, baseKey)
			}
		}
		return nil
	})

	if unreferenced == nil {
		unreferenced = []string{}
	}
	return unreferenced, err
}

// safePrefix returns the first n characters of s, or the entire string if
// shorter than n. Prevents index-out-of-range panics on corrupt data.
func safePrefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

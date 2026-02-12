package local

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/lock/flock"
	"github.com/CMGS/cocoon/storage"
	"github.com/CMGS/cocoon/types"
	"github.com/CMGS/cocoon/utils"
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
	cfg *config.CocoonConfig
}

// NewReferenceCounter creates a ReferenceCounter backed by references.json.
func NewReferenceCounter(cfg *config.CocoonConfig) storage.ReferenceCounter {
	return &fileReferenceCounter{cfg: cfg}
}

// --- helpers ---

// withRefsLock acquires references.lock, runs fn, then releases the lock.
func (rc *fileReferenceCounter) withRefsLock(fn func() error) error {
	fl := flock.New(rc.cfg.ReferencesLock())
	if err := fl.Lock(); err != nil {
		return fmt.Errorf("acquire references.lock: %w", err)
	}
	defer fl.Unlock() //nolint:errcheck

	return fn()
}

// loadRefs reads and unmarshals references.json.  If the file does not exist
// an empty map is returned (not an error).
func (rc *fileReferenceCounter) loadRefs() (types.ReferencesFile, error) {
	refs := make(types.ReferencesFile)
	err := utils.ReadJSON(rc.cfg.ReferencesFile(), &refs)
	if err != nil {
		if os.IsNotExist(err) {
			return refs, nil
		}
		return nil, fmt.Errorf("load references.json: %w", err)
	}
	return refs, nil
}

// saveRefs atomically persists refs to references.json (temp + fsync + rename).
func (rc *fileReferenceCounter) saveRefs(refs types.ReferencesFile) error {
	if err := utils.AtomicWriteJSON(rc.cfg.ReferencesFile(), refs); err != nil {
		return fmt.Errorf("save references.json: %w", err)
	}
	return nil
}

// --- ReferenceCounter interface ---

// AddReference pins vmID to baseKey.  Collision detection compares digestFull
// when the key already exists.  Returns types.ErrChecksumCollision on mismatch.
func (rc *fileReferenceCounter) AddReference(baseKey, vmID, digestFull, sourceRef string) error {
	return rc.withRefsLock(func() error {
		refs, err := rc.loadRefs()
		if err != nil {
			return err
		}

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
			return rc.saveRefs(refs)
		}
		entry.Refs = append(entry.Refs, vmID)

		return rc.saveRefs(refs)
	})
}

// RemoveReference unpins vmID from baseKey.  Deletes the entry when the last
// reference is removed.
func (rc *fileReferenceCounter) RemoveReference(baseKey, vmID string) error {
	return rc.withRefsLock(func() error {
		refs, err := rc.loadRefs()
		if err != nil {
			return err
		}

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

		return rc.saveRefs(refs)
	})
}

// GetReferences returns the VM IDs currently pinning baseKey.
func (rc *fileReferenceCounter) GetReferences(baseKey string) ([]string, error) {
	var result []string

	err := rc.withRefsLock(func() error {
		refs, err := rc.loadRefs()
		if err != nil {
			return err
		}
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

	err := rc.withRefsLock(func() error {
		refs, err := rc.loadRefs()
		if err != nil {
			return err
		}
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

	err := rc.withRefsLock(func() error {
		refs, err := rc.loadRefs()
		if err != nil {
			return err
		}

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

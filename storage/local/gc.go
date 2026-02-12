package local

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/lock/flock"
	"github.com/CMGS/cocoon/storage"
	"github.com/CMGS/cocoon/types"
	"github.com/CMGS/cocoon/utils"
)

// Compile-time interface check.
var _ storage.GarbageCollector = (*fileGarbageCollector)(nil)

// fileGarbageCollector implements GarbageCollector.
//
// Locking order (docs/06-concurrency.md):
//  1. gc.lock       (Level 1) -- acquired once per GC cycle.
//  2. references.lock (Level 2) -- acquired per-image for atomic check-and-delete.
//
// Never acquire Level 1 while holding Level 2.
type fileGarbageCollector struct {
	cfg *config.CocoonConfig
}

// NewGarbageCollector creates a GarbageCollector backed by the filesystem
// layout defined in config.
func NewGarbageCollector(cfg *config.CocoonConfig) storage.GarbageCollector {
	return &fileGarbageCollector{cfg: cfg}
}

// --- helpers ---

// withGCLock acquires gc.lock (Level 1) for the duration of fn.
func (gc *fileGarbageCollector) withGCLock(fn func() error) error {
	fl := flock.New(gc.cfg.GCLock())
	if err := fl.Lock(); err != nil {
		return fmt.Errorf("acquire gc.lock: %w", err)
	}
	defer fl.Unlock() //nolint:errcheck

	return fn()
}

// withRefsLock acquires references.lock (Level 2) for the duration of fn.
// Must only be called while gc.lock (Level 1) is already held.
func (gc *fileGarbageCollector) withRefsLock(fn func(refs types.ReferencesFile) (types.ReferencesFile, error)) error {
	fl := flock.New(gc.cfg.ReferencesLock())
	if err := fl.Lock(); err != nil {
		return fmt.Errorf("acquire references.lock: %w", err)
	}
	defer fl.Unlock() //nolint:errcheck

	refs := make(types.ReferencesFile)
	err := utils.ReadJSON(gc.cfg.ReferencesFile(), &refs)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("load references.json: %w", err)
	}

	updated, err := fn(refs)
	if err != nil {
		return err
	}

	// Persist if the callback returned an updated map.
	if updated != nil {
		if err := utils.AtomicWriteJSON(gc.cfg.ReferencesFile(), updated); err != nil {
			return fmt.Errorf("save references.json: %w", err)
		}
	}
	return nil
}

// --- GarbageCollector interface ---

// CollectUnreferencedImages soft-deletes base images with zero references
// whose mtime is older than gracePeriod.
//
// Lock order: gc.lock (L1) -> references.lock (L2) per image.
func (gc *fileGarbageCollector) CollectUnreferencedImages(gracePeriod time.Duration) ([]string, error) {
	var collected []string

	err := gc.withGCLock(func() error {
		// Scan cached images outside references.lock to minimize hold time.
		pattern := filepath.Join(gc.cfg.ImageCacheDir(), "*.qcow2")
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return fmt.Errorf("glob image cache: %w", err)
		}

		cutoff := time.Now().Add(-gracePeriod)

		for _, imagePath := range matches {
			baseKey := strings.TrimSuffix(filepath.Base(imagePath), ".qcow2")

			// Atomic check-and-delete under references.lock.
			// Both the reference check AND the file rename happen inside the
			// lock to prevent a TOCTOU race where a new VM pins a reference
			// between the check and the rename.
			var didCollect bool
			if err := gc.withRefsLock(func(refs types.ReferencesFile) (types.ReferencesFile, error) {
				entry := refs[baseKey]
				if entry != nil && len(entry.Refs) > 0 {
					return nil, nil // still referenced
				}

				fi, err := os.Stat(imagePath)
				if err != nil {
					return nil, nil // vanished, skip
				}
				if fi.ModTime().After(cutoff) {
					return nil, nil // too recent
				}

				// Move the image to trash while still holding the lock.
				trashPath := filepath.Join(gc.cfg.TrashDir(), filepath.Base(imagePath))
				if err := os.Rename(imagePath, trashPath); err != nil {
					return nil, nil // non-fatal: skip this image
				}

				didCollect = true

				// Remove the entry from references.json if it exists
				// (might be a zero-ref entry or absent entirely).
				if entry != nil {
					delete(refs, baseKey)
					return refs, nil
				}
				return nil, nil
			}); err != nil {
				return err
			}

			if didCollect {
				collected = append(collected, baseKey)
			}
		}
		return nil
	})

	if collected == nil {
		collected = []string{}
	}
	return collected, err
}

// CollectOrphanedOverlays finds VM directories where overlay.qcow2 exists
// but config.json is missing, and moves them to trash/.
//
// Lock: gc.lock (L1) only.
func (gc *fileGarbageCollector) CollectOrphanedOverlays() ([]string, error) {
	var collected []string

	err := gc.withGCLock(func() error {
		entries, err := os.ReadDir(gc.cfg.VMDir())
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("read VM directory: %w", err)
		}

		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}

			vmID := entry.Name()
			overlayPath := filepath.Join(gc.cfg.VMDir(), vmID, "overlay.qcow2")
			configPath := filepath.Join(gc.cfg.VMDir(), vmID, "config.json")

			overlayExists := fileExists(overlayPath)
			configExists := fileExists(configPath)

			if overlayExists && !configExists {
				// Orphaned overlay -- move to trash.
				trashName := vmID + "-orphan-overlay.qcow2"
				trashPath := filepath.Join(gc.cfg.TrashDir(), trashName)
				if err := os.Rename(overlayPath, trashPath); err != nil {
					continue // non-fatal
				}
				// Clean up the now-empty VM directory.
				_ = os.RemoveAll(filepath.Join(gc.cfg.VMDir(), vmID))
				collected = append(collected, vmID)
			}
		}
		return nil
	})

	if collected == nil {
		collected = []string{}
	}
	return collected, err
}

// CollectTempFiles removes files in temp/ older than maxAge.
//
// Lock: gc.lock (L1) only.
func (gc *fileGarbageCollector) CollectTempFiles(maxAge time.Duration) ([]string, error) {
	var collected []string

	err := gc.withGCLock(func() error {
		entries, err := os.ReadDir(gc.cfg.TempDir())
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("read temp directory: %w", err)
		}

		cutoff := time.Now().Add(-maxAge)

		for _, entry := range entries {
			info, err := entry.Info()
			if err != nil {
				continue
			}
			if info.ModTime().Before(cutoff) {
				path := filepath.Join(gc.cfg.TempDir(), entry.Name())
				if err := os.Remove(path); err != nil {
					continue // non-fatal
				}
				collected = append(collected, entry.Name())
			}
		}
		return nil
	})

	if collected == nil {
		collected = []string{}
	}
	return collected, err
}

// EmptyTrash permanently deletes items in trash/ older than maxAge.
//
// Lock: gc.lock (L1) only.
func (gc *fileGarbageCollector) EmptyTrash(maxAge time.Duration) error {
	return gc.withGCLock(func() error {
		entries, err := os.ReadDir(gc.cfg.TrashDir())
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("read trash directory: %w", err)
		}

		cutoff := time.Now().Add(-maxAge)

		for _, entry := range entries {
			info, err := entry.Info()
			if err != nil {
				continue
			}
			if info.ModTime().Before(cutoff) {
				path := filepath.Join(gc.cfg.TrashDir(), entry.Name())
				_ = os.RemoveAll(path) // best effort
			}
		}
		return nil
	})
}

// FullGC runs a complete garbage collection cycle using the grace periods
// from configuration.
//
// Each phase acquires gc.lock independently. This is NOT atomic across the
// full cycle -- an interleaving VM creation between phases is safe because
// each phase performs its own reference check under lock.
func (gc *fileGarbageCollector) FullGC() error {
	gracePeriod := time.Duration(gc.cfg.GCGracePeriodHours) * time.Hour
	trashRetention := time.Duration(gc.cfg.GCTrashRetentDays) * 24 * time.Hour
	tempMaxAge := 1 * time.Hour

	if _, err := gc.CollectUnreferencedImages(gracePeriod); err != nil {
		return fmt.Errorf("collect unreferenced images: %w", err)
	}

	if _, err := gc.CollectOrphanedOverlays(); err != nil {
		return fmt.Errorf("collect orphaned overlays: %w", err)
	}

	if _, err := gc.CollectTempFiles(tempMaxAge); err != nil {
		return fmt.Errorf("collect temp files: %w", err)
	}

	if err := gc.EmptyTrash(trashRetention); err != nil {
		return fmt.Errorf("empty trash: %w", err)
	}

	return nil
}

// fileExists reports whether path exists on the filesystem.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

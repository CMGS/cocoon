package local

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
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

// CollectUnreferencedImages permanently deletes base images with zero references.
//
// Lock order: gc.lock (L1) -> references.lock (L2) per image.
func (gc *fileGarbageCollector) CollectUnreferencedImages() ([]string, error) {
	var collected []string

	err := gc.withGCLock(func() error {
		// Scan cached images outside references.lock to minimize hold time.
		pattern := filepath.Join(gc.cfg.ImageCacheDir(), "*.qcow2")
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return fmt.Errorf("glob image cache: %w", err)
		}

		for _, imagePath := range matches {
			baseKey := strings.TrimSuffix(filepath.Base(imagePath), ".qcow2")

			// Atomic check-and-delete under references.lock.
			// Both the reference check AND the file removal happen inside the
			// lock to prevent a TOCTOU race where a new VM pins a reference
			// between the check and the delete.
			var didCollect bool
			if err := gc.withRefsLock(func(refs types.ReferencesFile) (types.ReferencesFile, error) {
				entry := refs[baseKey]
				if entry != nil && len(entry.Refs) > 0 {
					return nil, nil // still referenced
				}

				// Permanently delete the image while still holding the lock.
				if err := os.Remove(imagePath); err != nil {
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
// but config.json is missing, and permanently deletes them.
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

			overlayExists, overlayErr := fileExists(overlayPath)
			if overlayErr != nil {
				log.Printf("gc: stat overlay for %s: %v", vmID, overlayErr)
				continue
			}
			configExists, configErr := fileExists(configPath)
			if configErr != nil {
				log.Printf("gc: stat config for %s: %v", vmID, configErr)
				continue
			}

			if overlayExists && !configExists {
				// Orphaned overlay -- permanently delete the VM directory.
				vmDir := filepath.Join(gc.cfg.VMDir(), vmID)
				if err := os.RemoveAll(vmDir); err != nil {
					log.Printf("gc: remove orphaned VM dir %s: %v", vmDir, err)
					continue
				}
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

// CollectTempFiles removes entries (files/directories) in temp/ older than maxAge.
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
				if err := os.RemoveAll(path); err != nil {
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

// CollectStaleConversionLocks removes stale conversion lock files from
// cache/locks/ that meet all of these conditions:
//   - lock file mtime is older than maxAge
//   - corresponding base image qcow2 does not exist
//   - lock is not currently held (TryLock succeeds)
//
// Lock: gc.lock (L1) only.
//
// This is best-effort hygiene to clean lock files left behind after image
// deletion. Active lock files are preserved.
func (gc *fileGarbageCollector) CollectStaleConversionLocks(maxAge time.Duration) ([]string, error) {
	var collected []string

	err := gc.withGCLock(func() error {
		entries, err := os.ReadDir(gc.cfg.ConversionLockDir())
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("read conversion lock directory: %w", err)
		}

		cutoff := time.Now().Add(-maxAge)
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			if !strings.HasSuffix(name, ".lock") {
				continue
			}

			info, infoErr := entry.Info()
			if infoErr != nil {
				continue
			}
			// Defense-in-depth: skip recently touched lock files.
			if info.ModTime().After(cutoff) {
				continue
			}

			baseKey := strings.TrimSuffix(name, ".lock")
			basePath := gc.cfg.BaseImagePath(baseKey)
			if _, statErr := os.Stat(basePath); statErr == nil {
				// Base image exists: keep lock file.
				continue
			} else if !os.IsNotExist(statErr) {
				// Unexpected stat error: skip.
				continue
			}

			lockPath := filepath.Join(gc.cfg.ConversionLockDir(), name)
			fl := flock.New(lockPath)
			ok, tryErr := fl.TryLock()
			if tryErr != nil || !ok {
				// Held by another process or transient lock error.
				continue
			}

			// Remove while lock is held, then release.
			removeErr := os.Remove(lockPath)
			unlockErr := fl.Unlock()
			if removeErr != nil {
				if unlockErr != nil {
					log.Printf("gc: failed to release conversion lock %s after remove error: %v", lockPath, unlockErr)
				}
				continue
			}
			if unlockErr != nil {
				log.Printf("gc: failed to release conversion lock %s after remove: %v", lockPath, unlockErr)
			}

			collected = append(collected, name)
		}
		return nil
	})

	if collected == nil {
		collected = []string{}
	}
	sort.Strings(collected)
	return collected, err
}

// FullGC runs a complete garbage collection cycle.
//
// Each phase acquires gc.lock independently. This is NOT atomic across the
// full cycle -- an interleaving VM creation between phases is safe because
// each phase performs its own reference check under lock.
func (gc *fileGarbageCollector) FullGC() error {
	tempMaxAge := 1 * time.Hour
	lockMaxAge := storage.OCIGCGracePeriod

	if _, err := gc.CollectUnreferencedImages(); err != nil {
		return fmt.Errorf("collect unreferenced images: %w", err)
	}

	if _, err := gc.CollectOrphanedOverlays(); err != nil {
		return fmt.Errorf("collect orphaned overlays: %w", err)
	}

	if _, err := gc.CollectOrphanedOCILayouts(); err != nil {
		return fmt.Errorf("collect orphaned OCI layouts: %w", err)
	}

	if _, err := gc.CollectStaleOCITags(); err != nil {
		return fmt.Errorf("collect stale OCI tags: %w", err)
	}

	if _, err := gc.CollectOrphanedOCIManifestRefs(); err != nil {
		return fmt.Errorf("collect orphaned OCI manifest refs: %w", err)
	}

	if _, err := gc.CollectUnreferencedOCIBlobs(); err != nil {
		return fmt.Errorf("collect unreferenced OCI blobs: %w", err)
	}

	if _, err := gc.CollectStaleConversionLocks(lockMaxAge); err != nil {
		return fmt.Errorf("collect stale conversion locks: %w", err)
	}

	if _, err := gc.CollectTempFiles(tempMaxAge); err != nil {
		return fmt.Errorf("collect temp files: %w", err)
	}

	return nil
}

// fileExists reports whether path exists on the filesystem.
// It returns os.Stat errors other than not-exist so callers can avoid
// false "missing" conclusions on transient I/O failures.
func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

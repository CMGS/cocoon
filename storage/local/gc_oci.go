package local

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/CMGS/cocoon/lock/flock"
	"github.com/CMGS/cocoon/utils"
)

// --- Local struct definitions for reading OCI JSON files ---
// These duplicate minimal fields from oci.TagIndex and oci.LayerRefsIndex
// to avoid importing the oci package (prevents circular dependency).

type ociTagIndex struct {
	Tags map[string]ociTagEntry `json:"tags"`
}

type ociTagEntry struct {
	LayoutPath     string `json:"layout_path"`
	ManifestDigest string `json:"manifest_digest"`
}

type ociLayerRefsIndex struct {
	Blobs map[string]ociBlobRefEntry `json:"blobs"`
}

type ociBlobRefEntry struct {
	ManifestDigests []string  `json:"manifest_digests"`
	Size            int64     `json:"size"`
	CreatedAt       time.Time `json:"created_at"`
}

// CollectOrphanedOCILayouts removes OCI layout directories in cache/oci/layouts/
// that are not referenced by any tag in oci-build-tags.json.
//
// Lock: gc.lock (L1) only.
func (gc *fileGarbageCollector) CollectOrphanedOCILayouts() ([]string, error) {
	var collected []string

	err := gc.withGCLock(func() error {
		layoutsDir := gc.cfg.OCILayoutDir()
		entries, err := os.ReadDir(layoutsDir)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("read OCI layouts dir: %w", err)
		}

		// Load tag index under its lock to prevent TOCTOU with concurrent SaveTag.
		knownLayouts := make(map[string]bool)
		tagIdx := &ociTagIndex{Tags: make(map[string]ociTagEntry)}
		tagLock := flock.New(gc.cfg.OCIBuildTagLock())
		if err := tagLock.Lock(); err != nil {
			return fmt.Errorf("acquire OCI build tag lock: %w", err)
		}
		tagIdxPath := gc.cfg.OCIBuildTagIndex()
		if _, statErr := os.Stat(tagIdxPath); statErr == nil {
			if readErr := utils.ReadJSON(tagIdxPath, tagIdx); readErr != nil {
				tagLock.Unlock()
				return fmt.Errorf("read tag index: %w", readErr)
			}
		}
		tagLock.Unlock()

		for _, entry := range tagIdx.Tags {
			knownLayouts[entry.LayoutPath] = true
		}

		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			layoutPath := filepath.Join(layoutsDir, entry.Name())
			if knownLayouts[layoutPath] {
				continue // referenced by a tag
			}

			// Orphaned layout -- remove it.
			if err := os.RemoveAll(layoutPath); err != nil {
				continue // non-fatal
			}
			collected = append(collected, entry.Name())
		}

		return nil
	})

	if collected == nil {
		collected = []string{}
	}
	return collected, err
}

// CollectUnreferencedOCIBlobs removes blobs from cache/oci/blobs/sha256/
// that have zero manifest references in oci-layer-refs.json.
//
// Lock: gc.lock (L1) -> oci-layer-refs.lock for atomic check-and-delete.
func (gc *fileGarbageCollector) CollectUnreferencedOCIBlobs() ([]string, error) {
	var collected []string

	err := gc.withGCLock(func() error {
		blobDir := gc.cfg.OCIBlobDir()
		entries, err := os.ReadDir(blobDir)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("read OCI blob dir: %w", err)
		}

		// Load layer refs under lock.
		fl := flock.New(gc.cfg.OCILayerRefsLock())
		if err := fl.Lock(); err != nil {
			return fmt.Errorf("acquire OCI layer refs lock: %w", err)
		}
		defer fl.Unlock() //nolint:errcheck

		layerRefs := &ociLayerRefsIndex{Blobs: make(map[string]ociBlobRefEntry)}
		refsPath := gc.cfg.OCILayerRefsFile()
		if _, statErr := os.Stat(refsPath); statErr == nil {
			if readErr := utils.ReadJSON(refsPath, layerRefs); readErr != nil {
				return fmt.Errorf("read layer refs: %w", readErr)
			}
		}

		changed := false
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			digest := entry.Name()

			// Check if blob has any manifest references.
			blobRef, exists := layerRefs.Blobs[digest]
			if exists && len(blobRef.ManifestDigests) > 0 {
				continue // still referenced
			}

			// Unreferenced blob -- remove it.
			blobPath := filepath.Join(blobDir, digest)
			if err := os.Remove(blobPath); err != nil {
				continue // non-fatal
			}

			// Clean up the entry from layer refs.
			if exists {
				delete(layerRefs.Blobs, digest)
				changed = true
			}

			collected = append(collected, digest)
		}

		// Persist updated layer refs if changed.
		if changed {
			if err := utils.AtomicWriteJSON(refsPath, layerRefs); err != nil {
				return fmt.Errorf("save layer refs: %w", err)
			}
		}

		return nil
	})

	if collected == nil {
		collected = []string{}
	}
	return collected, err
}

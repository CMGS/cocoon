package local

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/CMGS/cocoon/lock/flock"
	"github.com/CMGS/cocoon/oci"
	"github.com/CMGS/cocoon/storage"
	"github.com/CMGS/cocoon/utils"
)

// OCI GC lock hierarchy (must remain consistent with docs/06-concurrency.md):
// 1. gc.lock (L1) is always outermost for all GC phases.
// 2. Build/index phases then acquire:
//   - oci-build-txn.lock
//   - oci-build-tags.lock
//   - oci-layer-refs.lock
// 3. Runtime cache phases acquire:
//   - oci-runtime-refs.lock
// Never acquire oci-runtime-refs.lock while holding oci-build-tags.lock or
// oci-layer-refs.lock.

// CollectOrphanedOCILayouts removes OCI layout directories in cache/oci/layouts/
// that are not referenced by any tag in oci-build-tags.json.
//
// Lock: gc.lock (L1) -> oci-build-txn.lock -> oci-build-tags.lock.
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

		// Serialize with concurrent build finalize+SaveTag operations.
		txnLock := flock.New(gc.cfg.OCIBuildTxnLock())
		if err := txnLock.Lock(); err != nil {
			return fmt.Errorf("acquire OCI build txn lock: %w", err)
		}
		defer txnLock.Unlock() //nolint:errcheck

		// Load tag index under its lock and hold the lock through the entire
		// orphan collection loop. This prevents a concurrent SaveTag from
		// adding a tag to a layout we are about to delete.
		knownLayouts := make(map[string]bool)
		tagIdx := &oci.TagIndex{Tags: make(map[string]oci.TagEntry)}
		tagLock := flock.New(gc.cfg.OCIBuildTagLock())
		if err := tagLock.Lock(); err != nil {
			return fmt.Errorf("acquire OCI build tag lock: %w", err)
		}
		defer tagLock.Unlock() //nolint:errcheck
		tagIdxPath := gc.cfg.OCIBuildTagIndex()
		if _, statErr := os.Stat(tagIdxPath); statErr == nil {
			if readErr := utils.ReadJSON(tagIdxPath, tagIdx); readErr != nil {
				return fmt.Errorf("read tag index: %w", readErr)
			}
		}

		for _, entry := range tagIdx.Tags {
			knownLayouts[entry.LayoutPath] = true
		}

		cutoff := time.Now().Add(-storage.OCIGCGracePeriod)

		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			layoutPath := filepath.Join(layoutsDir, entry.Name())
			if knownLayouts[layoutPath] {
				continue // referenced by a tag
			}

			// Defense-in-depth: skip recently-created layouts that may be
			// from a build in progress (not yet recorded in tag index).
			if info, statErr := entry.Info(); statErr == nil && info.ModTime().After(cutoff) {
				continue
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

// CollectStaleOCITags removes tags from oci-build-tags.json whose layout_path
// no longer exists on disk. For each stale tag, if its ManifestDigest becomes
// orphaned (no other tag shares it), the manifest digest is removed from all
// blob entries in oci-layer-refs.json and zero-ref blobs are deleted.
//
// Lock: gc.lock (L1) -> oci-build-txn.lock -> oci-build-tags.lock + oci-layer-refs.lock.
func (gc *fileGarbageCollector) CollectStaleOCITags() ([]string, error) {
	var collected []string

	err := gc.withGCLock(func() error {
		// Acquire txn lock to serialize with concurrent builds.
		txnLock := flock.New(gc.cfg.OCIBuildTxnLock())
		if err := txnLock.Lock(); err != nil {
			return fmt.Errorf("acquire OCI build txn lock: %w", err)
		}
		defer txnLock.Unlock() //nolint:errcheck

		// Acquire tag lock and load index.
		tagLock := flock.New(gc.cfg.OCIBuildTagLock())
		if err := tagLock.Lock(); err != nil {
			return fmt.Errorf("acquire OCI build tag lock: %w", err)
		}
		defer tagLock.Unlock() //nolint:errcheck

		tagIdx := &oci.TagIndex{Tags: make(map[string]oci.TagEntry)}
		tagIdxPath := gc.cfg.OCIBuildTagIndex()
		if _, statErr := os.Stat(tagIdxPath); statErr == nil {
			if readErr := utils.ReadJSON(tagIdxPath, tagIdx); readErr != nil {
				return fmt.Errorf("read tag index: %w", readErr)
			}
		}

		// Find stale tags (layout path missing).
		type staleInfo struct {
			tag            string
			manifestDigest string
		}
		var staleTags []staleInfo

		for tag, entry := range tagIdx.Tags {
			if _, statErr := os.Stat(entry.LayoutPath); os.IsNotExist(statErr) {
				staleTags = append(staleTags, staleInfo{tag: tag, manifestDigest: entry.ManifestDigest})
			}
		}

		if len(staleTags) == 0 {
			return nil
		}

		// Remove stale tags from index.
		for _, s := range staleTags {
			delete(tagIdx.Tags, s.tag)
			collected = append(collected, s.tag)
		}

		// Persist updated tag index.
		if err := utils.AtomicWriteJSON(tagIdxPath, tagIdx); err != nil {
			return fmt.Errorf("save tag index: %w", err)
		}

		// Build set of live manifest digests from remaining tags.
		liveManifests := make(map[string]bool)
		for _, entry := range tagIdx.Tags {
			liveManifests[entry.ManifestDigest] = true
		}

		// Find orphaned manifest digests from stale tags.
		var orphanedManifests []string
		for _, s := range staleTags {
			if !liveManifests[s.manifestDigest] {
				orphanedManifests = append(orphanedManifests, s.manifestDigest)
			}
		}

		if len(orphanedManifests) == 0 {
			return nil
		}

		// Cascade: remove orphaned manifest refs from layer-refs index.
		refsLock := flock.New(gc.cfg.OCILayerRefsLock())
		if err := refsLock.Lock(); err != nil {
			return fmt.Errorf("acquire OCI layer refs lock: %w", err)
		}
		defer refsLock.Unlock() //nolint:errcheck

		layerRefs := &oci.LayerRefsIndex{Blobs: make(map[string]oci.BlobRefEntry)}
		refsPath := gc.cfg.OCILayerRefsFile()
		if _, statErr := os.Stat(refsPath); statErr == nil {
			if readErr := utils.ReadJSON(refsPath, layerRefs); readErr != nil {
				return fmt.Errorf("read layer refs: %w", readErr)
			}
		}

		orphanSet := make(map[string]bool, len(orphanedManifests))
		for _, d := range orphanedManifests {
			orphanSet[d] = true
		}

		changed := false
		cutoff := time.Now().Add(-storage.OCIGCGracePeriod)
		blobDir := gc.cfg.OCIBlobDir()
		for digest, blobRef := range layerRefs.Blobs {
			filtered := filterManifestDigests(blobRef.ManifestDigests, orphanSet)
			if len(filtered) != len(blobRef.ManifestDigests) {
				blobRef.ManifestDigests = filtered
				layerRefs.Blobs[digest] = blobRef
				changed = true

				// If zero refs remain, try to delete the blob file.
				if len(filtered) == 0 && tryDeleteZeroRefBlob(blobDir, digest, cutoff) {
					delete(layerRefs.Blobs, digest)
				}
			}
		}

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

// CollectOrphanedOCIManifestRefs removes manifest digest references from
// oci-layer-refs.json that are not associated with any live tag in
// oci-build-tags.json. Blobs becoming zero-ref are deleted (with a 5-min
// grace period for in-progress builds).
//
// Lock: gc.lock (L1) -> oci-build-txn.lock -> tags.lock + layer-refs.lock.
func (gc *fileGarbageCollector) CollectOrphanedOCIManifestRefs() ([]string, error) {
	var collected []string

	err := gc.withGCLock(func() error {
		// Acquire txn lock.
		txnLock := flock.New(gc.cfg.OCIBuildTxnLock())
		if err := txnLock.Lock(); err != nil {
			return fmt.Errorf("acquire OCI build txn lock: %w", err)
		}
		defer txnLock.Unlock() //nolint:errcheck

		// Load tag index to build live manifest set.
		tagLock := flock.New(gc.cfg.OCIBuildTagLock())
		if err := tagLock.Lock(); err != nil {
			return fmt.Errorf("acquire OCI build tag lock: %w", err)
		}
		defer tagLock.Unlock() //nolint:errcheck

		tagIdx := &oci.TagIndex{Tags: make(map[string]oci.TagEntry)}
		tagIdxPath := gc.cfg.OCIBuildTagIndex()
		if _, statErr := os.Stat(tagIdxPath); statErr == nil {
			if readErr := utils.ReadJSON(tagIdxPath, tagIdx); readErr != nil {
				return fmt.Errorf("read tag index: %w", readErr)
			}
		}

		liveManifests := make(map[string]bool)
		for _, entry := range tagIdx.Tags {
			liveManifests[entry.ManifestDigest] = true
		}

		// Load layer refs.
		refsLock := flock.New(gc.cfg.OCILayerRefsLock())
		if err := refsLock.Lock(); err != nil {
			return fmt.Errorf("acquire OCI layer refs lock: %w", err)
		}
		defer refsLock.Unlock() //nolint:errcheck

		layerRefs := &oci.LayerRefsIndex{Blobs: make(map[string]oci.BlobRefEntry)}
		refsPath := gc.cfg.OCILayerRefsFile()
		if _, statErr := os.Stat(refsPath); statErr == nil {
			if readErr := utils.ReadJSON(refsPath, layerRefs); readErr != nil {
				return fmt.Errorf("read layer refs: %w", readErr)
			}
		}

		// Collect all orphaned manifest digests (in blob refs but not live).
		orphanedSet := make(map[string]bool)
		for _, blobRef := range layerRefs.Blobs {
			for _, md := range blobRef.ManifestDigests {
				if !liveManifests[md] {
					orphanedSet[md] = true
				}
			}
		}

		if len(orphanedSet) == 0 {
			return nil
		}

		// Filter orphaned manifests from blob entries.
		cutoff := time.Now().Add(-storage.OCIGCGracePeriod)
		blobDir := gc.cfg.OCIBlobDir()
		changed := false

		for digest, blobRef := range layerRefs.Blobs {
			filtered := filterManifestDigests(blobRef.ManifestDigests, orphanedSet)
			if len(filtered) != len(blobRef.ManifestDigests) {
				blobRef.ManifestDigests = filtered
				layerRefs.Blobs[digest] = blobRef
				changed = true

				// If zero refs remain, try to delete the blob.
				if len(filtered) == 0 && tryDeleteZeroRefBlob(blobDir, digest, cutoff) {
					delete(layerRefs.Blobs, digest)
				}
			}
		}

		if changed {
			if err := utils.AtomicWriteJSON(refsPath, layerRefs); err != nil {
				return fmt.Errorf("save layer refs: %w", err)
			}
		}

		for md := range orphanedSet {
			collected = append(collected, md)
		}
		sort.Strings(collected)

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

		layerRefs := &oci.LayerRefsIndex{Blobs: make(map[string]oci.BlobRefEntry)}
		refsPath := gc.cfg.OCILayerRefsFile()
		if _, statErr := os.Stat(refsPath); statErr == nil {
			if readErr := utils.ReadJSON(refsPath, layerRefs); readErr != nil {
				return fmt.Errorf("read layer refs: %w", readErr)
			}
		}

		cutoff := time.Now().Add(-storage.OCIGCGracePeriod)
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

			// Defense-in-depth: skip recently-created blobs that may be
			// from a build in progress (refs not yet recorded).
			if info, statErr := entry.Info(); statErr == nil && info.ModTime().After(cutoff) {
				continue
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

// CollectUnreferencedOCIRuntimeCaches removes unreferenced OCI runtime entries
// and orphaned layer cache directories using a two-phase approach:
//
// Phase 1 (Entry Sweep): scan entries/ directory and remove entries whose
// runtimeKey has no active pins in oci-runtime-refs.json.
//
// Phase 2 (Layer Mark-and-Sweep): scan surviving entry meta.json files to
// build a set of referenced layer digests, then remove any layer directory
// not in the referenced set.
//
// Lock: gc.lock (L1) -> oci-runtime-refs.lock.
func (gc *fileGarbageCollector) CollectUnreferencedOCIRuntimeCaches() ([]string, error) {
	var collected []string

	err := gc.withGCLock(func() error {
		refsLock := flock.New(gc.cfg.OCIRuntimeRefsLock())
		if err := refsLock.Lock(); err != nil {
			return fmt.Errorf("acquire OCI runtime refs lock: %w", err)
		}
		defer refsLock.Unlock() //nolint:errcheck

		refsIdx := &oci.RuntimeRefsIndex{Runtimes: make(map[string]oci.RuntimeRefEntry)}
		refsPath := gc.cfg.OCIRuntimeRefsFile()
		if _, statErr := os.Stat(refsPath); statErr == nil {
			if readErr := utils.ReadJSON(refsPath, refsIdx); readErr != nil {
				return fmt.Errorf("read OCI runtime refs: %w", readErr)
			}
		}

		cutoff := time.Now().Add(-storage.OCIGCGracePeriod)

		// Phase 1: Entry Sweep — remove unpinned entry dirs.
		swept, changed, sweepErr := gc.sweepOCIRuntimeEntries(refsIdx, cutoff)
		if sweepErr != nil {
			return sweepErr
		}
		collected = append(collected, swept...)

		if changed {
			if err := utils.AtomicWriteJSON(refsPath, refsIdx); err != nil {
				return fmt.Errorf("save OCI runtime refs: %w", err)
			}
		}

		// Phase 2: Layer Mark-and-Sweep.
		layerCollected, layerErr := gc.sweepOCIRuntimeLayers(cutoff)
		if layerErr != nil {
			return layerErr
		}
		collected = append(collected, layerCollected...)

		return nil
	})

	if collected == nil {
		collected = []string{}
	}
	return collected, err
}

// sweepOCIRuntimeEntries removes unpinned entry dirs and stale zero-ref index
// entries. Returns collected entry keys, whether refsIdx was mutated, and error.
func (gc *fileGarbageCollector) sweepOCIRuntimeEntries(refsIdx *oci.RuntimeRefsIndex, cutoff time.Time) ([]string, bool, error) {
	entriesDir := gc.cfg.OCIRuntimeEntriesDir()
	entries, err := os.ReadDir(entriesDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, false, fmt.Errorf("read OCI runtime entries dir: %w", err)
	}

	var collected []string
	changed := false

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		runtimeKey := entry.Name()
		refEntry, exists := refsIdx.Runtimes[runtimeKey]
		if exists && len(refEntry.Refs) > 0 {
			continue
		}

		// Defense-in-depth: skip recently-created entry dirs that may be
		// in the window between materialization and pin recording.
		if info, statErr := entry.Info(); statErr == nil && info.ModTime().After(cutoff) {
			continue
		}

		if removeErr := os.RemoveAll(gc.cfg.OCIRuntimeEntryDir(runtimeKey)); removeErr != nil {
			continue // non-fatal
		}
		collected = append(collected, "entry:"+runtimeKey)

		if exists {
			delete(refsIdx.Runtimes, runtimeKey)
			changed = true
		}
	}

	// Cleanup stale zero-ref entries even when their dirs were already gone.
	for runtimeKey, refEntry := range refsIdx.Runtimes {
		if len(refEntry.Refs) > 0 {
			continue
		}
		if _, statErr := os.Stat(gc.cfg.OCIRuntimeEntryDir(runtimeKey)); os.IsNotExist(statErr) {
			delete(refsIdx.Runtimes, runtimeKey)
			changed = true
		}
	}

	return collected, changed, nil
}

// sweepOCIRuntimeLayers collects referenced layers from surviving entry meta
// files, then removes unreferenced layer dirs.
func (gc *fileGarbageCollector) sweepOCIRuntimeLayers(cutoff time.Time) ([]string, error) {
	entriesDir := gc.cfg.OCIRuntimeEntriesDir()
	referencedLayers := make(map[string]struct{})
	survivingEntries, err := os.ReadDir(entriesDir)
	if err != nil && !os.IsNotExist(err) {
		// Transient/permission I/O errors: abort layer sweep to avoid
		// incorrectly treating valid layers as unreferenced.
		return nil, fmt.Errorf("read OCI runtime entries dir for layer reference scan: %w", err)
	}
	for _, entry := range survivingEntries {
		if !entry.IsDir() {
			continue
		}
		metaPath := gc.cfg.OCIRuntimeEntryMetaPath(entry.Name())
		meta, readErr := oci.ReadEntryMeta(metaPath)
		if readErr != nil {
			continue // skip unreadable entries
		}
		referencedLayers[meta.KernelLayerDigest] = struct{}{}
		for _, digest := range meta.RootfsLayerDigests {
			referencedLayers[digest] = struct{}{}
		}
	}

	layersDir := gc.cfg.OCIRuntimeLayersDir()
	layers, layerReadErr := os.ReadDir(layersDir)
	if layerReadErr != nil && !os.IsNotExist(layerReadErr) {
		return nil, fmt.Errorf("read OCI runtime layers dir: %w", layerReadErr)
	}

	var collected []string
	for _, layer := range layers {
		if !layer.IsDir() {
			continue
		}
		hexDigest := layer.Name()
		if _, referenced := referencedLayers[hexDigest]; referenced {
			continue
		}

		// Defense-in-depth: skip recently-created layers.
		if info, statErr := layer.Info(); statErr == nil && info.ModTime().After(cutoff) {
			continue
		}

		layerDir := gc.cfg.OCIRuntimeLayerDir(hexDigest)
		if removeErr := os.RemoveAll(layerDir); removeErr != nil {
			continue // non-fatal
		}
		collected = append(collected, "layer:"+hexDigest)
	}

	return collected, nil
}

// filterManifestDigests returns a new slice with entries not in the remove set.
func filterManifestDigests(digests []string, removeSet map[string]bool) []string {
	result := make([]string, 0, len(digests))
	for _, d := range digests {
		if !removeSet[d] {
			result = append(result, d)
		}
	}
	return result
}

// tryDeleteZeroRefBlob attempts to delete a blob file that has zero manifest
// references. If the file is within the grace period, the blob entry is kept
// in the index so Phase 6 can track and eventually clean it. Returns true if
// the entry should be removed from the index (file deleted or already gone).
func tryDeleteZeroRefBlob(blobDir, digest string, cutoff time.Time) bool {
	blobPath := filepath.Join(blobDir, digest)
	info, statErr := os.Stat(blobPath)
	if statErr != nil {
		return true // file already gone — remove dangling entry
	}
	if info.ModTime().Before(cutoff) {
		_ = os.Remove(blobPath) // best-effort
		return true
	}
	return false // within grace — keep zero-ref entry for Phase 6
}

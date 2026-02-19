package oci

import (
	"fmt"
	"log"
	"os"

	"github.com/CMGS/cocoon/config"
)

// assemblyInfo holds the results of assembling an OCI layout for blob ref tracking.
// Used by both Build (linux) and Pull (all platforms).
type assemblyInfo struct {
	manifestDigest string
	blobDigests    []string
	blobSizes      []int64
}

// cleanupManifestRefsIfUnreferencedTxnLocked removes blob refs for manifestDigest
// only when no current tag references it. This is used for best-effort rollback
// paths (e.g. build/pull failure after AddBlobRefs succeeded but before SaveTag).
// Must be called while holding the txn lock.
func (s *Store) cleanupManifestRefsIfUnreferencedTxnLocked(manifestDigest string) error {
	if manifestDigest == "" {
		return nil
	}
	manifestStillUsed := false
	if err := s.withLock(func(idx *TagIndex) error {
		for _, entry := range idx.Tags {
			if entry.ManifestDigest == manifestDigest {
				manifestStillUsed = true
				break
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if manifestStillUsed {
		return nil
	}
	if _, err := RemoveBlobRefs(s.cfg, manifestDigest); err != nil {
		return fmt.Errorf("remove blob refs for %s: %w", manifestDigest, err)
	}
	return nil
}

// finalizeLayoutDir atomically moves a work directory into its final location
// under the OCI layouts directory. The final path is keyed by tag + manifest digest.
func finalizeLayoutDir(store *Store, tag, manifestDigest, layoutWorkDir string) (string, error) {
	layoutDir := store.LayoutDir(tag + "@" + manifestDigest)
	if _, statErr := os.Stat(layoutDir); statErr == nil {
		// Layout already exists (same tag+manifest built previously).
		return layoutDir, nil
	} else if !os.IsNotExist(statErr) {
		return "", fmt.Errorf("stat finalized layout dir %s: %w", layoutDir, statErr)
	}

	if err := os.Rename(layoutWorkDir, layoutDir); err != nil {
		// Another process may have won the race creating the same final layout.
		if _, existsErr := os.Stat(layoutDir); existsErr != nil {
			return "", fmt.Errorf("finalize OCI layout dir: %w", err)
		}
	}
	return layoutDir, nil
}

// registerBlobRefsAndSaveTag atomically registers blob references and saves a
// tag under the OCI build transaction lock. Used by both Build and Pull.
func registerBlobRefsAndSaveTag(store *Store, cfg *config.CocoonConfig, tag, layoutWorkDir string, info *assemblyInfo) (string, error) {
	layoutDir := ""
	err := store.withTxnLock(func() error {
		finalizedLayoutDir, finalizeErr := finalizeLayoutDir(store, tag, info.manifestDigest, layoutWorkDir)
		if finalizeErr != nil {
			return finalizeErr
		}
		layoutDir = finalizedLayoutDir

		// Register blob refs before saving tag, while sharing the same txn lock
		// as SaveTag/RemoveTag to prevent cross-index races.
		if err := AddBlobRefs(cfg, info.manifestDigest, info.blobDigests, info.blobSizes); err != nil {
			return fmt.Errorf("register blob refs: %w", err)
		}
		if err := store.saveTagTxnLocked(tag, layoutDir, info.manifestDigest); err != nil {
			// Best-effort rollback: only clean refs when this manifest is no longer
			// referenced by any tag.
			if refErr := store.cleanupManifestRefsIfUnreferencedTxnLocked(info.manifestDigest); refErr != nil {
				log.Printf("warning: failed to clean blob refs after SaveTag failure (GC will reclaim): %v", refErr)
			}
			return fmt.Errorf("save tag: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return layoutDir, nil
}

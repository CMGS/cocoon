//go:build linux

package oci

import "fmt"

// cleanupManifestRefsIfUnreferenced removes blob refs for manifestDigest only
// when no current tag references it. This is used for best-effort rollback
// paths (e.g. build failure after AddBlobRefs succeeded but before SaveTag).
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

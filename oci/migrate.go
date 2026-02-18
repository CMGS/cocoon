package oci

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/CMGS/cocoon/config"
)

// MigrateOCICache moves layouts from cache/oci-builds/ to cache/oci/layouts/
// and populates the shared blob store from existing layout blobs.
// Idempotent: skips layouts that already exist in the new location.
func MigrateOCICache(cfg *config.CocoonConfig) error {
	oldDir := cfg.OCIBuildCacheDir()
	if _, err := os.Stat(oldDir); os.IsNotExist(err) {
		return nil // nothing to migrate
	}

	blobStore := NewBlobStore(cfg)
	store := NewStore(cfg)

	// Read current tag index to find layouts that need migration.
	tags, err := store.ListTags()
	if err != nil {
		return fmt.Errorf("list tags for migration: %w", err)
	}

	for _, entry := range tags {
		// Skip if layout is already in the new location.
		if isUnderDir(entry.LayoutPath, cfg.OCILayoutDir()) {
			continue
		}

		// Skip if old layout doesn't exist.
		if _, statErr := os.Stat(entry.LayoutPath); os.IsNotExist(statErr) {
			continue
		}

		newLayoutDir := store.LayoutDir(entry.Tag)

		// Migrate blobs from old layout to shared blob store.
		if err := migrateLayoutBlobs(entry.LayoutPath, blobStore, newLayoutDir); err != nil {
			return fmt.Errorf("migrate blobs for tag %s: %w", entry.Tag, err)
		}

		// Copy non-blob files (oci-layout, index.json).
		for _, name := range []string{"oci-layout", "index.json"} {
			src := filepath.Join(entry.LayoutPath, name)
			dst := filepath.Join(newLayoutDir, name)
			data, readErr := os.ReadFile(src) //nolint:gosec // G304: src is an internal layout path
			if readErr != nil {
				if os.IsNotExist(readErr) {
					continue // skip missing files
				}
				return fmt.Errorf("read %s from old layout: %w", name, readErr)
			}
			if writeErr := os.WriteFile(dst, data, 0o644); writeErr != nil { //nolint:gosec // G306: layout files are shared cache data
				return fmt.Errorf("copy %s: %w", name, writeErr)
			}
		}

		// Register blob refs BEFORE saving tag to prevent a GC race window
		// where blobs could be collected between SaveTag and AddBlobRefs.
		blobDigests, blobSizes, collectErr := collectBlobDigestsFromLayout(newLayoutDir)
		if collectErr != nil {
			return fmt.Errorf("collect blob digests for %s: %w", entry.Tag, collectErr)
		}
		if len(blobDigests) > 0 {
			if err := AddBlobRefs(cfg, entry.ManifestDigest, blobDigests, blobSizes); err != nil {
				return fmt.Errorf("add blob refs for %s: %w", entry.Tag, err)
			}
		}

		// Update tag index to point to new layout path.
		if err := store.SaveTag(entry.Tag, newLayoutDir, entry.ManifestDigest); err != nil {
			return fmt.Errorf("update tag index for %s: %w", entry.Tag, err)
		}
	}

	// Remove old directory after successful migration.
	if err := os.RemoveAll(oldDir); err != nil {
		// Non-fatal: log but continue.
		fmt.Fprintf(os.Stderr, "Warning: remove old OCI cache dir: %v\n", err)
	}

	return nil
}

// migrateLayoutBlobs moves blobs from an old layout to the shared blob store
// and creates hardlinks in the new layout directory.
func migrateLayoutBlobs(oldLayoutDir string, blobStore *BlobStore, newLayoutDir string) error {
	oldBlobsDir := filepath.Join(oldLayoutDir, "blobs", "sha256")
	entries, err := os.ReadDir(oldBlobsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read old blobs dir: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		digest := entry.Name()
		srcPath := filepath.Join(oldBlobsDir, digest)

		// Store in shared blob store.
		if _, err := blobStore.StoreBlob(srcPath, digest); err != nil {
			return fmt.Errorf("store blob %s: %w", digest, err)
		}

		// Create hardlink in new layout.
		if err := blobStore.LinkBlobToLayout(digest, newLayoutDir); err != nil {
			return fmt.Errorf("link blob %s to layout: %w", digest, err)
		}
	}

	return nil
}

// collectBlobDigestsFromLayout reads the manifest from a layout and returns
// all blob digests (config + layers) and their sizes.
func collectBlobDigestsFromLayout(layoutDir string) ([]string, []int64, error) {
	indexPath := filepath.Join(layoutDir, "index.json")
	indexData, err := os.ReadFile(indexPath) //nolint:gosec // G304: layoutDir is an internal path
	if err != nil {
		return nil, nil, fmt.Errorf("read index.json: %w", err)
	}

	var idx ociIndex
	if unmarshalErr := json.Unmarshal(indexData, &idx); unmarshalErr != nil {
		return nil, nil, fmt.Errorf("parse index.json: %w", unmarshalErr)
	}
	if len(idx.Manifests) == 0 {
		return nil, nil, fmt.Errorf("no manifests in index.json")
	}

	manifestDesc := idx.Manifests[0]
	manifestData, err := readBlob(layoutDir, manifestDesc.Digest)
	if err != nil {
		return nil, nil, fmt.Errorf("read manifest: %w", err)
	}

	var manifest ociManifest
	if unmarshalErr := json.Unmarshal(manifestData, &manifest); unmarshalErr != nil {
		return nil, nil, fmt.Errorf("parse manifest: %w", unmarshalErr)
	}

	var digests []string
	var sizes []int64

	digests = append(digests, stripSHA256Prefix(manifest.Config.Digest))
	sizes = append(sizes, manifest.Config.Size)

	for _, layer := range manifest.Layers {
		digests = append(digests, stripSHA256Prefix(layer.Digest))
		sizes = append(sizes, layer.Size)
	}

	digests = append(digests, stripSHA256Prefix(manifestDesc.Digest))
	sizes = append(sizes, manifestDesc.Size)

	return digests, sizes, nil
}

// stripSHA256Prefix removes "sha256:" prefix from a digest string.
func stripSHA256Prefix(digest string) string {
	if len(digest) > 7 && digest[:7] == "sha256:" {
		return digest[7:]
	}
	return digest
}

// isUnderDir reports whether path is under dir.
func isUnderDir(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != ".." && !filepath.IsAbs(rel) && len(rel) > 0 && rel[0] != '.'
}

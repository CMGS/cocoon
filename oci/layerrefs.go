package oci

import (
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/lock/flock"
	"github.com/CMGS/cocoon/utils"
)

// LayerRefsIndex maps blob digests to the manifests that reference them.
type LayerRefsIndex struct {
	Blobs map[string]BlobRefEntry `json:"blobs"`
}

// BlobRefEntry tracks which manifests reference a specific blob.
type BlobRefEntry struct {
	ManifestDigests []string  `json:"manifest_digests"`
	Size            int64     `json:"size"`
	CreatedAt       time.Time `json:"created_at"`
}

// AddBlobRefs records that manifestDigest references the given blob digests.
// Called after build or pull stores blobs.
func AddBlobRefs(cfg *config.CocoonConfig, manifestDigest string, blobDigests []string, sizes []int64) error {
	return withLayerRefsLock(cfg, func(idx *LayerRefsIndex) error {
		for i, digest := range blobDigests {
			entry, ok := idx.Blobs[digest]
			if !ok {
				var size int64
				if i < len(sizes) {
					size = sizes[i]
				}
				entry = BlobRefEntry{
					Size:      size,
					CreatedAt: time.Now().UTC(),
				}
			} else if i < len(sizes) && sizes[i] > 0 && entry.Size == 0 {
				// Update size if previously unknown.
				entry.Size = sizes[i]
			}
			// Add manifest if not already present.
			if !slices.Contains(entry.ManifestDigests, manifestDigest) {
				entry.ManifestDigests = append(entry.ManifestDigests, manifestDigest)
			}
			idx.Blobs[digest] = entry
		}
		return saveLayerRefs(cfg, idx)
	})
}

// RemoveBlobRefs removes manifestDigest from all blob entries.
// Returns blob digests that now have zero references (candidates for GC).
func RemoveBlobRefs(cfg *config.CocoonConfig, manifestDigest string) ([]string, error) {
	var zeroRef []string
	err := withLayerRefsLock(cfg, func(idx *LayerRefsIndex) error {
		// Note: deleting from a map during range iteration is safe in Go.
		// See https://go.dev/doc/effective_go#for
		for digest, entry := range idx.Blobs {
			entry.ManifestDigests = removeString(entry.ManifestDigests, manifestDigest)
			if len(entry.ManifestDigests) == 0 {
				zeroRef = append(zeroRef, digest)
				delete(idx.Blobs, digest)
			} else {
				idx.Blobs[digest] = entry
			}
		}
		return saveLayerRefs(cfg, idx)
	})
	return zeroRef, err
}

// GetUnreferencedBlobs returns blob digests with zero manifest references.
func GetUnreferencedBlobs(cfg *config.CocoonConfig) ([]string, error) {
	var result []string
	err := withLayerRefsLock(cfg, func(idx *LayerRefsIndex) error {
		for digest, entry := range idx.Blobs {
			if len(entry.ManifestDigests) == 0 {
				result = append(result, digest)
			}
		}
		return nil // read-only
	})
	return result, err
}

// GetAllTrackedBlobs returns all blob digests currently tracked in the layer refs index.
func GetAllTrackedBlobs(cfg *config.CocoonConfig) ([]string, error) {
	var result []string
	err := withLayerRefsLock(cfg, func(idx *LayerRefsIndex) error {
		for digest := range idx.Blobs {
			result = append(result, digest)
		}
		return nil // read-only
	})
	return result, err
}

func loadLayerRefs(cfg *config.CocoonConfig) (*LayerRefsIndex, error) {
	idx := &LayerRefsIndex{Blobs: make(map[string]BlobRefEntry)}
	path := cfg.OCILayerRefsFile()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return idx, nil
	}
	if err := utils.ReadJSON(path, idx); err != nil {
		return nil, fmt.Errorf("read OCI layer refs: %w", err)
	}
	if idx.Blobs == nil {
		idx.Blobs = make(map[string]BlobRefEntry)
	}
	return idx, nil
}

func saveLayerRefs(cfg *config.CocoonConfig, idx *LayerRefsIndex) error {
	return utils.AtomicWriteJSON(cfg.OCILayerRefsFile(), idx)
}

func withLayerRefsLock(cfg *config.CocoonConfig, fn func(*LayerRefsIndex) error) error {
	if err := os.MkdirAll(cfg.DBDir(), 0o755); err != nil {
		return fmt.Errorf("create db dir: %w", err)
	}
	fl := flock.New(cfg.OCILayerRefsLock())
	if err := fl.Lock(); err != nil {
		return fmt.Errorf("acquire OCI layer refs lock: %w", err)
	}
	defer fl.Unlock() //nolint:errcheck

	idx, err := loadLayerRefs(cfg)
	if err != nil {
		return err
	}
	return fn(idx)
}

func removeString(slice []string, s string) []string {
	result := make([]string, 0, len(slice))
	for _, v := range slice {
		if v != s {
			result = append(result, v)
		}
	}
	return result
}

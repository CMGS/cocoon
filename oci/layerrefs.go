package oci

import (
	"fmt"
	"slices"
	"time"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/lock/jsonstore"
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
	return layerRefsStore(cfg).Update(func(idx *LayerRefsIndex) error {
		for i, digest := range blobDigests {
			if err := validateHexDigest(digest); err != nil {
				return fmt.Errorf("invalid blob digest at index %d (%q): %w", i, digest, err)
			}
			if len(digest) != 64 {
				return fmt.Errorf("invalid blob digest at index %d (%q): expected 64 hex characters", i, digest)
			}
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
		return nil
	})
}

// RemoveBlobRefs removes manifestDigest from all blob entries.
// Returns blob digests that now have zero references (candidates for GC).
func RemoveBlobRefs(cfg *config.CocoonConfig, manifestDigest string) ([]string, error) {
	var zeroRef []string
	err := layerRefsStore(cfg).Update(func(idx *LayerRefsIndex) error {
		// Note: deleting from a map during range iteration is safe in Go.
		// See https://go.dev/doc/effective_go#for
		for digest, entry := range idx.Blobs {
			entry.ManifestDigests = slices.DeleteFunc(entry.ManifestDigests, func(s string) bool { return s == manifestDigest })
			if len(entry.ManifestDigests) == 0 {
				zeroRef = append(zeroRef, digest)
				delete(idx.Blobs, digest)
			} else {
				idx.Blobs[digest] = entry
			}
		}
		return nil
	})
	return zeroRef, err
}

// GetUnreferencedBlobs returns blob digests with zero manifest references.
func GetUnreferencedBlobs(cfg *config.CocoonConfig) ([]string, error) {
	var result []string
	err := layerRefsStore(cfg).Read(func(idx *LayerRefsIndex) error {
		for digest, entry := range idx.Blobs {
			if len(entry.ManifestDigests) == 0 {
				result = append(result, digest)
			}
		}
		return nil
	})
	return result, err
}

// GetAllTrackedBlobs returns all blob digests currently tracked in the layer refs index.
func GetAllTrackedBlobs(cfg *config.CocoonConfig) ([]string, error) {
	var result []string
	err := layerRefsStore(cfg).Read(func(idx *LayerRefsIndex) error {
		for digest := range idx.Blobs {
			result = append(result, digest)
		}
		return nil
	})
	return result, err
}

// GetAllManifestDigests returns all unique manifest digests referenced by any
// blob entry in the layer refs index.
func GetAllManifestDigests(cfg *config.CocoonConfig) ([]string, error) {
	var result []string
	err := layerRefsStore(cfg).Read(func(idx *LayerRefsIndex) error {
		seen := make(map[string]bool)
		for _, entry := range idx.Blobs {
			for _, md := range entry.ManifestDigests {
				if !seen[md] {
					result = append(result, md)
					seen[md] = true
				}
			}
		}
		return nil
	})
	return result, err
}

func layerRefsStore(cfg *config.CocoonConfig) *jsonstore.Store[LayerRefsIndex] {
	return jsonstore.New(
		cfg.OCILayerRefsLock(),
		cfg.OCILayerRefsFile(),
		func() *LayerRefsIndex {
			return &LayerRefsIndex{Blobs: make(map[string]BlobRefEntry)}
		},
	).WithEnsureDir(cfg.DBDir())
}

package oci

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// LayoutInfo holds parsed metadata from an OCI image layout.
type LayoutInfo struct {
	// ManifestDigest is the sha256 digest of the manifest blob.
	ManifestDigest string `json:"manifest_digest"`
	// Layers describes each layer in the manifest.
	Layers []LayerInfo `json:"layers"`
	// Config is the parsed VMImageConfig from the config blob.
	Config *VMImageConfig `json:"config"`
}

// LayerInfo describes a single layer in the OCI manifest.
type LayerInfo struct {
	MediaType string `json:"media_type"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

// InspectLayout reads and parses an OCI image layout directory, returning
// structured metadata about the manifest, layers, and VM config.
func InspectLayout(layoutPath string) (*LayoutInfo, error) {
	// 1. Read index.json to find the manifest digest.
	indexPath := filepath.Join(layoutPath, "index.json")
	indexData, err := os.ReadFile(indexPath) //nolint:gosec // G304: layoutPath is from local store
	if err != nil {
		return nil, fmt.Errorf("read index.json: %w", err)
	}

	var idx ociIndex
	if err = json.Unmarshal(indexData, &idx); err != nil {
		return nil, fmt.Errorf("parse index.json: %w", err)
	}
	if len(idx.Manifests) == 0 {
		return nil, fmt.Errorf("index.json contains no manifests")
	}

	manifestDesc := idx.Manifests[0]
	manifestDigest := manifestDesc.Digest // e.g., "sha256:abcdef..."

	// 2. Read the manifest blob.
	manifestData, err := readBlob(layoutPath, manifestDigest)
	if err != nil {
		return nil, fmt.Errorf("read manifest blob: %w", err)
	}

	var manifest ociManifest
	if err = json.Unmarshal(manifestData, &manifest); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}

	// 3. Read the config blob.
	configData, err := readBlob(layoutPath, manifest.Config.Digest)
	if err != nil {
		return nil, fmt.Errorf("read config blob: %w", err)
	}

	var vmConfig VMImageConfig
	if err = json.Unmarshal(configData, &vmConfig); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// 4. Build layer info list.
	layers := make([]LayerInfo, 0, len(manifest.Layers))
	for _, l := range manifest.Layers {
		layers = append(layers, LayerInfo(l))
	}

	return &LayoutInfo{
		ManifestDigest: manifestDigest,
		Layers:         layers,
		Config:         &vmConfig,
	}, nil
}

// LayoutSize returns the total size of all blobs (config + layers) in an OCI layout.
func LayoutSize(layoutPath string) (int64, error) {
	indexPath := filepath.Join(layoutPath, "index.json")
	indexData, err := os.ReadFile(indexPath) //nolint:gosec // G304: layoutPath is from local store
	if err != nil {
		return 0, fmt.Errorf("read index.json: %w", err)
	}

	var idx ociIndex
	if unmarshalErr := json.Unmarshal(indexData, &idx); unmarshalErr != nil {
		return 0, fmt.Errorf("parse index.json: %w", unmarshalErr)
	}
	if len(idx.Manifests) == 0 {
		return 0, fmt.Errorf("no manifests in index.json")
	}

	manifestData, err := readBlob(layoutPath, idx.Manifests[0].Digest)
	if err != nil {
		return 0, fmt.Errorf("read manifest: %w", err)
	}

	var manifest ociManifest
	if unmarshalErr := json.Unmarshal(manifestData, &manifest); unmarshalErr != nil {
		return 0, fmt.Errorf("parse manifest: %w", unmarshalErr)
	}

	var total int64
	total += idx.Manifests[0].Size // manifest blob itself
	total += manifest.Config.Size
	for _, layer := range manifest.Layers {
		total += layer.Size
	}
	return total, nil
}

// readBlob reads a blob from the OCI layout's blobs directory.
// digest should be in the form "sha256:hexstring".
func readBlob(layoutPath, digest string) ([]byte, error) {
	// Parse "sha256:abcdef..." into algorithm and hex.
	if len(digest) < 8 || digest[:7] != "sha256:" {
		return nil, fmt.Errorf("unsupported digest format: %s", digest)
	}
	hex := digest[7:]
	// Validate hex part contains only hexadecimal characters to prevent path traversal.
	for _, c := range hex {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return nil, fmt.Errorf("invalid character in digest hex: %s", digest)
		}
	}
	blobPath := filepath.Join(layoutPath, "blobs", "sha256", hex)
	data, err := os.ReadFile(blobPath) //nolint:gosec // G304: path derived from local OCI layout
	if err != nil {
		return nil, fmt.Errorf("read blob %s: %w", digest, err)
	}
	return data, nil
}

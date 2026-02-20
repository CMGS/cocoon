package oci

import (
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/CMGS/cocoon/utils"
)

// MaterializeRootfs extracts all non-kernel layers from an OCI layout into a
// single flat directory, applying OCI whiteout semantics (.wh.<name> and
// .wh..wh..opq) so the result is a complete, flattened rootfs tree suitable
// for qcow2 conversion.
//
// Layers are applied base-to-top (manifest order). Compressed layers
// (media type containing "+gzip") are transparently decompressed.
func MaterializeRootfs(ctx context.Context, layoutPath, targetDir string, info *LayoutInfo) error {
	if info == nil {
		return fmt.Errorf("materialize rootfs: nil layout info")
	}

	if err := os.MkdirAll(targetDir, 0o755); err != nil { //nolint:gosec // targetDir is a caller-controlled temp path
		return fmt.Errorf("materialize rootfs: create target dir: %w", err)
	}

	for i, layer := range info.Layers {
		if err := ctx.Err(); err != nil {
			return err
		}

		// Skip kernel layers — only rootfs layers are materialized.
		if layer.MediaType == MediaTypeKernelLayer {
			continue
		}

		blobPath, err := LayoutBlobPath(layoutPath, layer.Digest)
		if err != nil {
			return fmt.Errorf("materialize rootfs: resolve layer %d blob: %w", i, err)
		}

		if isCompressedLayer(layer.MediaType) {
			if err := extractCompressedLayerToDir(ctx, blobPath, targetDir); err != nil {
				return fmt.Errorf("materialize rootfs: extract compressed layer %d (%s): %w", i, layer.Digest, err)
			}
		} else {
			if err := utils.ExtractOCILayerTarToDir(ctx, blobPath, targetDir); err != nil {
				return fmt.Errorf("materialize rootfs: extract layer %d (%s): %w", i, layer.Digest, err)
			}
		}
	}

	return nil
}

// isCompressedLayer returns true if the media type indicates gzip compression.
func isCompressedLayer(mediaType string) bool {
	normalized := strings.ToLower(strings.TrimSpace(mediaType))
	return strings.Contains(normalized, "+gzip") || strings.HasSuffix(normalized, ".gzip")
}

// extractCompressedLayerToDir decompresses a gzip-compressed tar layer and
// extracts it into targetDir with OCI whiteout-apply semantics.
func extractCompressedLayerToDir(ctx context.Context, blobPath, targetDir string) error {
	f, err := os.Open(blobPath) //nolint:gosec // blobPath is from a validated local OCI layout
	if err != nil {
		return fmt.Errorf("open compressed layer %q: %w", blobPath, err)
	}
	defer f.Close() //nolint:errcheck

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("decompress layer %q: %w", blobPath, err)
	}
	defer gz.Close() //nolint:errcheck

	return utils.ExtractOCILayerTarFromReader(ctx, blobPath, targetDir, gz)
}

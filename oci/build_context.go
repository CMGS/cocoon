package oci

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/CMGS/cocoon/utils"
)

const buildContextFilename = ".cocoon-build-context.json"

// BuildSourceType identifies the origin of a build base image.
type BuildSourceType string

const (
	// BuildSourceLocalOCITag indicates the build base came from a local OCI tag
	// materialized into a temporary qcow2 image.
	BuildSourceLocalOCITag BuildSourceType = "local-oci-tag"
)

// BuildContext carries metadata from the CLI's build resolution phase into the
// OCI builder so it can preserve base layers when building on top of local OCI
// tags.
type BuildContext struct {
	SourceType BuildSourceType `json:"source_type"`

	BaseTag        string        `json:"base_tag,omitempty"`
	BaseLayoutPath string        `json:"base_layout_path,omitempty"`
	BaseConfig     VMImageConfig `json:"base_config"`

	KernelLayer  LayerInfo   `json:"kernel_layer"`
	RootfsLayers []LayerInfo `json:"rootfs_layers,omitempty"`

	// BaseRootfsDir points to the materialized rootfs directory used to create
	// the temporary qcow2. It is used as the "before" snapshot for delta layer
	// generation.
	BaseRootfsDir string `json:"base_rootfs_dir,omitempty"`
}

// buildContextPathForImage returns the sidecar build-context path associated
// with a materialized image path.
func buildContextPathForImage(imagePath string) string {
	return filepath.Join(filepath.Dir(imagePath), buildContextFilename)
}

// WriteBuildContextForImage atomically writes a build-context sidecar file for
// the given materialized image path.
func WriteBuildContextForImage(imagePath string, ctx *BuildContext) error {
	if ctx == nil {
		return fmt.Errorf("build context is nil")
	}
	ctxPath := buildContextPathForImage(imagePath)
	if err := os.MkdirAll(filepath.Dir(ctxPath), 0o755); err != nil { //nolint:gosec // temporary workspace path
		return fmt.Errorf("create build context dir: %w", err)
	}
	if err := utils.AtomicWriteJSON(ctxPath, ctx); err != nil {
		return fmt.Errorf("write build context %s: %w", ctxPath, err)
	}
	return nil
}

// ReadBuildContextForImage reads the optional build-context sidecar associated
// with imagePath. It returns (nil, nil) when no context file exists.
func ReadBuildContextForImage(imagePath string) (*BuildContext, error) {
	ctxPath := buildContextPathForImage(imagePath)
	var ctx BuildContext
	if err := utils.ReadJSON(ctxPath, &ctx); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read build context %s: %w", ctxPath, err)
	}
	return &ctx, nil
}

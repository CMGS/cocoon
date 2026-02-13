//go:build darwin

package pipeline

import (
	"context"
	"fmt"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/image"
)

func identifyOCIPlatform(_ context.Context, ref string) (*image.ImageIdentity, error) {
	return nil, fmt.Errorf("OCI registry pull requires buildah and skopeo (Linux only); use a direct URL or local file path instead (ref: %s)", ref)
}

func pullAndMountOCIPlatform(_ context.Context, _ *config.CocoonConfig, _ *image.ImageIdentity) error {
	return fmt.Errorf("OCI registry pull requires buildah and skopeo (Linux only)")
}

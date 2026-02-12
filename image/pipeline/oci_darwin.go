//go:build darwin

package pipeline

import (
	"context"
	"fmt"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/image"
)

func pullOCIPlatform(_ context.Context, _ *config.CocoonConfig, ref string) (*image.ImageIdentity, error) {
	return nil, fmt.Errorf("OCI registry pull requires buildah and skopeo (Linux only); use a direct URL or local file path instead (ref: %s)", ref)
}

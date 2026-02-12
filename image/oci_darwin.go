//go:build darwin

package image

import (
	"context"
	"fmt"

	"github.com/CMGS/cocoon/config"
)

func pullOCIPlatform(_ context.Context, _ *config.CocoonConfig, ref string) (*ImageIdentity, error) {
	return nil, fmt.Errorf("OCI registry pull requires buildah and skopeo (Linux only); use a direct URL or local file path instead (ref: %s)", ref)
}

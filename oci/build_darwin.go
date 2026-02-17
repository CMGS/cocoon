//go:build darwin

package oci

import (
	"context"
	"fmt"

	"github.com/CMGS/cocoon/config"
)

// Build is not supported on Darwin. Cloud image extraction requires
// libguestfs which is Linux-only.
func Build(_ context.Context, _ *config.CocoonConfig, _, _ string, _ *Cocoonfile) (*BuildResult, error) {
	return nil, fmt.Errorf("cocoon image build requires libguestfs (Linux only)")
}

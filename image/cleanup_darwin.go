//go:build darwin

package image

import "github.com/CMGS/cocoon/config"

func cleanupBuildahContainer(_ string, _ *config.CocoonConfig) {
	// No-op on darwin: buildah is not available.
}

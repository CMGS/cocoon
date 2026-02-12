//go:build darwin

package pipeline

import "github.com/CMGS/cocoon/config"

func cleanupBuildahContainer(_ string, _ *config.CocoonConfig) {
	// No-op on darwin: buildah is not available.
}

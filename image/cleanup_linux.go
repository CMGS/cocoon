//go:build linux

package image

import (
	"log"
	"os/exec"

	"github.com/CMGS/cocoon/config"
)

func cleanupBuildahContainer(containerID string, cfg *config.CocoonConfig) {
	if containerID == "" {
		return
	}
	root := cfg.BuildahRoot
	if out, err := exec.Command("buildah", "--root", root, "umount", containerID).CombinedOutput(); err != nil { //nolint:gosec // containerID is from buildah output, root is from config
		log.Printf("warning: buildah umount %s: %s: %v", containerID, string(out), err)
	}
	if out, err := exec.Command("buildah", "--root", root, "rm", containerID).CombinedOutput(); err != nil { //nolint:gosec // containerID is from buildah output, root is from config
		log.Printf("warning: buildah rm %s: %s: %v", containerID, string(out), err)
	}
}

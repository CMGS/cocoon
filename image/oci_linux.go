//go:build linux

package image

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/types"
)

// ociManifest represents a single OCI image manifest (application/vnd.oci.image.manifest.v1+json
// or application/vnd.docker.distribution.manifest.v2+json).
type ociManifest struct {
	MediaType string `json:"mediaType"`
	Config    struct {
		Digest string `json:"digest"`
	} `json:"config"`
	Layers []struct {
		Digest string `json:"digest"`
	} `json:"layers"`
}

// ociIndex represents an OCI image index / Docker manifest list.
type ociIndex struct {
	MediaType string `json:"mediaType"`
	SchemaVersion int    `json:"schemaVersion"`
}

// pullOCIPlatform pulls an OCI image on Linux using skopeo and buildah.
func pullOCIPlatform(ctx context.Context, cfg *config.CocoonConfig, ref string) (*ImageIdentity, error) {
	// 1. Check required tools are available.
	if _, err := exec.LookPath("skopeo"); err != nil {
		return nil, types.NewPermanentError(fmt.Errorf("skopeo not found in PATH: %w", err))
	}
	if _, err := exec.LookPath("buildah"); err != nil {
		return nil, types.NewPermanentError(fmt.Errorf("buildah not found in PATH: %w", err))
	}

	arch := GOARCHToOCI(runtime.GOARCH)
	root := cfg.BuildahRoot

	// 2. Inspect the raw manifest to determine if it's a manifest list or single manifest.
	rawManifest, err := runCmd(ctx, "skopeo", "inspect", "--raw", "docker://"+ref)
	if err != nil {
		return nil, classifySkopeoError(fmt.Errorf("skopeo inspect %s: %w", ref, err))
	}

	// 3. Detect manifest list vs single manifest.
	var index ociIndex
	if jsonErr := json.Unmarshal(rawManifest, &index); jsonErr != nil {
		return nil, types.NewPermanentError(fmt.Errorf("parse manifest for %s: %w", ref, jsonErr))
	}

	isManifestList := strings.Contains(index.MediaType, "image.index") ||
		strings.Contains(index.MediaType, "manifest.list")

	if isManifestList {
		// 4. Re-inspect with architecture override to get the platform-specific manifest.
		rawManifest, err = runCmd(ctx, "skopeo", "inspect", "--raw", "--override-arch", arch, "docker://"+ref)
		if err != nil {
			return nil, classifySkopeoError(fmt.Errorf("skopeo inspect (arch=%s) %s: %w", arch, ref, err))
		}
	}

	// 5. Parse the single manifest.
	var manifest ociManifest
	if err := json.Unmarshal(rawManifest, &manifest); err != nil {
		return nil, types.NewPermanentError(fmt.Errorf("parse single manifest for %s: %w", ref, err))
	}

	configDigest := manifest.Config.Digest
	layerDigests := make([]string, len(manifest.Layers))
	for i, l := range manifest.Layers {
		layerDigests[i] = l.Digest
	}

	// 6. Compute content-addressed identity.
	fullDigest, checksum := ComputeOCIChecksum(configDigest, layerDigests, arch)

	// 7. Pull image with buildah.
	if _, err := runCmd(ctx, "buildah", "--root", root, "pull", ref); err != nil {
		return nil, classifyBuildahError(fmt.Errorf("buildah pull %s: %w", ref, err))
	}

	// 8. Create working container.
	containerOut, err := runCmd(ctx, "buildah", "--root", root, "from", ref)
	if err != nil {
		return nil, classifyBuildahError(fmt.Errorf("buildah from %s: %w", ref, err))
	}
	containerID := strings.TrimSpace(string(containerOut))

	// 9. Mount container to get rootfs path.
	mountOut, err := runCmd(ctx, "buildah", "--root", root, "mount", containerID)
	if err != nil {
		return nil, classifyBuildahError(fmt.Errorf("buildah mount %s: %w", containerID, err))
	}
	mountPath := strings.TrimSpace(string(mountOut))

	// 10. Return identity.
	return &ImageIdentity{
		Checksum:    checksum,
		Arch:        arch,
		FullDigest:  fullDigest,
		SourceRef:   ref,
		ImageType:   ImageTypeOCI,
		TempPath:    mountPath,
		ContainerID: containerID,
	}, nil
}

// runCmd executes a command and returns its combined stdout output.
func runCmd(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // args are controlled internal values
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("%s: %s", strings.Join(append([]string{name}, args...), " "), string(exitErr.Stderr))
		}
		return nil, err
	}
	return out, nil
}

// classifySkopeoError classifies skopeo errors as transient or permanent.
func classifySkopeoError(err error) error {
	msg := err.Error()
	// Auth failures and not-found are permanent.
	if strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "authentication required") ||
		strings.Contains(msg, "denied") ||
		strings.Contains(msg, "not found") ||
		strings.Contains(msg, "manifest unknown") ||
		strings.Contains(msg, "NAME_UNKNOWN") {
		return types.NewPermanentError(err)
	}
	// Network/timeout errors are transient.
	return types.NewTransientError(err)
}

// classifyBuildahError classifies buildah errors as transient or permanent.
func classifyBuildahError(err error) error {
	msg := err.Error()
	if strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "temporary failure") ||
		strings.Contains(msg, "i/o timeout") {
		return types.NewTransientError(err)
	}
	return types.NewPermanentError(err)
}

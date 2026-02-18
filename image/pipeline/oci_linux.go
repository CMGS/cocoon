//go:build linux

package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/image"
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
	MediaType     string `json:"mediaType"`
	SchemaVersion int    `json:"schemaVersion"`
}

// identifyOCIPlatform inspects an OCI image manifest using skopeo to compute
// the content-addressed identity without pulling the image layers. This is
// cheap and idempotent, suitable for running outside the conversion lock.
func identifyOCIPlatform(ctx context.Context, ref string) (*image.ImageIdentity, error) {
	// 1. Check that skopeo is available.
	if _, err := exec.LookPath("skopeo"); err != nil {
		return nil, types.NewPermanentError(fmt.Errorf("skopeo not found in PATH: %w", err))
	}

	arch := goarchToOCI(runtime.GOARCH)

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
	if err = json.Unmarshal(rawManifest, &manifest); err != nil {
		return nil, types.NewPermanentError(fmt.Errorf("parse single manifest for %s: %w", ref, err))
	}

	configDigest := manifest.Config.Digest
	layerDigests := make([]string, len(manifest.Layers))
	for i, l := range manifest.Layers {
		layerDigests[i] = l.Digest
	}

	// 6. Compute content-addressed identity.
	fullDigest, checksum := computeOCIChecksum(configDigest, layerDigests, arch)

	// 7. Compute manifest digest to pin the pull to this exact manifest,
	// preventing TOCTOU races where a tag is updated between identify and pull.
	manifestHash := sha256.Sum256(rawManifest)
	manifestDigest := hex.EncodeToString(manifestHash[:])

	// 8. Return identity without TempPath or ContainerID (layers not pulled yet).
	return &image.ImageIdentity{
		Checksum:        checksum,
		Arch:            arch,
		FullDigest:      fullDigest,
		ManifestDigest:  manifestDigest,
		SourceRef:       ref,
		ImageType:       image.ImageTypeOCI,
	}, nil
}

// pullAndMountOCIPlatform pulls an OCI image and mounts it using buildah.
// This must be called inside the conversion lock since it performs the expensive
// pull + container creation + mount operations.
// On success it populates identity.TempPath and identity.ContainerID.
func pullAndMountOCIPlatform(ctx context.Context, cfg *config.CocoonConfig, identity *image.ImageIdentity) error {
	// 1. Check that buildah is available.
	if _, err := exec.LookPath("buildah"); err != nil {
		return types.NewPermanentError(fmt.Errorf("buildah not found in PATH: %w", err))
	}

	ref := identity.SourceRef
	root := cfg.BuildahRoot

	// 2. Pull image with buildah, pinned to manifest digest when available
	// to prevent TOCTOU races (tag updated between identify and pull).
	pullRef := ref
	if identity.ManifestDigest != "" {
		pullRef = ref + "@sha256:" + identity.ManifestDigest
	}
	if _, err := runCmd(ctx, "buildah", "--root", root, "pull", pullRef); err != nil {
		if identity.ManifestDigest != "" {
			// Fall back to tag-based pull if digest-pinned pull fails
			// (e.g., registry doesn't support digest references).
			if _, fallbackErr := runCmd(ctx, "buildah", "--root", root, "pull", ref); fallbackErr != nil {
				return classifyBuildahError(fmt.Errorf("buildah pull %s: %w", ref, fallbackErr))
			}
		} else {
			return classifyBuildahError(fmt.Errorf("buildah pull %s: %w", ref, err))
		}
	}

	// 3. Create working container (use original ref for container name).
	containerOut, err := runCmd(ctx, "buildah", "--root", root, "from", ref)
	if err != nil {
		return classifyBuildahError(fmt.Errorf("buildah from %s: %w", ref, err))
	}
	containerID := strings.TrimSpace(string(containerOut))
	identity.ContainerID = containerID // Assign immediately so callers can clean up on later failures

	// 4. Mount container to get rootfs path.
	mountOut, err := runCmd(ctx, "buildah", "--root", root, "mount", containerID)
	if err != nil {
		return classifyBuildahError(fmt.Errorf("buildah mount %s: %w", containerID, err))
	}
	mountPath := strings.TrimSpace(string(mountOut))

	// 5. Populate the identity with transient paths.
	identity.TempPath = mountPath
	return nil
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
	msg := strings.ToLower(err.Error())
	// Auth failures are permanent (not retryable).
	if strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "authentication required") ||
		strings.Contains(msg, "403") ||
		strings.Contains(msg, "401") {
		return types.NewPermanentError(err)
	}
	// Network/timeout errors are transient (retryable).
	if strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "temporary failure") ||
		strings.Contains(msg, "i/o timeout") {
		return types.NewTransientError(err)
	}
	return types.NewPermanentError(err)
}

// computeOCIChecksum computes a content-addressed checksum for an OCI image
// from its config digest, layer digests, and target architecture.
func computeOCIChecksum(configDigest string, layerDigests []string, arch string) (fullDigest string, checksum string) {
	var sb strings.Builder
	sb.WriteString(configDigest)
	sb.WriteString("\n")
	sb.WriteString(strings.Join(layerDigests, "\n"))
	sb.WriteString("\n")
	sb.WriteString("linux/" + arch)

	hash := sha256.Sum256([]byte(sb.String()))
	fullDigest = hex.EncodeToString(hash[:])
	checksum = fullDigest[:checksumHexLen]
	return fullDigest, checksum
}

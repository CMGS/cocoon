package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/CMGS/cocoon/image"
	"github.com/CMGS/cocoon/oci"
	"github.com/CMGS/cocoon/types"
)

// identifyOCIRemote fetches the manifest from a remote registry using
// go-containerregistry and computes the content-addressed identity without
// pulling image layers. This is cheap and idempotent, suitable for running
// outside the conversion lock.
func identifyOCIRemote(ctx context.Context, ref string) (*image.ImageIdentity, error) {
	ref = oci.EnsureLatestTag(ref)

	tag, err := name.NewTag(ref)
	if err != nil {
		return nil, types.NewPermanentError(fmt.Errorf("invalid registry reference %q: %w", ref, err))
	}

	keychain := authn.NewMultiKeychain(oci.CocoonKeychain(), authn.DefaultKeychain)
	arch := defaultArch()
	platform := v1.Platform{
		Architecture: arch,
		OS:           "linux",
	}

	desc, err := remote.Get(tag,
		remote.WithAuthFromKeychain(keychain),
		remote.WithContext(ctx),
		remote.WithPlatform(platform),
	)
	if err != nil {
		return nil, oci.ClassifyRegistryError("identify", err)
	}

	rawManifest := desc.Manifest

	// Parse single manifest for config digest and layer digests.
	var manifest struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(rawManifest, &manifest); err != nil {
		return nil, types.NewPermanentError(fmt.Errorf("parse manifest for %s: %w", ref, err))
	}

	configDigest := manifest.Config.Digest
	layerDigests := make([]string, len(manifest.Layers))
	for i, l := range manifest.Layers {
		layerDigests[i] = l.Digest
	}

	fullDigest, checksum := computeOCIChecksum(configDigest, layerDigests, arch)

	manifestHash := sha256.Sum256(rawManifest)
	manifestDigest := hex.EncodeToString(manifestHash[:])

	return &image.ImageIdentity{
		Checksum:       checksum,
		Arch:           arch,
		FullDigest:     fullDigest,
		ManifestDigest: manifestDigest,
		SourceRef:      ref,
		ImageType:      image.ImageTypeOCI,
	}, nil
}

// pullAndMaterializeOCI downloads a standard OCI image using go-containerregistry,
// stores it as a local OCI layout, and materializes the rootfs layers into a
// flat directory. Returns the materialized rootfs directory path.
// The caller is responsible for cleaning up tempRootfsDir.
func pullAndMaterializeOCI(ctx context.Context, ref string, identity *image.ImageIdentity) (string, error) {
	ref = oci.EnsureLatestTag(ref)

	tag, err := name.NewTag(ref)
	if err != nil {
		return "", types.NewPermanentError(fmt.Errorf("invalid registry reference %q: %w", ref, err))
	}

	keychain := authn.NewMultiKeychain(oci.CocoonKeychain(), authn.DefaultKeychain)
	platform := v1.Platform{
		Architecture: identity.Arch,
		OS:           "linux",
	}

	img, err := remote.Image(tag,
		remote.WithAuthFromKeychain(keychain),
		remote.WithContext(ctx),
		remote.WithPlatform(platform),
	)
	if err != nil {
		return "", oci.ClassifyRegistryError("pull", err)
	}

	// If the identity has a manifest digest, verify the pulled image matches.
	if identity.ManifestDigest != "" {
		pulledDigest, digestErr := img.Digest()
		if digestErr != nil {
			return "", oci.ClassifyRegistryError("pull", digestErr)
		}
		if pulledDigest.Hex != identity.ManifestDigest {
			return "", types.NewTransientError(fmt.Errorf(
				"OCI ref %s changed between identify and pull (expected manifest %s, got %s); retry",
				ref, identity.ManifestDigest, pulledDigest.Hex,
			))
		}
	}

	// Write to temp OCI layout for MaterializeRootfs.
	layoutDir, layoutInfo, err := writeImageToTempLayout(img)
	if err != nil {
		return "", fmt.Errorf("write temp OCI layout for %s: %w", ref, err)
	}
	// Layout is only needed for materialization; clean up after.
	defer func() {
		_ = os.RemoveAll(layoutDir)
	}()

	// Materialize rootfs (flatten all layers with whiteout handling).
	rootfsDir, err := os.MkdirTemp("", "cocoon-rootfs-*")
	if err != nil {
		return "", fmt.Errorf("create rootfs temp dir: %w", err)
	}

	if err := oci.MaterializeRootfs(ctx, layoutDir, rootfsDir, layoutInfo); err != nil {
		_ = os.RemoveAll(rootfsDir)
		return "", fmt.Errorf("materialize rootfs for %s: %w", ref, err)
	}

	// Set the TempPath so Convert() can find the rootfs.
	identity.TempPath = rootfsDir

	return rootfsDir, nil
}

// writeImageToTempLayout writes a go-containerregistry v1.Image to a temporary
// OCI layout directory and returns the path + parsed LayoutInfo.
func writeImageToTempLayout(img v1.Image) (string, *oci.LayoutInfo, error) {
	rawManifest, err := img.RawManifest()
	if err != nil {
		return "", nil, fmt.Errorf("get manifest: %w", err)
	}
	manifestDigest, err := img.Digest()
	if err != nil {
		return "", nil, fmt.Errorf("get digest: %w", err)
	}

	var manifest struct {
		Config struct {
			Digest    string `json:"digest"`
			MediaType string `json:"mediaType"`
			Size      int64  `json:"size"`
		} `json:"config"`
		Layers []struct {
			Digest    string `json:"digest"`
			MediaType string `json:"mediaType"`
			Size      int64  `json:"size"`
		} `json:"layers"`
	}
	if unmarshalErr := json.Unmarshal(rawManifest, &manifest); unmarshalErr != nil {
		return "", nil, fmt.Errorf("parse manifest: %w", unmarshalErr)
	}

	layoutDir, err := os.MkdirTemp("", "cocoon-layout-*")
	if err != nil {
		return "", nil, fmt.Errorf("create layout temp dir: %w", err)
	}
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(layoutDir)
		}
	}()

	blobsDir := filepath.Join(layoutDir, "blobs", "sha256")
	if mkdirErr := os.MkdirAll(blobsDir, 0o755); mkdirErr != nil { //nolint:gosec // G301: internal OCI layout directory needs standard permissions
		return "", nil, fmt.Errorf("create blobs dir: %w", mkdirErr)
	}

	// Write config blob.
	configData, err := img.RawConfigFile()
	if err != nil {
		return "", nil, fmt.Errorf("get config: %w", err)
	}
	configHex := strings.TrimPrefix(manifest.Config.Digest, "sha256:")
	if writeErr := os.WriteFile(filepath.Join(blobsDir, configHex), configData, 0o644); writeErr != nil { //nolint:gosec // G306: OCI blob files need standard read permissions
		return "", nil, fmt.Errorf("write config blob: %w", writeErr)
	}

	// Write layer blobs.
	layers, err := img.Layers()
	if err != nil {
		return "", nil, fmt.Errorf("get layers: %w", err)
	}
	for i, layer := range layers {
		layerDigest, digestErr := layer.Digest()
		if digestErr != nil {
			return "", nil, fmt.Errorf("get layer %d digest: %w", i, digestErr)
		}
		layerHex := layerDigest.Hex

		rc, compErr := layer.Compressed()
		if compErr != nil {
			return "", nil, fmt.Errorf("get layer %d compressed: %w", i, compErr)
		}
		blobPath := filepath.Join(blobsDir, layerHex)
		out, createErr := os.Create(blobPath) //nolint:gosec // G304: blob path is derived from content-addressed digest
		if createErr != nil {
			_ = rc.Close()
			return "", nil, fmt.Errorf("create layer %d blob: %w", i, createErr)
		}
		if _, copyErr := io.Copy(out, rc); copyErr != nil {
			_ = out.Close()
			_ = rc.Close()
			return "", nil, fmt.Errorf("write layer %d blob: %w", i, copyErr)
		}
		_ = rc.Close()
		if syncErr := out.Sync(); syncErr != nil {
			_ = out.Close()
			return "", nil, fmt.Errorf("sync layer %d blob: %w", i, syncErr)
		}
		if closeErr := out.Close(); closeErr != nil {
			return "", nil, fmt.Errorf("close layer %d blob: %w", i, closeErr)
		}
	}

	// Write manifest blob.
	if writeErr := os.WriteFile(filepath.Join(blobsDir, manifestDigest.Hex), rawManifest, 0o644); writeErr != nil { //nolint:gosec // G306: OCI blob files need standard read permissions
		return "", nil, fmt.Errorf("write manifest blob: %w", writeErr)
	}

	// Write index.json.
	indexJSON := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:%s","size":%d}]}`,
		manifestDigest.Hex, len(rawManifest))
	if writeErr := os.WriteFile(filepath.Join(layoutDir, "index.json"), []byte(indexJSON), 0o644); writeErr != nil { //nolint:gosec // G306: OCI layout metadata file
		return "", nil, fmt.Errorf("write index.json: %w", writeErr)
	}

	// Write oci-layout.
	if writeErr := os.WriteFile(filepath.Join(layoutDir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644); writeErr != nil { //nolint:gosec // G306: OCI layout metadata file
		return "", nil, fmt.Errorf("write oci-layout: %w", writeErr)
	}

	// Build LayoutInfo for MaterializeRootfs.
	layerInfos := make([]oci.LayerInfo, len(manifest.Layers))
	for i, l := range manifest.Layers {
		layerInfos[i] = oci.LayerInfo{
			MediaType: l.MediaType,
			Digest:    l.Digest,
			Size:      l.Size,
		}
	}

	var vmConfig oci.VMImageConfig
	if err := json.Unmarshal(configData, &vmConfig); err != nil {
		// Not a Cocoon VM config — that's fine for standard OCI images.
		vmConfig = oci.VMImageConfig{Arch: defaultArch()}
	}

	layoutInfo := &oci.LayoutInfo{
		ManifestDigest: "sha256:" + manifestDigest.Hex,
		Layers:         layerInfos,
		Config:         &vmConfig,
	}

	success = true
	return layoutDir, layoutInfo, nil
}

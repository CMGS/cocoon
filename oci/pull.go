package oci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/types"
	"github.com/CMGS/cocoon/utils"
)

// maxPullLayers is the maximum number of layers allowed in a pulled Cocoon VM
// manifest: 1 kernel + 1 rootfs + up to 32 customization layers.
const maxPullLayers = 34

// Pull downloads a Cocoon OCI VM image from a container registry into the
// local OCI store. It validates that the remote manifest uses Cocoon VM media
// types, stores blobs in the shared blob store, creates an OCI layout with
// hardlinks, and registers the tag. Pull is idempotent: re-pulling the same
// manifest digest is a no-op.
func Pull(ctx context.Context, cfg *config.CocoonConfig, ref string) (*PullResult, error) {
	ref = EnsureLatestTag(ref)

	tag, err := name.NewTag(ref)
	if err != nil {
		return nil, types.NewPermanentError(fmt.Errorf("invalid registry reference %q: %w", ref, err))
	}

	keychain := authn.NewMultiKeychain(CocoonKeychain(), authn.DefaultKeychain)
	platform := hostPlatform()

	fmt.Fprintf(os.Stderr, "Pulling OCI VM image: %s\n", ref)

	img, err := remote.Image(tag,
		remote.WithAuthFromKeychain(keychain),
		remote.WithContext(ctx),
		remote.WithPlatform(platform),
	)
	if err != nil {
		return nil, classifyPullError(err)
	}

	// Fetch and validate the manifest.
	rawManifest, err := img.RawManifest()
	if err != nil {
		return nil, classifyPullError(err)
	}
	if validateErr := validateCocoonVMManifest(rawManifest); validateErr != nil {
		return nil, types.NewPermanentError(fmt.Errorf("validate manifest for %q: %w", ref, validateErr))
	}

	manifestDigest, err := img.Digest()
	if err != nil {
		return nil, classifyPullError(err)
	}

	// Idempotency check: if we already have this tag with the same manifest, skip.
	store := NewStore(cfg)
	if alreadyPulled, layoutPath := checkIdempotent(store, ref, manifestDigest.Hex); alreadyPulled {
		fmt.Fprintf(os.Stderr, "Already up to date: %s (digest: sha256:%s)\n", ref, manifestDigest.Hex)
		return &PullResult{
			Ref:            ref,
			ManifestDigest: "sha256:" + manifestDigest.Hex,
			LayoutPath:     layoutPath,
		}, nil
	}

	blobStore := NewBlobStore(cfg)

	// Parse manifest to identify blobs.
	var manifest ociManifest
	if unmarshalErr := json.Unmarshal(rawManifest, &manifest); unmarshalErr != nil {
		return nil, types.NewPermanentError(fmt.Errorf("parse manifest: %w", unmarshalErr))
	}

	// Track all blob digests and sizes for ref tracking.
	var blobDigests []string
	var blobSizes []int64

	// Store config blob.
	configData, err := img.RawConfigFile()
	if err != nil {
		return nil, classifyPullError(err)
	}
	configDigestHex := stripSHA256Prefix(manifest.Config.Digest)
	if _, storeErr := blobStore.StoreBlobFromBytes(configData, configDigestHex); storeErr != nil {
		return nil, fmt.Errorf("store config blob: %w", storeErr)
	}
	blobDigests = append(blobDigests, configDigestHex)
	blobSizes = append(blobSizes, manifest.Config.Size)
	fmt.Fprintf(os.Stderr, "  Config: sha256:%s\n", configDigestHex[:12])

	// Store layer blobs.
	layers, err := img.Layers()
	if err != nil {
		return nil, classifyPullError(err)
	}
	for i, layer := range layers {
		layerDigest, layerDigestErr := layer.Digest()
		if layerDigestErr != nil {
			return nil, classifyPullError(layerDigestErr)
		}
		layerSize, layerSizeErr := layer.Size()
		if layerSizeErr != nil {
			return nil, classifyPullError(layerSizeErr)
		}
		layerHex := layerDigest.Hex

		fmt.Fprintf(os.Stderr, "  Layer %d/%d: sha256:%s (%s)\n",
			i+1, len(layers), layerHex[:12], utils.HumanBytes(layerSize))

		if !blobStore.BlobExists(layerHex) {
			if pullErr := pullLayerToStore(blobStore, layer, layerHex); pullErr != nil {
				return nil, fmt.Errorf("store layer %d (sha256:%s): %w", i+1, layerHex[:12], pullErr)
			}
		}
		blobDigests = append(blobDigests, layerHex)
		blobSizes = append(blobSizes, layerSize)
	}

	// Store manifest blob.
	if _, storeErr := blobStore.StoreBlobFromBytes(rawManifest, manifestDigest.Hex); storeErr != nil {
		return nil, fmt.Errorf("store manifest blob: %w", storeErr)
	}
	blobDigests = append(blobDigests, manifestDigest.Hex)
	blobSizes = append(blobSizes, int64(len(rawManifest)))

	// Create OCI layout work directory.
	layoutWorkDir, layoutErr := createPullLayoutWorkDir(cfg)
	if layoutErr != nil {
		return nil, layoutErr
	}
	cleanupWorkDir := true
	defer func() {
		if cleanupWorkDir {
			_ = os.RemoveAll(layoutWorkDir)
		}
	}()

	// Write oci-layout file.
	ociLayoutContent := []byte(`{"imageLayoutVersion":"1.0.0"}`)
	if writeErr := os.WriteFile(filepath.Join(layoutWorkDir, "oci-layout"), ociLayoutContent, 0o644); writeErr != nil { //nolint:gosec // G306: local cache file
		return nil, fmt.Errorf("write oci-layout: %w", writeErr)
	}

	// Write index.json.
	indexJSON := ociIndex{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.index.v1+json",
		Manifests: []ociDescriptor{
			{
				MediaType: "application/vnd.oci.image.manifest.v1+json",
				Digest:    "sha256:" + manifestDigest.Hex,
				Size:      int64(len(rawManifest)),
			},
		},
	}
	indexData, marshalErr := json.MarshalIndent(indexJSON, "", "  ")
	if marshalErr != nil {
		return nil, fmt.Errorf("marshal index.json: %w", marshalErr)
	}
	if writeErr := os.WriteFile(filepath.Join(layoutWorkDir, "index.json"), indexData, 0o644); writeErr != nil { //nolint:gosec // G306: local cache file
		return nil, fmt.Errorf("write index.json: %w", writeErr)
	}

	// Hardlink all blobs from shared store into layout.
	for _, digest := range blobDigests {
		if linkErr := blobStore.LinkBlobToLayout(digest, layoutWorkDir); linkErr != nil {
			return nil, fmt.Errorf("link blob %s to layout: %w", digest[:12], linkErr)
		}
	}

	// Register tag + blob refs inside txn lock (same pattern as build).
	info := &assemblyInfo{
		manifestDigest: "sha256:" + manifestDigest.Hex,
		blobDigests:    blobDigests,
		blobSizes:      blobSizes,
	}
	layoutDir, err := registerBlobRefsAndSaveTag(store, cfg, ref, layoutWorkDir, info)
	if err != nil {
		return nil, fmt.Errorf("register pull result: %w", err)
	}
	cleanupWorkDir = false // layoutWorkDir was renamed by finalizeLayoutDir

	fmt.Fprintf(os.Stderr, "Pulled: %s (digest: sha256:%s)\n", ref, manifestDigest.Hex)

	return &PullResult{
		Ref:            ref,
		ManifestDigest: "sha256:" + manifestDigest.Hex,
		LayoutPath:     layoutDir,
	}, nil
}

// ProbeRegistryVMImage checks whether the given ref points to a Cocoon VM
// image on a remote registry by fetching just the manifest and validating
// its media types. Returns false on any error (safe fallback to cloud pipeline).
func ProbeRegistryVMImage(ctx context.Context, cfg *config.CocoonConfig, ref string) bool {
	ref = EnsureLatestTag(ref)
	tag, err := name.NewTag(ref)
	if err != nil {
		return false
	}

	keychain := authn.NewMultiKeychain(CocoonKeychain(), authn.DefaultKeychain)
	desc, err := remote.Get(tag,
		remote.WithAuthFromKeychain(keychain),
		remote.WithContext(ctx),
		remote.WithPlatform(hostPlatform()),
	)
	if err != nil {
		return false
	}

	return validateCocoonVMManifest(desc.Manifest) == nil
}

// validateCocoonVMManifest checks that rawManifest describes a valid Cocoon VM image.
func validateCocoonVMManifest(rawManifest []byte) error {
	var manifest ociManifest
	if err := json.Unmarshal(rawManifest, &manifest); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}

	// Check Cocoon VM identification: artifactType, config mediaType, or layer types.
	hasArtifactType := manifest.ArtifactType == ArtifactTypeVMImage
	hasConfigType := manifest.Config.MediaType == MediaTypeVMConfig
	hasKernel := false
	hasRootfs := false
	for _, layer := range manifest.Layers {
		switch layer.MediaType {
		case MediaTypeKernelLayer:
			hasKernel = true
		case MediaTypeRootfsLayer:
			hasRootfs = true
		}
	}
	hasLayerTypes := hasKernel && hasRootfs

	if !hasArtifactType && !hasConfigType && !hasLayerTypes {
		return fmt.Errorf("not a Cocoon VM image: missing artifactType %q, config mediaType %q, and kernel+rootfs layer types",
			ArtifactTypeVMImage, MediaTypeVMConfig)
	}

	// Validate layer count.
	if len(manifest.Layers) > maxPullLayers {
		return fmt.Errorf("too many layers (%d): Cocoon VM images support at most %d layers (1 kernel + 1 rootfs + %d customization)",
			len(manifest.Layers), maxPullLayers, maxPullLayers-2)
	}

	return nil
}

// classifyPullError categorizes pull errors as transient or permanent.
func classifyPullError(err error) error {
	errStr := err.Error()

	permanentPatterns := []string{
		"UNAUTHORIZED", "unauthorized",
		"DENIED", "denied",
		"FORBIDDEN", "forbidden",
		"NAME_UNKNOWN", "not found",
		"MANIFEST_UNKNOWN",
		"invalid reference",
	}
	for _, p := range permanentPatterns {
		if strings.Contains(errStr, p) {
			return types.NewPermanentError(fmt.Errorf("pull %w", err))
		}
	}

	if isNetworkError(err) {
		return types.NewTransientError(fmt.Errorf("pull %w", err))
	}
	transientPatterns := []string{
		"timeout", "connection refused", "connection reset",
		"EOF", "INTERNAL_ERROR", "BAD_GATEWAY",
		"SERVICE_UNAVAILABLE", "TOO_MANY_REQUESTS",
		"Service Unavailable", "Too Many Requests",
		"Internal Server Error", "Bad Gateway",
	}
	errUpper := strings.ToUpper(errStr)
	for _, p := range transientPatterns {
		if strings.Contains(errUpper, strings.ToUpper(p)) {
			return types.NewTransientError(fmt.Errorf("pull %w", err))
		}
	}

	return types.NewPermanentError(fmt.Errorf("pull %w", err))
}

// checkIdempotent returns true and the layout path if the tag already exists
// with the same manifest digest.
func checkIdempotent(store *Store, ref, manifestHex string) (bool, string) {
	has, err := store.HasTag(ref)
	if err != nil || !has {
		return false, ""
	}
	layoutPath, err := store.ResolveTag(ref)
	if err != nil {
		return false, ""
	}
	info, err := InspectLayout(layoutPath)
	if err != nil {
		return false, ""
	}
	if info.ManifestDigest == "sha256:"+manifestHex {
		return true, layoutPath
	}
	return false, ""
}

// pullLayerToStore downloads a layer to a temp file and stores it in the blob store.
func pullLayerToStore(blobStore *BlobStore, layer v1.Layer, digestHexStr string) error {
	rc, err := layer.Compressed()
	if err != nil {
		return classifyPullError(err)
	}
	defer rc.Close() //nolint:errcheck

	// Write to temp file, verify digest, then store.
	tmpFile, err := os.CreateTemp("", "cocoon-pull-layer-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath) //nolint:errcheck

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmpFile, h), rc); err != nil {
		_ = tmpFile.Close()
		return classifyPullError(err)
	}
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	actualDigest := hex.EncodeToString(h.Sum(nil))
	if actualDigest != digestHexStr {
		return types.NewPermanentError(fmt.Errorf("layer digest mismatch: expected %s, got %s", digestHexStr, actualDigest))
	}

	if _, err := blobStore.StoreBlob(tmpPath, digestHexStr); err != nil {
		return fmt.Errorf("store blob: %w", err)
	}
	return nil
}

// hostPlatform returns the OCI platform for the current host.
func hostPlatform() v1.Platform {
	return v1.Platform{
		Architecture: runtime.GOARCH,
		OS:           "linux",
	}
}

// createPullLayoutWorkDir creates a temp directory for the OCI layout under
// the temp dir (not under layouts/) so GC never sweeps in-progress pulls.
func createPullLayoutWorkDir(cfg *config.CocoonConfig) (string, error) {
	layoutTempRoot := filepath.Join(cfg.TempDir(), "oci-layout-pulls")
	if err := os.MkdirAll(layoutTempRoot, 0o750); err != nil {
		return "", fmt.Errorf("create OCI layout pull temp root: %w", err)
	}
	workDir, err := os.MkdirTemp(layoutTempRoot, "layout-pull-*")
	if err != nil {
		return "", fmt.Errorf("create OCI layout pull temp dir: %w", err)
	}
	return workDir, nil
}

// stripSHA256Prefix extracts the hex part from a "sha256:hex" digest string.
func stripSHA256Prefix(digest string) string {
	const prefix = "sha256:"
	if strings.HasPrefix(digest, prefix) {
		return digest[len(prefix):]
	}
	return digest
}

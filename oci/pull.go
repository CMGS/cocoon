package oci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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

const pullProgressRefreshInterval = 250 * time.Millisecond

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
		return nil, ClassifyRegistryError("pull", err)
	}

	// Fetch and validate the manifest.
	rawManifest, err := img.RawManifest()
	if err != nil {
		return nil, ClassifyRegistryError("pull", err)
	}
	if validateErr := validateCocoonVMManifest(rawManifest); validateErr != nil {
		return nil, types.NewPermanentError(fmt.Errorf("validate manifest for %q: %w", ref, validateErr))
	}

	manifestDigest, err := img.Digest()
	if err != nil {
		return nil, ClassifyRegistryError("pull", err)
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
		return nil, ClassifyRegistryError("pull", err)
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
		return nil, ClassifyRegistryError("pull", err)
	}
	for i, layer := range layers {
		layerDigest, layerDigestErr := layer.Digest()
		if layerDigestErr != nil {
			return nil, ClassifyRegistryError("pull", layerDigestErr)
		}
		layerSize, layerSizeErr := layer.Size()
		if layerSizeErr != nil {
			return nil, ClassifyRegistryError("pull", layerSizeErr)
		}
		layerHex := layerDigest.Hex

		if !blobStore.BlobExists(layerHex) {
			progress := newPullLayerProgress(os.Stderr, i+1, len(layers), layerHex, layerSize)
			if pullErr := pullLayerToStore(blobStore, layer, layerHex, progress); pullErr != nil {
				return nil, fmt.Errorf("store layer %d (sha256:%s): %w", i+1, layerHex[:12], pullErr)
			}
		} else {
			fmt.Fprintf(os.Stderr, "  Layer %d/%d: sha256:%s (%s) [cached]\n",
				i+1, len(layers), layerHex[:12], utils.HumanBytes(layerSize))
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
	defer os.RemoveAll(layoutWorkDir) //nolint:errcheck // best-effort cleanup if finalization fails or races

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
	vmType, err := ProbeRegistryVMImageType(ctx, cfg, ref)
	if err != nil {
		log.Printf("warning: probe registry VM image %q: %v", ref, err)
		return false
	}
	return vmType == types.VMImageTypeOCIVM
}

// validateCocoonVMManifest checks that rawManifest describes a valid Cocoon VM image.
func validateCocoonVMManifest(rawManifest []byte) error {
	var manifest ociManifest
	if err := json.Unmarshal(rawManifest, &manifest); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}

	// Validate image identity and runtime contract fields.
	if manifest.Config.MediaType != MediaTypeVMConfig {
		return fmt.Errorf("invalid config mediaType %q: expected %q", manifest.Config.MediaType, MediaTypeVMConfig)
	}
	if manifest.ArtifactType != "" && manifest.ArtifactType != ArtifactTypeVMImage {
		return fmt.Errorf("invalid artifactType %q: expected %q or empty", manifest.ArtifactType, ArtifactTypeVMImage)
	}

	// Validate layer count.
	if len(manifest.Layers) > maxPullLayers {
		return fmt.Errorf("too many layers (%d): Cocoon VM images support at most %d layers (1 kernel + 1 rootfs + %d customization)",
			len(manifest.Layers), maxPullLayers, maxPullLayers-2)
	}
	if len(manifest.Layers) == 0 {
		return fmt.Errorf("missing layers: expected 1 kernel + at least 1 rootfs layer")
	}

	kernelCount := 0
	rootfsCount := 0
	for idx, layer := range manifest.Layers {
		switch layer.MediaType {
		case MediaTypeKernelLayer:
			kernelCount++
			if idx != 0 {
				return fmt.Errorf("invalid layer[%d] mediaType %q: kernel layer must be first", idx, layer.MediaType)
			}
		case MediaTypeRootfsLayer:
			rootfsCount++
		default:
			return fmt.Errorf("unsupported layer mediaType %q at index %d", layer.MediaType, idx)
		}
	}
	if kernelCount != 1 {
		return fmt.Errorf("invalid kernel layer count %d: expected exactly 1", kernelCount)
	}
	if rootfsCount < 1 {
		return fmt.Errorf("missing rootfs layer: expected at least one %q layer", MediaTypeRootfsLayer)
	}

	return nil
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
func pullLayerToStore(blobStore *BlobStore, layer v1.Layer, digestHexStr string, progress *pullLayerProgress) error {
	rc, err := layer.Compressed()
	if err != nil {
		return ClassifyRegistryError("pull", err)
	}
	defer rc.Close() //nolint:errcheck

	// Write to temp file, verify digest, then store.
	tempRoot := blobStore.cfg.TempDir()
	if mkErr := os.MkdirAll(tempRoot, 0o750); mkErr != nil {
		return fmt.Errorf("create pull temp dir: %w", mkErr)
	}
	tmpFile, err := os.CreateTemp(tempRoot, "cocoon-pull-layer-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath) //nolint:errcheck

	if progress != nil {
		progress.start()
		defer progress.finish()
	}

	h := sha256.New()
	writers := []io.Writer{tmpFile, h}
	if progress != nil {
		writers = append(writers, &pullProgressCounter{progress: progress})
	}
	if _, err := io.Copy(io.MultiWriter(writers...), rc); err != nil {
		_ = tmpFile.Close()
		return ClassifyRegistryError("pull", err)
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

type pullLayerProgress struct {
	writer      io.Writer
	layerIndex  int
	layerCount  int
	digestShort string
	totalBytes  int64

	completed atomic.Int64
	done      chan struct{}
	once      sync.Once
	wg        sync.WaitGroup
}

type pullProgressCounter struct {
	progress *pullLayerProgress
}

func (c *pullProgressCounter) Write(p []byte) (int, error) {
	if c.progress != nil {
		c.progress.completed.Add(int64(len(p)))
	}
	return len(p), nil
}

func newPullLayerProgress(writer io.Writer, layerIndex, layerCount int, digestHex string, totalBytes int64) *pullLayerProgress {
	digestShort := digestHex
	if len(digestShort) > 12 {
		digestShort = digestShort[:12]
	}
	return &pullLayerProgress{
		writer:      writer,
		layerIndex:  layerIndex,
		layerCount:  layerCount,
		digestShort: digestShort,
		totalBytes:  totalBytes,
	}
}

func (p *pullLayerProgress) start() {
	if p == nil || p.writer == nil {
		return
	}
	p.done = make(chan struct{})
	p.wg.Go(func() {
		ticker := time.NewTicker(pullProgressRefreshInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				_, _ = fmt.Fprintf(p.writer, "\r%s", formatPullLayerProgressLine(
					p.layerIndex,
					p.layerCount,
					p.digestShort,
					p.completed.Load(),
					p.totalBytes,
				))
			case <-p.done:
				_, _ = fmt.Fprintf(p.writer, "\r%s\n", formatPullLayerProgressLine(
					p.layerIndex,
					p.layerCount,
					p.digestShort,
					p.completed.Load(),
					p.totalBytes,
				))
				return
			}
		}
	})
}

func (p *pullLayerProgress) finish() {
	if p == nil {
		return
	}
	p.once.Do(func() {
		if p.done != nil {
			close(p.done)
		}
		p.wg.Wait()
	})
}

func formatPullLayerProgressLine(layerIndex, layerCount int, digestShort string, complete, total int64) string {
	if total > 0 {
		percent := int64(0)
		if complete > 0 {
			percent = (complete * 100) / total
		}
		if percent > 100 {
			percent = 100
		}
		return fmt.Sprintf(
			"  Layer %d/%d: sha256:%s %3d%% (%s / %s)",
			layerIndex,
			layerCount,
			digestShort,
			percent,
			utils.HumanBytes(complete),
			utils.HumanBytes(total),
		)
	}
	return fmt.Sprintf(
		"  Layer %d/%d: sha256:%s %s",
		layerIndex,
		layerCount,
		digestShort,
		utils.HumanBytes(complete),
	)
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

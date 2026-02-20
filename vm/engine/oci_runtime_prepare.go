package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/lock/flock"
	"github.com/CMGS/cocoon/oci"
	"github.com/CMGS/cocoon/utils"
)

const (
	defaultOCIRuntimeVirtioFSTag = "/dev/root"
)

const (
	minOCIRuntimeKernelBytes = 1024
	minOCIRuntimeInitrdBytes = 1
)

type ociRuntimeSpec struct {
	LocalTag        string
	ManifestDigest  string
	RuntimeKey      string
	Arch            string
	RootfsLowerDirs []string
	KernelPath      string
	InitramfsPath   string
	Cmdline         string
	VirtioFSTag     string
}

func prepareLocalOCIRuntime(ctx context.Context, cfg *config.CocoonConfig, localTag string) (*ociRuntimeSpec, error) {
	localTag = strings.TrimSpace(localTag)
	if localTag == "" {
		return nil, fmt.Errorf("OCI runtime requires a local OCI tag")
	}

	// Keep the txn lock for the full prepare+materialize window so the layout
	// cannot be concurrently removed/repointed by tag mutations while we read
	// rootfs/kernel blobs. Per-runtime lock below serializes same-manifest work.
	txnLock := flock.New(cfg.OCIBuildTxnLock())
	if err := txnLock.Lock(); err != nil {
		return nil, fmt.Errorf("acquire OCI build txn lock: %w", err)
	}
	defer txnLock.Unlock() //nolint:errcheck

	store := oci.NewStore(cfg)
	entry, err := store.GetTag(localTag)
	if err != nil {
		return nil, fmt.Errorf("resolve local OCI tag %q: %w", localTag, err)
	}
	if _, statErr := os.Stat(entry.LayoutPath); statErr != nil {
		return nil, fmt.Errorf("local OCI layout for %q is unavailable at %s: %w", localTag, entry.LayoutPath, statErr)
	}

	layoutInfo, err := oci.InspectLayout(entry.LayoutPath)
	if err != nil {
		return nil, fmt.Errorf("inspect OCI layout for %q: %w", localTag, err)
	}
	runtimeKey, err := oci.ParseSHA256Digest(layoutInfo.ManifestDigest)
	if err != nil {
		return nil, err
	}

	lockPath := filepath.Join(cfg.ConversionLockDir(), "oci-runtime-"+runtimeKey+".lock")
	runtimeLock := flock.New(lockPath)
	if lockErr := runtimeLock.Lock(); lockErr != nil {
		return nil, fmt.Errorf("acquire OCI runtime lock for %s: %w", runtimeKey, lockErr)
	}
	defer runtimeLock.Unlock() //nolint:errcheck

	if matErr := materializeOCIRuntimeCache(ctx, cfg, runtimeKey, entry.LayoutPath, layoutInfo); matErr != nil {
		return nil, matErr
	}

	// Read the entry metadata written by materializeOCIRuntimeCache to
	// derive lowerdir paths, kernel paths, and runtime config.
	entryMeta, err := oci.ReadEntryMeta(cfg.OCIRuntimeEntryMetaPath(runtimeKey))
	if err != nil {
		return nil, fmt.Errorf("read OCI runtime entry meta for %s: %w", runtimeKey, err)
	}

	// Build rootfs lowerdir list: OverlayFS lowerdir is leftmost=highest
	// priority (top layer). OCI layers are stored base-to-top in meta, so
	// reverse for lowerdir order.
	rootfsLowerDirs := make([]string, len(entryMeta.RootfsLayerDigests))
	for i, digest := range entryMeta.RootfsLayerDigests {
		hex, hexErr := oci.ParseSHA256Digest("sha256:" + digest)
		if hexErr != nil {
			return nil, fmt.Errorf("invalid rootfs layer digest in entry meta: %w", hexErr)
		}
		rootfsLowerDirs[len(rootfsLowerDirs)-1-i] = filepath.Join(cfg.OCIRuntimeLayerDir(hex), "rootfs")
	}

	kernelHex, err := oci.ParseSHA256Digest("sha256:" + entryMeta.KernelLayerDigest)
	if err != nil {
		return nil, fmt.Errorf("invalid kernel layer digest in entry meta: %w", err)
	}
	kernelLayerDir := filepath.Join(cfg.OCIRuntimeLayerDir(kernelHex), "kernel")

	virtiofsTag := entryMeta.VirtioFSTag
	if strings.TrimSpace(virtiofsTag) == "" {
		virtiofsTag = defaultOCIRuntimeVirtioFSTag
	}

	cmdline := normalizeVirtiofsKernelCmdline(entryMeta.KernelCmdline, virtiofsTag)

	return &ociRuntimeSpec{
		LocalTag:        localTag,
		ManifestDigest:  layoutInfo.ManifestDigest,
		RuntimeKey:      runtimeKey,
		Arch:            entryMeta.Arch,
		RootfsLowerDirs: rootfsLowerDirs,
		KernelPath:      filepath.Join(kernelLayerDir, "vmlinuz"),
		InitramfsPath:   filepath.Join(kernelLayerDir, "initrd.img"),
		Cmdline:         cmdline,
		VirtioFSTag:     virtiofsTag,
	}, nil
}

func materializeOCIRuntimeCache(ctx context.Context, cfg *config.CocoonConfig, runtimeKey, layoutPath string, info *oci.LayoutInfo) error {
	if info == nil {
		return fmt.Errorf("inspect OCI layout: empty metadata")
	}

	// Fast path: if entry metadata already exists, layers are cached.
	if oci.EntryMetaExists(cfg.OCIRuntimeEntryMetaPath(runtimeKey)) {
		return nil
	}

	kernelLayer, rootfsLayers, err := locateOCIRuntimeLayers(info)
	if err != nil {
		return err
	}

	// Extract kernel layer.
	kernelHex, err := oci.ParseSHA256Digest(kernelLayer.Digest)
	if err != nil {
		return fmt.Errorf("parse kernel layer digest: %w", err)
	}
	if err := extractLayerToCache(ctx, cfg, kernelHex, layoutPath, kernelLayer, true, info.Config); err != nil {
		return err
	}

	// Extract rootfs layers.
	rootfsDigests := make([]string, 0, len(rootfsLayers))
	for _, layer := range rootfsLayers {
		hex, hexErr := oci.ParseSHA256Digest(layer.Digest)
		if hexErr != nil {
			return fmt.Errorf("parse rootfs layer digest: %w", hexErr)
		}
		if hexErr = extractLayerToCache(ctx, cfg, hex, layoutPath, layer, false, nil); hexErr != nil {
			return hexErr
		}
		rootfsDigests = append(rootfsDigests, hex)
	}

	// Determine arch and cmdline from layout config.
	arch := runtime.GOARCH
	var kernelCmdline string
	if info.Config != nil {
		if strings.TrimSpace(info.Config.Arch) != "" {
			arch = strings.TrimSpace(info.Config.Arch)
		}
		if strings.TrimSpace(info.Config.KernelCmdline) != "" {
			kernelCmdline = strings.TrimSpace(info.Config.KernelCmdline)
		}
	}

	// Write entry metadata.
	entryMeta := &oci.OCIRuntimeEntryMeta{
		ManifestDigest:     info.ManifestDigest,
		KernelLayerDigest:  kernelHex,
		RootfsLayerDigests: rootfsDigests,
		Arch:               arch,
		KernelCmdline:      kernelCmdline,
		VirtioFSTag:        defaultOCIRuntimeVirtioFSTag,
	}

	entryDir := cfg.OCIRuntimeEntryDir(runtimeKey)
	if err := os.MkdirAll(entryDir, 0o755); err != nil { //nolint:gosec // cocoon-managed cache dir
		return fmt.Errorf("create OCI runtime entry dir: %w", err)
	}
	if err := oci.WriteEntryMeta(cfg.OCIRuntimeEntryMetaPath(runtimeKey), entryMeta); err != nil {
		return fmt.Errorf("write OCI runtime entry meta for %s: %w", runtimeKey, err)
	}
	return nil
}

// extractLayerToCache extracts a single OCI layer into the shared layer cache
// at layers/{hexDigest}/. Uses a per-layer flock for concurrent dedup.
func extractLayerToCache(ctx context.Context, cfg *config.CocoonConfig, hexDigest, layoutPath string, layer oci.LayerInfo, isKernel bool, vmCfg *oci.VMImageConfig) error {
	layerDir := cfg.OCIRuntimeLayerDir(hexDigest)
	if statPathExists(layerDir) {
		return nil // already cached
	}

	// Per-layer flock to prevent concurrent duplicate extraction.
	lockPath := filepath.Join(cfg.ConversionLockDir(), "oci-layer-"+hexDigest+".lock")
	layerLock := flock.New(lockPath)
	if err := layerLock.Lock(); err != nil {
		return fmt.Errorf("acquire OCI layer lock for %s: %w", hexDigest, err)
	}
	defer layerLock.Unlock() //nolint:errcheck

	// Double-check after acquiring lock.
	if statPathExists(layerDir) {
		return nil
	}

	layersDir := cfg.OCIRuntimeLayersDir()
	if err := os.MkdirAll(layersDir, 0o755); err != nil { //nolint:gosec // cocoon-managed cache dir
		return fmt.Errorf("create OCI runtime layers dir: %w", err)
	}

	// Build workspace under layers dir so os.Rename is atomic (same filesystem).
	workDir, err := os.MkdirTemp(layersDir, hexDigest+"-work-*")
	if err != nil {
		return fmt.Errorf("create OCI layer temp dir: %w", err)
	}
	defer os.RemoveAll(workDir) //nolint:errcheck,gosec // best-effort cleanup

	blobPath, err := oci.LayoutBlobPath(layoutPath, layer.Digest)
	if err != nil {
		return fmt.Errorf("resolve OCI layer blob %s: %w", layer.Digest, err)
	}

	if isKernel {
		if err := extractKernelLayer(ctx, workDir, blobPath, layer.Digest, hexDigest, vmCfg); err != nil {
			return err
		}
	} else {
		if err := extractRootfsLayer(ctx, workDir, blobPath, layer.Digest); err != nil {
			return err
		}
	}

	// Atomic promotion: rename work dir to final layer dir.
	if err := os.Rename(workDir, layerDir); err != nil {
		return fmt.Errorf("promote OCI layer cache %s: %w", hexDigest, err)
	}
	return nil
}

func extractKernelLayer(ctx context.Context, workDir, blobPath, digest, hexDigest string, vmCfg *oci.VMImageConfig) error {
	kernelDir := filepath.Join(workDir, "kernel")
	if err := os.MkdirAll(kernelDir, 0o755); err != nil { //nolint:gosec // cocoon-managed cache dir
		return fmt.Errorf("create kernel extract dir: %w", err)
	}
	if err := utils.ExtractOCILayerTarToDir(ctx, blobPath, kernelDir); err != nil {
		return fmt.Errorf("extract OCI kernel layer %s: %w", digest, err)
	}
	if err := installOCIRuntimeKernelArtifacts(kernelDir, vmCfg); err != nil {
		return err
	}
	initrdPath := filepath.Join(kernelDir, "initrd.img")
	if err := validateOCIRuntimeInitramfsVirtiofs(initrdPath); err != nil {
		return fmt.Errorf("OCI runtime kernel layer %s: %w", hexDigest, err)
	}
	return nil
}

func extractRootfsLayer(ctx context.Context, workDir, blobPath, digest string) error {
	rootfsDir := filepath.Join(workDir, "rootfs")
	if err := os.MkdirAll(rootfsDir, 0o755); err != nil { //nolint:gosec // cocoon-managed cache dir
		return fmt.Errorf("create rootfs extract dir: %w", err)
	}
	if err := utils.ExtractOCILayerTarToDir(ctx, blobPath, rootfsDir); err != nil {
		return fmt.Errorf("extract OCI rootfs layer %s: %w", digest, err)
	}
	return nil
}

func locateOCIRuntimeLayers(info *oci.LayoutInfo) (oci.LayerInfo, []oci.LayerInfo, error) {
	if info == nil {
		return oci.LayerInfo{}, nil, fmt.Errorf("OCI layout metadata is empty")
	}
	var kernelLayer oci.LayerInfo
	kernelCount := 0
	rootfsLayers := make([]oci.LayerInfo, 0, len(info.Layers))
	for idx, layer := range info.Layers {
		switch layer.MediaType {
		case oci.MediaTypeKernelLayer:
			if idx != 0 {
				return oci.LayerInfo{}, nil, fmt.Errorf("OCI runtime kernel layer must be first (index 0), got index %d", idx)
			}
			kernelCount++
			if kernelLayer.Digest == "" {
				kernelLayer = layer
			}
		case oci.MediaTypeRootfsLayer:
			rootfsLayers = append(rootfsLayers, layer)
		}
	}
	if kernelCount == 0 {
		return oci.LayerInfo{}, nil, fmt.Errorf("OCI runtime kernel layer not found")
	}
	if kernelCount > 1 {
		return oci.LayerInfo{}, nil, fmt.Errorf("OCI runtime has %d kernel layers; expected exactly 1", kernelCount)
	}
	if len(rootfsLayers) == 0 {
		return oci.LayerInfo{}, nil, fmt.Errorf("OCI runtime rootfs layer not found")
	}
	return kernelLayer, rootfsLayers, nil
}

func installOCIRuntimeKernelArtifacts(kernelDir string, cfg *oci.VMImageConfig) error {
	kernelCandidates := []string{"vmlinuz"}
	initrdCandidates := []string{"initrd.img", "initramfs.img"}
	if cfg != nil {
		if p := strings.TrimPrefix(strings.TrimSpace(cfg.KernelPath), "/"); p != "" {
			kernelCandidates = append([]string{filepath.FromSlash(p)}, kernelCandidates...)
		}
		if p := strings.TrimPrefix(strings.TrimSpace(cfg.InitrdPath), "/"); p != "" {
			initrdCandidates = append([]string{filepath.FromSlash(p)}, initrdCandidates...)
		}
	}

	kernelSrc, err := utils.FirstExistingPath(kernelDir, kernelCandidates)
	if err != nil {
		return fmt.Errorf("locate OCI runtime kernel in layer: %w", err)
	}
	if sizeErr := ensureRuntimeArtifactMinSize(kernelSrc, minOCIRuntimeKernelBytes, "kernel"); sizeErr != nil {
		return sizeErr
	}
	initrdSrc, err := utils.FirstExistingPath(kernelDir, initrdCandidates)
	if err != nil {
		return fmt.Errorf("locate OCI runtime initrd in layer: %w", err)
	}
	if sizeErr := ensureRuntimeArtifactMinSize(initrdSrc, minOCIRuntimeInitrdBytes, "initrd"); sizeErr != nil {
		return sizeErr
	}

	kernelDst := filepath.Join(kernelDir, "vmlinuz")
	if filepath.Clean(kernelSrc) != filepath.Clean(kernelDst) {
		if err := utils.CopyFile(kernelSrc, kernelDst, 0o644); err != nil { //nolint:gosec // cocoon-managed cache file
			return fmt.Errorf("write OCI runtime kernel artifact: %w", err)
		}
	}

	initrdDst := filepath.Join(kernelDir, "initrd.img")
	if filepath.Clean(initrdSrc) != filepath.Clean(initrdDst) {
		if err := utils.CopyFile(initrdSrc, initrdDst, 0o644); err != nil { //nolint:gosec // cocoon-managed cache file
			return fmt.Errorf("write OCI runtime initrd artifact: %w", err)
		}
	}
	return nil
}

func ensureRuntimeArtifactMinSize(path string, minBytes int64, kind string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat OCI runtime %s artifact %s: %w", kind, path, err)
	}
	if fi.Size() < minBytes {
		return fmt.Errorf("OCI runtime %s artifact %s is too small (%d bytes, minimum %d)", kind, path, fi.Size(), minBytes)
	}
	return nil
}

func normalizeVirtiofsKernelCmdline(raw, virtiofsTag string) string {
	if strings.TrimSpace(virtiofsTag) == "" {
		virtiofsTag = defaultOCIRuntimeVirtioFSTag
	}

	fields := strings.Fields(strings.TrimSpace(raw))
	out := make([]string, 0, len(fields)+5)
	for _, field := range fields {
		switch {
		case strings.HasPrefix(field, "root="),
			strings.HasPrefix(field, "rootfstype="),
			field == "ro",
			field == "rw",
			strings.HasPrefix(field, "console="):
			continue
		}
		out = append(out, field)
	}
	// Ensure serial output is always visible to boot detection (ttyS0), while
	// keeping hvc0 as the primary console by appending it last.
	out = append(out, "console=ttyS0", "console=hvc0")
	// The runtime overlay upperdir is writable, so force rw semantics even if
	// source cmdline requested ro to avoid guest booting with a read-only root.
	out = append(out, "root="+virtiofsTag, "rootfstype=virtiofs", "rw")
	return strings.Join(out, " ")
}

func validateOCIRuntimeInitramfsVirtiofs(initramfsPath string) error {
	path := strings.TrimSpace(initramfsPath)
	if path == "" {
		return fmt.Errorf("initramfs path is empty")
	}

	found, err := oci.CheckInitramfsVirtiofsFromInitrdPath(path)
	if err != nil {
		return fmt.Errorf("virtiofs module detection failed for %s: %w", path, err)
	}
	if !found {
		return fmt.Errorf(
			"virtiofs module not found in initramfs %s; rebuild image/initramfs with virtiofs support before create/run",
			path,
		)
	}
	return nil
}

func statPathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

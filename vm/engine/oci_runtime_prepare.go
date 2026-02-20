package engine

import (
	"context"
	"fmt"
	"log"
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
	LocalTag       string
	ManifestDigest string
	RuntimeKey     string
	Arch           string
	RootfsLowerDir string
	KernelPath     string
	InitramfsPath  string
	Cmdline        string
	VirtioFSTag    string
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
	if err := runtimeLock.Lock(); err != nil {
		return nil, fmt.Errorf("acquire OCI runtime lock for %s: %w", runtimeKey, err)
	}
	defer runtimeLock.Unlock() //nolint:errcheck

	if err := materializeOCIRuntimeCache(ctx, cfg, runtimeKey, entry.LayoutPath, layoutInfo); err != nil {
		return nil, err
	}

	virtiofsTag := defaultOCIRuntimeVirtioFSTag
	cmdline := normalizeVirtiofsKernelCmdline("", virtiofsTag)
	arch := runtime.GOARCH
	if layoutInfo.Config != nil {
		if strings.TrimSpace(layoutInfo.Config.KernelCmdline) != "" {
			cmdline = normalizeVirtiofsKernelCmdline(layoutInfo.Config.KernelCmdline, virtiofsTag)
		}
		if strings.TrimSpace(layoutInfo.Config.Arch) != "" {
			arch = strings.TrimSpace(layoutInfo.Config.Arch)
		}
	}

	return &ociRuntimeSpec{
		LocalTag:       localTag,
		ManifestDigest: layoutInfo.ManifestDigest,
		RuntimeKey:     runtimeKey,
		Arch:           arch,
		RootfsLowerDir: cfg.OCIRuntimeRootfsDir(runtimeKey),
		KernelPath:     cfg.OCIRuntimeKernelPath(runtimeKey),
		InitramfsPath:  cfg.OCIRuntimeInitrdPath(runtimeKey),
		Cmdline:        cmdline,
		VirtioFSTag:    virtiofsTag,
	}, nil
}

func materializeOCIRuntimeCache(ctx context.Context, cfg *config.CocoonConfig, runtimeKey, layoutPath string, info *oci.LayoutInfo) error {
	if info == nil {
		return fmt.Errorf("inspect OCI layout: empty metadata")
	}

	rootfsDir := cfg.OCIRuntimeRootfsDir(runtimeKey)
	kernelPath := cfg.OCIRuntimeKernelPath(runtimeKey)
	initrdPath := cfg.OCIRuntimeInitrdPath(runtimeKey)
	if statPathExists(rootfsDir) && statPathExists(kernelPath) && statPathExists(initrdPath) {
		return nil
	}

	kernelLayer, rootfsLayers, err := locateOCIRuntimeLayers(info)
	if err != nil {
		return err
	}

	if err = os.MkdirAll(cfg.OCIRuntimeCacheDir(), 0o755); err != nil { //nolint:gosec // cocoon-managed cache dir
		return fmt.Errorf("create OCI runtime cache dir: %w", err)
	}
	// Build the workspace under OCIRuntimeCacheDir so directory promotions stay
	// on the same filesystem and os.Rename remains atomic.
	workDir, err := os.MkdirTemp(cfg.OCIRuntimeCacheDir(), runtimeKey+"-work-*")
	if err != nil {
		return fmt.Errorf("create OCI runtime temp dir: %w", err)
	}
	defer os.RemoveAll(workDir) //nolint:errcheck,gosec // best-effort cleanup

	workRootfs := filepath.Join(workDir, "rootfs")
	workKernel := filepath.Join(workDir, "kernel")
	if err = os.MkdirAll(workRootfs, 0o755); err != nil { //nolint:gosec // cocoon-managed temp dir
		return fmt.Errorf("create OCI runtime rootfs workspace: %w", err)
	}
	if err = os.MkdirAll(workKernel, 0o755); err != nil { //nolint:gosec // cocoon-managed temp dir
		return fmt.Errorf("create OCI runtime kernel workspace: %w", err)
	}

	for _, layer := range rootfsLayers {
		layerPath, layerErr := oci.LayoutBlobPath(layoutPath, layer.Digest)
		if layerErr != nil {
			return fmt.Errorf("resolve OCI rootfs layer %s: %w", layer.Digest, layerErr)
		}
		if layerErr = utils.ExtractOCILayerTarToDir(ctx, layerPath, workRootfs); layerErr != nil {
			return fmt.Errorf("extract OCI rootfs layer %s: %w", layer.Digest, layerErr)
		}
	}

	kernelLayerPath, err := oci.LayoutBlobPath(layoutPath, kernelLayer.Digest)
	if err != nil {
		return fmt.Errorf("resolve OCI kernel layer %s: %w", kernelLayer.Digest, err)
	}
	if err = utils.ExtractOCILayerTarToDir(ctx, kernelLayerPath, workKernel); err != nil {
		return fmt.Errorf("extract OCI kernel layer %s: %w", kernelLayer.Digest, err)
	}
	if err = installOCIRuntimeKernelArtifacts(workKernel, info.Config); err != nil {
		return err
	}
	if err = validateOCIRuntimeInitramfsVirtiofs(filepath.Join(workKernel, "initrd.img")); err != nil {
		return fmt.Errorf("OCI runtime initramfs check failed for %s: %w", runtimeKey, err)
	}

	if err := promoteOCIRuntimeCacheDir(workDir, cfg.OCIRuntimeEntryDir(runtimeKey)); err != nil {
		return fmt.Errorf("promote OCI runtime cache %s: %w", runtimeKey, err)
	}
	return nil
}

func promoteOCIRuntimeCacheDir(workDir, finalDir string) error {
	finalDir = filepath.Clean(finalDir)
	newDir := finalDir + ".new"
	oldDir := finalDir + ".old"

	// Best-effort cleanup from a prior interrupted promotion.
	_ = os.RemoveAll(newDir)
	_ = os.RemoveAll(oldDir)

	if err := os.Rename(workDir, newDir); err != nil {
		return fmt.Errorf("stage new OCI runtime cache: %w", err)
	}

	hadOld := false
	if err := os.Rename(finalDir, oldDir); err == nil {
		hadOld = true
	} else if !os.IsNotExist(err) {
		_ = os.RemoveAll(newDir)
		return fmt.Errorf("rotate existing OCI runtime cache: %w", err)
	}

	if err := os.Rename(newDir, finalDir); err != nil {
		// Best-effort rollback to keep the previous runtime cache available.
		if hadOld {
			if rollbackErr := os.Rename(oldDir, finalDir); rollbackErr != nil {
				log.Printf("warning: rollback OCI runtime cache promotion failed (%s -> %s): %v", oldDir, finalDir, rollbackErr)
			}
		}
		return fmt.Errorf("activate new OCI runtime cache: %w", err)
	}

	// Best-effort cleanup; stale .old directories are harmless and can be GC'd.
	if hadOld {
		_ = os.RemoveAll(oldDir)
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

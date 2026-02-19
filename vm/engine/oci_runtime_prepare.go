package engine

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/lock/flock"
	"github.com/CMGS/cocoon/oci"
	"github.com/CMGS/cocoon/utils"
)

const defaultOCIRuntimeVirtioFSTag = "cocoon-rootfs"

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
	runtimeKey, err := ociRuntimeKeyFromDigest(layoutInfo.ManifestDigest)
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
	if layoutInfo.Config != nil && strings.TrimSpace(layoutInfo.Config.VirtiofsTag) != "" {
		virtiofsTag = strings.TrimSpace(layoutInfo.Config.VirtiofsTag)
	}
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
	workDir, err := os.MkdirTemp(cfg.TempDir(), "oci-runtime-"+runtimeKey+"-*")
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
		layerPath, layerErr := ociLayoutBlobPath(layoutPath, layer.Digest)
		if layerErr != nil {
			return fmt.Errorf("resolve OCI rootfs layer %s: %w", layer.Digest, layerErr)
		}
		if layerErr = utils.ExtractOCILayerTarToDir(ctx, layerPath, workRootfs); layerErr != nil {
			return fmt.Errorf("extract OCI rootfs layer %s: %w", layer.Digest, layerErr)
		}
	}

	kernelLayerPath, err := ociLayoutBlobPath(layoutPath, kernelLayer.Digest)
	if err != nil {
		return fmt.Errorf("resolve OCI kernel layer %s: %w", kernelLayer.Digest, err)
	}
	if err = utils.ExtractOCILayerTarToDir(ctx, kernelLayerPath, workKernel); err != nil {
		return fmt.Errorf("extract OCI kernel layer %s: %w", kernelLayer.Digest, err)
	}
	if err = installOCIRuntimeKernelArtifacts(workKernel, info.Config); err != nil {
		return err
	}

	finalDir := cfg.OCIRuntimeEntryDir(runtimeKey)
	_ = os.RemoveAll(finalDir)
	if err := os.Rename(workDir, finalDir); err != nil {
		return fmt.Errorf("promote OCI runtime cache %s: %w", runtimeKey, err)
	}
	return nil
}

func locateOCIRuntimeLayers(info *oci.LayoutInfo) (oci.LayerInfo, []oci.LayerInfo, error) {
	if info == nil {
		return oci.LayerInfo{}, nil, fmt.Errorf("OCI layout metadata is empty")
	}
	var kernelLayer oci.LayerInfo
	rootfsLayers := make([]oci.LayerInfo, 0, len(info.Layers))
	for _, layer := range info.Layers {
		switch layer.MediaType {
		case oci.MediaTypeKernelLayer:
			if kernelLayer.Digest == "" {
				kernelLayer = layer
			}
		case oci.MediaTypeRootfsLayer:
			rootfsLayers = append(rootfsLayers, layer)
		}
	}
	if kernelLayer.Digest == "" {
		return oci.LayerInfo{}, nil, fmt.Errorf("OCI runtime kernel layer not found")
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

	kernelSrc, err := firstExistingPath(kernelDir, kernelCandidates)
	if err != nil {
		return fmt.Errorf("locate OCI runtime kernel in layer: %w", err)
	}
	initrdSrc, err := firstExistingPath(kernelDir, initrdCandidates)
	if err != nil {
		return fmt.Errorf("locate OCI runtime initrd in layer: %w", err)
	}

	kernelDst := filepath.Join(kernelDir, "vmlinuz")
	if filepath.Clean(kernelSrc) != filepath.Clean(kernelDst) {
		if err := copyRuntimeFile(kernelSrc, kernelDst, 0o644); err != nil { //nolint:gosec // cocoon-managed cache file
			return fmt.Errorf("write OCI runtime kernel artifact: %w", err)
		}
	}

	initrdDst := filepath.Join(kernelDir, "initrd.img")
	if filepath.Clean(initrdSrc) != filepath.Clean(initrdDst) {
		if err := copyRuntimeFile(initrdSrc, initrdDst, 0o644); err != nil { //nolint:gosec // cocoon-managed cache file
			return fmt.Errorf("write OCI runtime initrd artifact: %w", err)
		}
	}
	return nil
}

func normalizeVirtiofsKernelCmdline(raw, virtiofsTag string) string {
	virtiofsTag = strings.TrimSpace(virtiofsTag)
	if virtiofsTag == "" {
		virtiofsTag = defaultOCIRuntimeVirtioFSTag
	}

	fields := strings.Fields(strings.TrimSpace(raw))
	out := make([]string, 0, len(fields)+4)
	hasConsole := false
	for _, field := range fields {
		switch {
		case strings.HasPrefix(field, "root="),
			strings.HasPrefix(field, "rootfstype="),
			field == "ro",
			field == "rw":
			continue
		case strings.HasPrefix(field, "console="):
			hasConsole = true
		}
		out = append(out, field)
	}
	if !hasConsole {
		out = append(out, "console=hvc0")
	}
	out = append(out, "root="+virtiofsTag, "rootfstype=virtiofs", "rw")
	return strings.Join(out, " ")
}

func ociRuntimeKeyFromDigest(digest string) (string, error) {
	const prefix = "sha256:"
	if !strings.HasPrefix(digest, prefix) {
		return "", fmt.Errorf("invalid OCI manifest digest %q", digest)
	}
	hex := strings.TrimPrefix(digest, prefix)
	if len(hex) != 64 {
		return "", fmt.Errorf("invalid OCI manifest digest length for %q", digest)
	}
	for _, c := range hex {
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return "", fmt.Errorf("invalid OCI manifest digest characters for %q", digest)
		}
	}
	return strings.ToLower(hex), nil
}

func ociLayoutBlobPath(layoutPath, digest string) (string, error) {
	const prefix = "sha256:"
	if !strings.HasPrefix(digest, prefix) {
		return "", fmt.Errorf("unsupported OCI digest format %q", digest)
	}
	hex := strings.TrimPrefix(digest, prefix)
	if len(hex) != 64 {
		return "", fmt.Errorf("invalid OCI digest length for %q", digest)
	}
	for _, c := range hex {
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return "", fmt.Errorf("invalid OCI digest characters for %q", digest)
		}
	}
	return filepath.Join(layoutPath, "blobs", "sha256", hex), nil
}

func firstExistingPath(baseDir string, rels []string) (string, error) {
	for _, rel := range rels {
		p := filepath.Join(baseDir, rel)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("none of %v found under %s", rels, baseDir)
}

func copyRuntimeFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src) //nolint:gosec // source path is cocoon-managed
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode) //nolint:gosec // destination path is cocoon-managed
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func statPathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

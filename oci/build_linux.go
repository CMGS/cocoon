//go:build linux

package oci

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/utils"
)

// assemblyInfo holds the results of assembleOCILayout for blob ref tracking.
type assemblyInfo struct {
	manifestDigest string
	blobDigests    []string
	blobSizes      []int64
}

type layerInput struct {
	MediaType string
	DigestHex string
	Size      int64
	TarPath   string
}

func shouldUseDeltaLayer(buildCtx *BuildContext, cf *Cocoonfile) bool {
	if buildCtx == nil || cf == nil || len(cf.Steps) == 0 {
		return false
	}
	if buildCtx.SourceType != BuildSourceLocalOCITag {
		return false
	}
	if buildCtx.KernelLayer.Digest == "" || len(buildCtx.RootfsLayers) == 0 {
		return false
	}
	if strings.TrimSpace(buildCtx.BaseRootfsDir) == "" || strings.TrimSpace(buildCtx.BaseLayoutPath) == "" {
		return false
	}
	return true
}

// Build extracts kernel, initrd, and rootfs from a cloud image, packages them
// as deterministic tar layers, and stores the result as an OCI image layout.
//
// When cf is non-nil, Cocoonfile customization steps (RUN/COPY) are applied
// to the image via virt-customize before rootfs extraction.
//
// See docs/04.1-oci-vm-images.md Section 3 for the full build pipeline.
// validateBuildPrereqs checks that the image path is valid and required tools are available.
func validateBuildPrereqs(imagePath string, cf *Cocoonfile) error {
	if err := validateSafePath(imagePath); err != nil {
		return fmt.Errorf("invalid image path: %w", err)
	}
	if _, err := os.Stat(imagePath); err != nil {
		return fmt.Errorf("image file not found: %w", err)
	}
	if _, err := exec.LookPath("guestfish"); err != nil {
		return fmt.Errorf("guestfish not found in PATH: OCI VM image build requires libguestfs: %w", err)
	}
	if cf != nil && len(cf.Steps) > 0 {
		if _, err := exec.LookPath("virt-customize"); err != nil {
			return fmt.Errorf("virt-customize not found in PATH: Cocoonfile RUN/COPY requires libguestfs-tools: %w", err)
		}
	}
	return nil
}

func Build(ctx context.Context, cfg *config.CocoonConfig, imagePath, tag string, cf *Cocoonfile) (*BuildResult, error) {
	if err := validateBuildPrereqs(imagePath, cf); err != nil {
		return nil, err
	}

	buildCtx, err := ReadBuildContextForImage(imagePath)
	if err != nil {
		return nil, fmt.Errorf("read build context: %w", err)
	}
	useReusedDeltaLayer := shouldUseDeltaLayer(buildCtx, cf)
	useCustomizationDelta := cf != nil && len(cf.Steps) > 0 && !useReusedDeltaLayer

	// Calculate total build steps.
	// Base steps: detect kernel, extract kernel+initrd, build kernel layer,
	// read PARTUUID, extract rootfs, build rootfs layer, assemble OCI layout, save tag.
	totalSteps := 8
	if useReusedDeltaLayer {
		totalSteps = 7
	}
	if cf != nil && len(cf.Steps) > 0 {
		totalSteps += len(cf.Steps)
	}
	if useCustomizationDelta {
		// Additional steps beyond base build:
		// 1) prepare base rootfs snapshot
		// 2) extract customized rootfs
		// 3) build customization delta layer
		totalSteps += 3
	}
	pw := NewProgressWriter(os.Stderr, totalSteps)

	tmpDir, err := os.MkdirTemp(cfg.TempDir(), "oci-build-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir) //nolint:errcheck // best-effort cleanup

	// If Cocoonfile has customization steps, work on a writable copy.
	workImage, err := applyCocoonfileSteps(ctx, imagePath, tmpDir, cf, pw)
	if err != nil {
		return nil, err
	}

	kernelImage := workImage
	rootfsBaseImage := workImage
	baseRootfsDir := ""
	if useCustomizationDelta {
		rootfsBaseImage = imagePath
		baseRootfsDir, err = materializeBaseRootfsSnapshot(ctx, tmpDir, imagePath, pw)
		if err != nil {
			return nil, err
		}
	}

	// Step 1-3: List /boot and detect kernel.
	pw.Step("Detecting kernel...")
	bootFiles, err := listBootFiles(ctx, kernelImage)
	if err != nil {
		return nil, fmt.Errorf("list boot files: %w", err)
	}

	ki, err := DetectKernel(bootFiles)
	if err != nil {
		return nil, fmt.Errorf("detect kernel: %w", err)
	}
	pw.Detail(ki.Version)

	layerInputs, err := prepareLayerInputs(
		ctx,
		tmpDir,
		kernelImage,
		rootfsBaseImage,
		workImage,
		baseRootfsDir,
		ki,
		bootFiles,
		pw,
		buildCtx,
		useReusedDeltaLayer,
		useCustomizationDelta,
	)
	if err != nil {
		return nil, err
	}

	// Step 5: Read PARTUUID.
	pw.Step("Reading PARTUUID...")
	partUUID, err := readPartUUID(ctx, workImage, tmpDir)
	if err != nil {
		return nil, fmt.Errorf("read PARTUUID: %w", err)
	}
	if !isValidUUID(partUUID) && !isValidMBRPartUUID(partUUID) {
		return nil, fmt.Errorf("invalid PARTUUID format %q (expected UUID or MBR-style ID)", partUUID)
	}
	pw.Detail(partUUID)

	// Step 7-8: Build config blob and assemble OCI layout.
	vmConfig, err := buildVMConfig(cfg, partUUID, cf, buildCtx, useReusedDeltaLayer)
	if err != nil {
		return nil, err
	}

	blobStore := NewBlobStore(cfg)
	store := NewStore(cfg)
	layoutWorkDir, err := createLayoutWorkDir(cfg)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(layoutWorkDir) //nolint:errcheck // best-effort cleanup if finalization fails

	pw.Step("Assembling OCI layout...")
	baseLayoutPath := ""
	if useReusedDeltaLayer {
		baseLayoutPath = buildCtx.BaseLayoutPath
	}
	info, err := assembleOCILayout(layoutWorkDir, vmConfig, layerInputs, baseLayoutPath, blobStore)
	if err != nil {
		return nil, fmt.Errorf("assemble OCI layout: %w", err)
	}
	pw.Detail("sha256:" + info.manifestDigest[:12])

	// Save tag in local index. Once saved, CollectOrphanedOCILayouts will
	// not delete this layout directory.
	pw.Step("Saving tag...")
	layoutDir, err := registerBlobRefsAndSaveTag(store, cfg, tag, layoutWorkDir, info)
	if err != nil {
		return nil, err
	}
	pw.Detail(tag)

	return &BuildResult{
		Tag:            tag,
		KernelVersion:  ki.Version,
		ManifestDigest: info.manifestDigest,
		LayoutPath:     layoutDir,
	}, nil
}

func prepareLayerInputs(
	ctx context.Context,
	tmpDir string,
	kernelImage string,
	rootfsBaseImage string,
	workImage string,
	baseRootfsDir string,
	ki *KernelInfo,
	bootFiles []string,
	pw *ProgressWriter,
	buildCtx *BuildContext,
	useReusedDeltaLayer bool,
	useCustomizationDelta bool,
) ([]layerInput, error) {
	layerInputs := make([]layerInput, 0, 4)
	if useReusedDeltaLayer {
		if err := appendReusedBaseLayers(buildCtx, pw, &layerInputs); err != nil {
			return nil, err
		}
		if err := appendDeltaRootfsLayer(ctx, tmpDir, workImage, buildCtx.BaseRootfsDir, pw, &layerInputs); err != nil {
			return nil, err
		}
		return layerInputs, nil
	}

	if err := appendNewKernelLayer(ctx, tmpDir, kernelImage, ki, pw, &layerInputs); err != nil {
		return nil, err
	}
	if err := appendRootfsLayerFromImage(ctx, tmpDir, rootfsBaseImage, ki, bootFiles, pw, &layerInputs); err != nil {
		return nil, err
	}
	if useCustomizationDelta {
		if strings.TrimSpace(baseRootfsDir) == "" {
			return nil, fmt.Errorf("base rootfs snapshot path is empty")
		}
		if err := appendDeltaRootfsLayer(ctx, tmpDir, workImage, baseRootfsDir, pw, &layerInputs); err != nil {
			return nil, err
		}
	}
	return layerInputs, nil
}

func appendReusedBaseLayers(buildCtx *BuildContext, pw *ProgressWriter, layerInputs *[]layerInput) error {
	kernelHex, err := digestHex(buildCtx.KernelLayer.Digest)
	if err != nil {
		return fmt.Errorf("invalid base kernel layer digest: %w", err)
	}
	*layerInputs = append(*layerInputs, layerInput{
		MediaType: MediaTypeKernelLayer,
		DigestHex: kernelHex,
		Size:      buildCtx.KernelLayer.Size,
	})
	for _, layer := range buildCtx.RootfsLayers {
		digestHexValue, digestErr := digestHex(layer.Digest)
		if digestErr != nil {
			return fmt.Errorf("invalid base rootfs layer digest %q: %w", layer.Digest, digestErr)
		}
		*layerInputs = append(*layerInputs, layerInput{
			MediaType: MediaTypeRootfsLayer,
			DigestHex: digestHexValue,
			Size:      layer.Size,
		})
	}
	pw.Step("Reusing base layers...")
	pw.Detail(fmt.Sprintf("base rootfs layers: %d", len(buildCtx.RootfsLayers)))
	return nil
}

func appendNewKernelLayer(
	ctx context.Context,
	tmpDir string,
	workImage string,
	ki *KernelInfo,
	pw *ProgressWriter,
	layerInputs *[]layerInput,
) error {
	pw.Step("Extracting kernel and initrd...")
	kernelLocal := filepath.Join(tmpDir, "vmlinuz")
	initrdLocal := filepath.Join(tmpDir, "initrd.img")

	if err := extractGuestFile(ctx, workImage, ki.KernelPath, kernelLocal); err != nil {
		return fmt.Errorf("extract kernel: %w", err)
	}
	if err := extractGuestFile(ctx, workImage, ki.InitrdPath, initrdLocal); err != nil {
		return fmt.Errorf("extract initrd: %w", err)
	}

	pw.Step("Building kernel layer...")
	kernelTarPath := filepath.Join(tmpDir, "kernel-layer.tar")
	kernelDigest, kernelSize, err := buildKernelLayerTar(kernelLocal, initrdLocal, kernelTarPath)
	if err != nil {
		return fmt.Errorf("build kernel layer: %w", err)
	}
	pw.Detail("sha256:" + kernelDigest[:12])
	*layerInputs = append(*layerInputs, layerInput{
		MediaType: MediaTypeKernelLayer,
		DigestHex: kernelDigest,
		Size:      kernelSize,
		TarPath:   kernelTarPath,
	})
	return nil
}

func appendRootfsLayerFromImage(
	ctx context.Context,
	tmpDir string,
	workImage string,
	ki *KernelInfo,
	bootFiles []string,
	pw *ProgressWriter,
	layerInputs *[]layerInput,
) error {
	pw.Step("Extracting rootfs...")
	rawTarPath := filepath.Join(tmpDir, "rootfs-raw.tar")
	if err := extractRootfsTar(ctx, workImage, rawTarPath); err != nil {
		return fmt.Errorf("extract rootfs: %w", err)
	}

	excludePaths := bootExcludePaths(ki, bootFiles)
	pw.Step("Building rootfs layer...")
	rootfsTarPath := filepath.Join(tmpDir, "rootfs-layer.tar")
	rootfsDigest, rootfsSize, err := rewriteDeterministicTar(rawTarPath, rootfsTarPath, excludePaths)
	if err != nil {
		return fmt.Errorf("rewrite rootfs tar: %w", err)
	}
	pw.Detail("sha256:" + rootfsDigest[:12])
	*layerInputs = append(*layerInputs, layerInput{
		MediaType: MediaTypeRootfsLayer,
		DigestHex: rootfsDigest,
		Size:      rootfsSize,
		TarPath:   rootfsTarPath,
	})
	return nil
}

func materializeBaseRootfsSnapshot(
	ctx context.Context,
	tmpDir string,
	imagePath string,
	pw *ProgressWriter,
) (string, error) {
	pw.Step("Preparing base rootfs snapshot...")
	rawTarPath := filepath.Join(tmpDir, "rootfs-base-raw.tar")
	if err := extractRootfsTar(ctx, imagePath, rawTarPath); err != nil {
		return "", fmt.Errorf("extract base rootfs: %w", err)
	}

	baseRootfsDir := filepath.Join(tmpDir, "rootfs-base")
	if err := os.MkdirAll(baseRootfsDir, 0o755); err != nil { //nolint:gosec // local build temp workspace
		return "", fmt.Errorf("create base rootfs dir: %w", err)
	}
	if err := utils.ExtractTarToDir(ctx, rawTarPath, baseRootfsDir); err != nil {
		return "", fmt.Errorf("extract base rootfs tar to dir: %w", err)
	}
	return baseRootfsDir, nil
}

func appendDeltaRootfsLayer(
	ctx context.Context,
	tmpDir string,
	workImage string,
	baseRootfsDir string,
	pw *ProgressWriter,
	layerInputs *[]layerInput,
) error {
	pw.Step("Extracting customized rootfs...")
	rawTarPath := filepath.Join(tmpDir, "rootfs-customized-raw.tar")
	if err := extractRootfsTar(ctx, workImage, rawTarPath); err != nil {
		return fmt.Errorf("extract customized rootfs: %w", err)
	}

	modifiedRootfsDir := filepath.Join(tmpDir, "rootfs-customized")
	if err := os.MkdirAll(modifiedRootfsDir, 0o755); err != nil { //nolint:gosec // local build temp workspace
		return fmt.Errorf("create customized rootfs dir: %w", err)
	}
	if err := utils.ExtractTarToDir(ctx, rawTarPath, modifiedRootfsDir); err != nil {
		return fmt.Errorf("extract customized rootfs tar to dir: %w", err)
	}

	pw.Step("Building customization delta layer...")
	deltaTarPath := filepath.Join(tmpDir, "rootfs-delta-layer.tar")
	deltaDigest, deltaSize, changeCount, err := generateDeltaLayerTar(baseRootfsDir, modifiedRootfsDir, deltaTarPath)
	if err != nil {
		return fmt.Errorf("generate customization delta layer: %w", err)
	}
	if changeCount == 0 {
		pw.Detail("no filesystem changes")
		return nil
	}
	*layerInputs = append(*layerInputs, layerInput{
		MediaType: MediaTypeRootfsLayer,
		DigestHex: deltaDigest,
		Size:      deltaSize,
		TarPath:   deltaTarPath,
	})
	pw.Detail("sha256:" + deltaDigest[:12])
	return nil
}

func buildVMConfig(
	cfg *config.CocoonConfig,
	partUUID string,
	cf *Cocoonfile,
	buildCtx *BuildContext,
	useDeltaLayer bool,
) (*VMImageConfig, error) {
	memMB := cfg.DefaultMemoryMB
	if memMB < 0 || memMB > 1<<31-1 {
		return nil, fmt.Errorf("default_memory_mb %d out of range", memMB)
	}

	vmConfig := &VMImageConfig{
		Arch:            runtime.GOARCH,
		DefaultCPUs:     cfg.DefaultCPUs,
		DefaultMemoryMB: int(memMB),
		KernelCmdline:   fmt.Sprintf("console=hvc0 root=PARTUUID=%s ro quiet", partUUID),
		KernelPath:      "/vmlinuz",
		InitrdPath:      "/initrd.img",
		VirtiofsTag:     "cocoon-rootfs",
		RootfsPartUUID:  partUUID,
	}

	labels := map[string]string{}
	if useDeltaLayer {
		maps.Copy(labels, buildCtx.BaseConfig.Labels)
	}
	if cf != nil && len(cf.Labels) > 0 {
		maps.Copy(labels, cf.Labels)
	}
	if len(labels) > 0 {
		vmConfig.Labels = labels
	}
	return vmConfig, nil
}

func createLayoutWorkDir(cfg *config.CocoonConfig) (string, error) {
	// Build temp layouts live under temp/ (not cache/oci/layouts/) so GC phase 3
	// never sweeps an in-progress build directory.
	layoutTempRoot := filepath.Join(cfg.TempDir(), "oci-layout-builds")
	if err := os.MkdirAll(layoutTempRoot, 0o750); err != nil {
		return "", fmt.Errorf("create OCI layout temp root: %w", err)
	}
	layoutWorkDir, err := os.MkdirTemp(layoutTempRoot, "layout-build-*")
	if err != nil {
		return "", fmt.Errorf("create OCI layout temp dir: %w", err)
	}
	return layoutWorkDir, nil
}

func finalizeLayoutDir(store *Store, tag, manifestDigest, layoutWorkDir string) (string, error) {
	layoutDir := store.LayoutDir(tag + "@" + manifestDigest)
	if _, statErr := os.Stat(layoutDir); statErr == nil {
		// Layout already exists (same tag+manifest built previously).
		return layoutDir, nil
	} else if !os.IsNotExist(statErr) {
		return "", fmt.Errorf("stat finalized layout dir %s: %w", layoutDir, statErr)
	}

	if err := os.Rename(layoutWorkDir, layoutDir); err != nil {
		// Another process may have won the race creating the same final layout.
		if _, existsErr := os.Stat(layoutDir); existsErr != nil {
			return "", fmt.Errorf("finalize OCI layout dir: %w", err)
		}
	}
	return layoutDir, nil
}

func registerBlobRefsAndSaveTag(store *Store, cfg *config.CocoonConfig, tag, layoutWorkDir string, info *assemblyInfo) (string, error) {
	layoutDir := ""
	err := store.withTxnLock(func() error {
		finalizedLayoutDir, finalizeErr := finalizeLayoutDir(store, tag, info.manifestDigest, layoutWorkDir)
		if finalizeErr != nil {
			return finalizeErr
		}
		layoutDir = finalizedLayoutDir

		// Register blob refs before saving tag, while sharing the same txn lock
		// as SaveTag/RemoveTag to prevent cross-index races.
		if err := AddBlobRefs(cfg, info.manifestDigest, info.blobDigests, info.blobSizes); err != nil {
			return fmt.Errorf("register blob refs: %w", err)
		}
		if err := store.saveTagTxnLocked(tag, layoutDir, info.manifestDigest); err != nil {
			// Best-effort rollback: only clean refs when this manifest is no longer
			// referenced by any tag.
			if refErr := store.cleanupManifestRefsIfUnreferencedTxnLocked(info.manifestDigest); refErr != nil {
				log.Printf("warning: failed to clean blob refs after SaveTag failure (GC will reclaim): %v", refErr)
			}
			return fmt.Errorf("save tag: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return layoutDir, nil
}

// bootExcludePaths returns paths to exclude from the rootfs layer. It covers
// the selected kernel+initrd and all vmlinuz*, initrd*, initramfs* in /boot.
func bootExcludePaths(ki *KernelInfo, bootFiles []string) []string {
	paths := []string{
		strings.TrimPrefix(ki.KernelPath, "/"),
		strings.TrimPrefix(ki.InitrdPath, "/"),
	}
	for _, bf := range bootFiles {
		base := filepath.Base(bf)
		if strings.HasPrefix(base, "vmlinuz") || strings.HasPrefix(base, "initrd") || strings.HasPrefix(base, "initramfs") {
			p := strings.TrimPrefix(bf, "/")
			if !slices.Contains(paths, p) {
				paths = append(paths, p)
			}
		}
	}
	return paths
}

// applyCocoonfileSteps prepares a writable copy of the base image (if needed)
// and applies Cocoonfile RUN/COPY steps. Returns the work image path.
func applyCocoonfileSteps(ctx context.Context, imagePath, tmpDir string, cf *Cocoonfile, pw *ProgressWriter) (string, error) {
	if cf == nil || len(cf.Steps) == 0 {
		return imagePath, nil
	}
	workCopy := filepath.Join(tmpDir, "work.qcow2")
	if err := copyFile(imagePath, workCopy); err != nil {
		return "", fmt.Errorf("copy image for customization: %w", err)
	}

	// Apply each RUN/COPY step via virt-customize.
	// Resolve COPY source paths relative to the Cocoonfile's directory
	// so that --file /path/to/Cocoonfile works from any working directory.
	for i, step := range cf.Steps {
		resolved := step
		if resolved.Type == "copy" && cf.BaseDir != "" {
			parts := strings.Fields(resolved.Args)
			if len(parts) >= 2 && !filepath.IsAbs(parts[0]) {
				parts[0] = filepath.Join(cf.BaseDir, parts[0])
				resolved.Args = strings.Join(parts, " ")
			}
		}
		pw.Step(customizationStepProgressLabel(resolved.Type, i+1, len(cf.Steps), resolved.Args))
		if err := applyStep(ctx, workCopy, resolved); err != nil {
			return "", fmt.Errorf("Cocoonfile step %d (%s): %w", i+1, resolved.Type, err)
		}
	}
	return workCopy, nil
}

func customizationStepProgressLabel(stepType string, stepIndex, total int, args string) string {
	op := strings.ToUpper(strings.TrimSpace(stepType))
	if op == "" {
		op = "STEP"
	}
	if total < 1 {
		return op
	}
	if stepIndex < 1 {
		stepIndex = 1
	}
	label := fmt.Sprintf("%s [%d/%d]", op, stepIndex, total)
	if args = strings.TrimSpace(args); args != "" {
		label += " " + args
	}
	return label
}

// applyStep executes a single Cocoonfile RUN or COPY step via virt-customize.
//
// COPY semantics: virt-customize --copy-in copies src INTO the dest directory.
// This differs from Dockerfile COPY where dest can be a file path. The dest
// argument must be a directory path in the guest filesystem.
func applyStep(ctx context.Context, imagePath string, step Step) error {
	switch step.Type {
	case "run":
		cmd := exec.CommandContext(ctx, "virt-customize", "-a", imagePath, "--run-command", step.Args) //nolint:gosec // step.Args from parsed Cocoonfile
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("virt-customize --run-command %q: %s: %w", step.Args, strings.TrimSpace(string(out)), err)
		}
	case "copy":
		// COPY args format: "src dest"
		// virt-customize --copy-in expects "src:dest_dir"
		parts := strings.Fields(step.Args)
		if len(parts) < 2 {
			return fmt.Errorf("COPY requires src and dest, got %q", step.Args)
		}
		src := parts[0]
		dest := parts[1]
		copyInArg := src + ":" + dest
		cmd := exec.CommandContext(ctx, "virt-customize", "-a", imagePath, "--copy-in", copyInArg) //nolint:gosec
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("virt-customize --copy-in %q: %s: %w", copyInArg, strings.TrimSpace(string(out)), err)
		}
	default:
		return fmt.Errorf("unknown step type %q", step.Type)
	}
	return nil
}

// listBootFiles runs guestfish to list files in /boot.
func listBootFiles(ctx context.Context, imagePath string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "guestfish", "--ro", "-a", imagePath, "-i", "ls", "/boot") //nolint:gosec // imagePath validated above
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("guestfish ls /boot: %w", err)
	}

	var files []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		files = append(files, "/boot/"+line)
	}
	return files, nil
}

// extractGuestFile copies a single file from the guest image to a local path.
func extractGuestFile(ctx context.Context, imagePath, guestPath, localPath string) error {
	// guestfish copy-out copies to a directory, so we use download instead.
	cmd := exec.CommandContext(ctx, "guestfish", "--ro", "-a", imagePath, "-i", "download", guestPath, localPath) //nolint:gosec // paths validated
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("guestfish download %s: %s: %w", guestPath, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// readPartUUID dynamically detects the root partition and reads its PARTUUID.
// For GPT images, it reads the GPT GUID via guestfish part-get-gpt-guid.
// For MBR images, it reads the 32-bit disk signature from MBR offset 440–443
// and synthesizes a PARTUUID in Linux's native MBR format: {8-hex-sig}-{2-digit-partnum}.
func readPartUUID(ctx context.Context, imagePath, tmpDir string) (string, error) {
	// Use guestfish inspect to find the root partition dynamically.
	// inspect-os returns filesystem devices for detected roots.
	cmd := exec.CommandContext(ctx, "guestfish", "--ro", "-a", imagePath, "-i", "inspect-os") //nolint:gosec
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("guestfish inspect-os: %w", err)
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return "", fmt.Errorf("guestfish inspect-os returned no root device")
	}
	// If multiple roots, take the first line (primary OS).
	if idx := strings.IndexByte(root, '\n'); idx >= 0 {
		log.Printf("guestfish inspect-os returned multiple roots; using first: %s", root[:idx])
		root = root[:idx]
	}

	// Now get the PARTUUID for this root device.
	// Extract partition number from device path (e.g., /dev/sda2 -> 2).
	// The root device from inspect-os is a filesystem device like /dev/sda2.
	// We need the disk device (/dev/sda) and partition number (2).
	disk, partNum, err := parseDevicePartition(root)
	if err != nil {
		return "", fmt.Errorf("parse root device %s: %w", root, err)
	}
	// Validate device path to prevent guestfish script injection.
	if !strings.HasPrefix(disk, "/dev/") {
		return "", fmt.Errorf("unexpected device path %q from inspect-os (expected /dev/...)", disk)
	}

	// Try GPT PARTUUID first.
	cmd2 := exec.CommandContext(ctx, "guestfish", "--ro", "-a", imagePath, "-i", "part-get-gpt-guid", disk, strconv.Itoa(partNum)) //nolint:gosec
	out2, err := cmd2.Output()
	if err == nil {
		uuid := strings.TrimSpace(string(out2))
		if uuid != "" {
			return uuid, nil
		}
	}

	// Fallback for MBR images: read the 32-bit disk signature from MBR bytes
	// 440–443 using qemu-img dd (works with qcow2 and raw), then synthesize a
	// PARTUUID in Linux's native MBR format: {8-hex-sig}-{2-digit-partnum}.
	// This matches the kernel's blkdev_parts_gen_uuid() for MBR partitions,
	// so root=PARTUUID=<sig>-<partnum> will resolve correctly at boot.
	mbrBinPath := filepath.Join(tmpDir, "mbr-sector.bin")
	cmd3 := exec.CommandContext(ctx, "qemu-img", "dd", "if="+imagePath, "of="+mbrBinPath, "bs=512", "count=1") //nolint:gosec
	if out3, ddErr := cmd3.CombinedOutput(); ddErr != nil {
		return "", fmt.Errorf("qemu-img dd (read MBR sector): %s: %w", strings.TrimSpace(string(out3)), ddErr)
	}
	mbrData, err := os.ReadFile(mbrBinPath) //nolint:gosec // G304: path in build tmpDir
	if err != nil {
		return "", fmt.Errorf("read MBR sector file: %w", err)
	}
	if len(mbrData) < 444 {
		return "", fmt.Errorf("MBR sector too short (%d bytes), cannot read disk signature", len(mbrData))
	}
	sig := binary.LittleEndian.Uint32(mbrData[440:444])
	if sig == 0 {
		return "", fmt.Errorf("MBR disk signature is zero for %s (image may lack an MBR disk ID)", disk)
	}
	return fmt.Sprintf("%08x-%02d", sig, partNum), nil
}

// parseDevicePartition splits a device path like "/dev/sda2" into disk "/dev/sda" and partition 2.
// Also handles nvme style like "/dev/nvme0n1p2" -> "/dev/nvme0n1" and 2.
// LVM (/dev/mapper/...) and device-mapper (/dev/dm-*) devices are not supported
// because PARTUUID requires a physical partition.
func parseDevicePartition(device string) (string, int, error) {
	if strings.HasPrefix(device, "/dev/mapper/") || strings.HasPrefix(device, "/dev/dm-") {
		return "", 0, fmt.Errorf("LVM/device-mapper root device %q is not supported: PARTUUID requires a physical partition", device)
	}
	// Find trailing digits.
	i := len(device)
	for i > 0 && device[i-1] >= '0' && device[i-1] <= '9' {
		i--
	}
	if i == len(device) {
		return "", 0, fmt.Errorf("no partition number in device %q", device)
	}
	partStr := device[i:]
	diskPart := device[:i]
	// Strip trailing 'p' for nvme-style devices (e.g., /dev/nvme0n1p2).
	if len(diskPart) > 0 && diskPart[len(diskPart)-1] == 'p' {
		// Only strip if before 'p' there's a digit (nvme pattern).
		if len(diskPart) > 1 && diskPart[len(diskPart)-2] >= '0' && diskPart[len(diskPart)-2] <= '9' {
			diskPart = diskPart[:len(diskPart)-1]
		}
	}
	partNum, err := strconv.Atoi(partStr)
	if err != nil {
		return "", 0, fmt.Errorf("parse partition number %q: %w", partStr, err)
	}
	return diskPart, partNum, nil
}

// extractRootfsTar extracts the root filesystem as a tar archive.
func extractRootfsTar(ctx context.Context, imagePath, tarPath string) error {
	if err := validateSafePath(imagePath); err != nil {
		return err
	}
	if err := validateSafePath(tarPath); err != nil {
		return err
	}

	// Use guestfish -i (inspect mode) which auto-detects and mounts the root filesystem.
	cmd := exec.CommandContext(ctx, "guestfish", "--ro", "-a", imagePath, "-i", "tar-out", "/", tarPath) //nolint:gosec
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("guestfish tar-out: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// assembleOCILayout writes a standard OCI image layout to layoutDir.
// Blobs are stored in the shared BlobStore and hard-linked into the layout.
// Returns an assemblyInfo with the manifest digest and all blob digests/sizes
// for GC reference tracking.
func assembleOCILayout(
	layoutDir string,
	vmConfig *VMImageConfig,
	layers []layerInput,
	baseLayoutPath string,
	blobStore *BlobStore,
) (*assemblyInfo, error) {
	if len(layers) == 0 {
		return nil, fmt.Errorf("assemble OCI layout: no layers provided")
	}

	// 1. Build and store config blob in shared store.
	configJSON, err := json.Marshal(vmConfig)
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	configDigest := sha256Hex(configJSON)
	configSize := int64(len(configJSON))
	if _, err = blobStore.StoreBlobFromBytes(configJSON, configDigest); err != nil {
		return nil, fmt.Errorf("store config blob: %w", err)
	}

	manifestLayers := make([]ociDescriptor, 0, len(layers))
	blobDigests := make([]string, 0, len(layers)+2)
	blobSizes := make([]int64, 0, len(layers)+2)
	blobDigests = append(blobDigests, configDigest)
	blobSizes = append(blobSizes, configSize)

	for _, layer := range layers {
		if layer.MediaType == "" {
			return nil, fmt.Errorf("assemble OCI layout: layer media type is empty")
		}
		if layer.DigestHex == "" {
			return nil, fmt.Errorf("assemble OCI layout: layer digest is empty")
		}
		ensureErr := ensureLayerBlobPresent(blobStore, layer, baseLayoutPath)
		if ensureErr != nil {
			return nil, ensureErr
		}
		manifestLayers = append(manifestLayers, ociDescriptor{
			MediaType: layer.MediaType,
			Digest:    "sha256:" + layer.DigestHex,
			Size:      layer.Size,
		})
		blobDigests = append(blobDigests, layer.DigestHex)
		blobSizes = append(blobSizes, layer.Size)
	}

	// 3. Build manifest.
	manifest := ociManifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.manifest.v1+json",
		ArtifactType:  ArtifactTypeVMImage,
		Config: ociDescriptor{
			MediaType: MediaTypeVMConfig,
			Digest:    "sha256:" + configDigest,
			Size:      configSize,
		},
		Layers: manifestLayers,
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("marshal manifest: %w", err)
	}
	manifestDigest := sha256Hex(manifestJSON)
	manifestSize := int64(len(manifestJSON))
	if _, err = blobStore.StoreBlobFromBytes(manifestJSON, manifestDigest); err != nil {
		return nil, fmt.Errorf("store manifest blob: %w", err)
	}

	// 4. Create layout directory and hardlink all blobs from shared store.
	linkDigests := append(append([]string{}, blobDigests...), manifestDigest)
	for _, digest := range linkDigests {
		if err = blobStore.LinkBlobToLayout(digest, layoutDir); err != nil {
			return nil, fmt.Errorf("link blob %s to layout: %w", digest, err)
		}
	}

	// 5. Write oci-layout file.
	ociLayout := `{"imageLayoutVersion":"1.0.0"}`
	if err = os.WriteFile(filepath.Join(layoutDir, "oci-layout"), []byte(ociLayout+"\n"), 0o644); err != nil { //nolint:gosec // G306
		return nil, fmt.Errorf("write oci-layout: %w", err)
	}

	// 6. Write index.json.
	index := ociIndex{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.index.v1+json",
		Manifests: []ociDescriptor{
			{
				MediaType: "application/vnd.oci.image.manifest.v1+json",
				Digest:    "sha256:" + manifestDigest,
				Size:      manifestSize,
			},
		},
	}
	indexJSON, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal index: %w", err)
	}
	if err = os.WriteFile(filepath.Join(layoutDir, "index.json"), append(indexJSON, '\n'), 0o644); err != nil { //nolint:gosec // G306
		return nil, fmt.Errorf("write index.json: %w", err)
	}

	blobDigests = append(blobDigests, manifestDigest)
	blobSizes = append(blobSizes, manifestSize)
	return &assemblyInfo{
		manifestDigest: manifestDigest,
		blobDigests:    blobDigests,
		blobSizes:      blobSizes,
	}, nil
}

func ensureLayerBlobPresent(blobStore *BlobStore, layer layerInput, baseLayoutPath string) error {
	if layer.TarPath != "" {
		if _, err := blobStore.StoreBlob(layer.TarPath, layer.DigestHex); err != nil {
			return fmt.Errorf("store layer blob %s: %w", layer.DigestHex, err)
		}
		return nil
	}
	if blobStore.BlobExists(layer.DigestHex) {
		return nil
	}
	if strings.TrimSpace(baseLayoutPath) == "" {
		return fmt.Errorf("missing source layout for existing layer %s", layer.DigestHex)
	}
	srcPath := filepath.Join(baseLayoutPath, "blobs", "sha256", layer.DigestHex)
	if _, err := os.Stat(srcPath); err != nil {
		return fmt.Errorf("source layer blob %s missing from base layout: %w", layer.DigestHex, err)
	}
	if _, err := blobStore.StoreBlob(srcPath, layer.DigestHex); err != nil {
		return fmt.Errorf("import layer blob %s from base layout: %w", layer.DigestHex, err)
	}
	return nil
}

// validateSafePath rejects control characters that can break command invocation.
// Spaces and other normal filesystem characters are allowed.
func validateSafePath(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("path cannot be empty")
	}
	if strings.ContainsRune(path, '\x00') {
		return fmt.Errorf("path contains NUL byte")
	}
	if strings.ContainsAny(path, "\r\n") {
		return fmt.Errorf("path contains newline characters")
	}
	return nil
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func digestHex(digest string) (string, error) {
	const prefix = "sha256:"
	if !strings.HasPrefix(digest, prefix) {
		return "", fmt.Errorf("unsupported digest format %q", digest)
	}
	hexDigest := strings.TrimPrefix(digest, prefix)
	if len(hexDigest) != 64 {
		return "", fmt.Errorf("invalid digest length for %q", digest)
	}
	for _, c := range hexDigest {
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return "", fmt.Errorf("invalid digest characters for %q", digest)
		}
	}
	return strings.ToLower(hexDigest), nil
}

// isValidUUID checks that s matches UUID format: xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx.
func isValidUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				return false
			}
		}
	}
	return true
}

// isValidMBRPartUUID checks that s matches the Linux native MBR PARTUUID format:
// {8 hex chars}-{2 digit partition number} (e.g., "a1b2c3d4-02").
func isValidMBRPartUUID(s string) bool {
	// Format: xxxxxxxx-dd (11 characters total).
	if len(s) != 11 || s[8] != '-' {
		return false
	}
	for _, c := range s[:8] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	for _, c := range s[9:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func copyFile(src, dst string) error {
	sf, err := os.Open(src) //nolint:gosec // G304: src is a build pipeline temp file
	if err != nil {
		return err
	}
	defer sf.Close() //nolint:errcheck

	df, err := os.Create(dst) //nolint:gosec // G304: dst is a build pipeline output path
	if err != nil {
		return err
	}

	if _, err = io.Copy(df, sf); err != nil {
		df.Close() //nolint:errcheck,gosec // G104: best-effort close on error path
		return err
	}
	return df.Close()
}

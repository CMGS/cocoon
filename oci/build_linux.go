//go:build linux

package oci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/CMGS/cocoon/config"
)

// assemblyInfo holds the results of assembleOCILayout for blob ref tracking.
type assemblyInfo struct {
	manifestDigest string
	blobDigests    []string
	blobSizes      []int64
}

// Build extracts kernel, initrd, and rootfs from a cloud image, packages them
// as deterministic tar layers, and stores the result as an OCI image layout.
//
// When cf is non-nil, Cocoonfile customization steps (RUN/COPY) are applied
// to the image via virt-customize before rootfs extraction.
//
// See docs/04.1-oci-vm-images.md Section 3 for the full build pipeline.
func Build(ctx context.Context, cfg *config.CocoonConfig, imagePath, tag string, cf *Cocoonfile) (*BuildResult, error) {
	// Validate input path.
	if err := validateSafePath(imagePath); err != nil {
		return nil, fmt.Errorf("invalid image path: %w", err)
	}
	if _, err := os.Stat(imagePath); err != nil {
		return nil, fmt.Errorf("image file not found: %w", err)
	}

	// Check guestfish is available.
	if _, err := exec.LookPath("guestfish"); err != nil {
		return nil, fmt.Errorf("guestfish not found in PATH: OCI VM image build requires libguestfs: %w", err)
	}

	// If Cocoonfile has customization steps, virt-customize is required.
	if cf != nil && len(cf.Steps) > 0 {
		if _, err := exec.LookPath("virt-customize"); err != nil {
			return nil, fmt.Errorf("virt-customize not found in PATH: Cocoonfile RUN/COPY requires libguestfs-tools: %w", err)
		}
	}

	// Calculate total build steps.
	// Base steps: detect kernel, extract kernel+initrd, build kernel layer,
	// read PARTUUID, extract rootfs, build rootfs layer, assemble OCI layout, save tag.
	totalSteps := 8
	if cf != nil && len(cf.Steps) > 0 {
		totalSteps += len(cf.Steps)
	}
	pw := NewProgressWriter(os.Stderr, totalSteps)

	tmpDir, err := os.MkdirTemp(cfg.TempDir(), "ocivm-build-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir) //nolint:errcheck // best-effort cleanup

	// If Cocoonfile has customization steps, work on a writable copy.
	workImage := imagePath
	if cf != nil && len(cf.Steps) > 0 {
		workCopy := filepath.Join(tmpDir, "work.qcow2")
		if err = copyFile(imagePath, workCopy); err != nil {
			return nil, fmt.Errorf("copy image for customization: %w", err)
		}
		workImage = workCopy

		// Apply each RUN/COPY step via virt-customize.
		for i, step := range cf.Steps {
			pw.Step(fmt.Sprintf("%s %s", strings.ToUpper(step.Type), step.Args))
			if err = applyStep(ctx, workImage, step); err != nil {
				return nil, fmt.Errorf("Cocoonfile step %d (%s): %w", i+1, step.Type, err)
			}
		}
	}

	// Step 1-3: List /boot and detect kernel.
	pw.Step("Detecting kernel...")
	bootFiles, err := listBootFiles(ctx, workImage)
	if err != nil {
		return nil, fmt.Errorf("list boot files: %w", err)
	}

	ki, err := DetectKernel(bootFiles)
	if err != nil {
		return nil, fmt.Errorf("detect kernel: %w", err)
	}
	pw.Detail(ki.Version)

	// Step 4: Extract kernel and initrd.
	pw.Step("Extracting kernel and initrd...")
	kernelLocal := filepath.Join(tmpDir, "vmlinuz")
	initrdLocal := filepath.Join(tmpDir, "initrd.img")

	if err = extractGuestFile(ctx, workImage, ki.KernelPath, kernelLocal); err != nil {
		return nil, fmt.Errorf("extract kernel: %w", err)
	}
	if err = extractGuestFile(ctx, workImage, ki.InitrdPath, initrdLocal); err != nil {
		return nil, fmt.Errorf("extract initrd: %w", err)
	}

	// Build kernel layer tar.
	pw.Step("Building kernel layer...")
	kernelTarPath := filepath.Join(tmpDir, "kernel-layer.tar")
	kernelDigest, kernelSize, err := buildKernelLayerTar(kernelLocal, initrdLocal, kernelTarPath)
	if err != nil {
		return nil, fmt.Errorf("build kernel layer: %w", err)
	}
	pw.Detail("sha256:" + kernelDigest[:12])

	// Step 5: Read PARTUUID.
	pw.Step("Reading PARTUUID...")
	partUUID, err := readPartUUID(ctx, workImage)
	if err != nil {
		return nil, fmt.Errorf("read PARTUUID: %w", err)
	}
	if !isValidUUID(partUUID) {
		return nil, fmt.Errorf("invalid PARTUUID format %q (expected UUID)", partUUID)
	}
	pw.Detail(partUUID)

	// Step 6: Extract rootfs as tar, rewrite deterministically.
	pw.Step("Extracting rootfs...")
	rawTarPath := filepath.Join(tmpDir, "rootfs-raw.tar")
	if err = extractRootfsTar(ctx, workImage, rawTarPath); err != nil {
		return nil, fmt.Errorf("extract rootfs: %w", err)
	}

	// Exclude kernel and initrd from rootfs (they're in the kernel layer).
	excludePaths := []string{
		strings.TrimPrefix(ki.KernelPath, "/"),
		strings.TrimPrefix(ki.InitrdPath, "/"),
	}
	pw.Step("Building rootfs layer...")
	rootfsTarPath := filepath.Join(tmpDir, "rootfs-layer.tar")
	rootfsDigest, rootfsSize, err := rewriteDeterministicTar(rawTarPath, rootfsTarPath, excludePaths)
	if err != nil {
		return nil, fmt.Errorf("rewrite rootfs tar: %w", err)
	}
	pw.Detail("sha256:" + rootfsDigest[:12])

	// Step 7-8: Build config blob and assemble OCI layout.
	arch := runtime.GOARCH
	memMB := cfg.DefaultMemoryMB
	if memMB < 0 || memMB > 1<<31-1 {
		return nil, fmt.Errorf("default_memory_mb %d out of range", memMB)
	}
	vmConfig := &VMImageConfig{
		Arch:            arch,
		DefaultCPUs:     cfg.DefaultCPUs,
		DefaultMemoryMB: int(memMB),
		KernelCmdline: fmt.Sprintf(
			"console=hvc0 root=PARTUUID=%s ro quiet",
			partUUID,
		),
		KernelPath:     "/vmlinuz",
		InitrdPath:     "/initrd.img",
		VirtiofsTag:    "cocoon-rootfs",
		RootfsPartUUID: partUUID,
	}

	// Merge Cocoonfile labels into config if present.
	if cf != nil && len(cf.Labels) > 0 {
		vmConfig.Labels = cf.Labels
	}

	blobStore := NewBlobStore(cfg)
	store := NewStore(cfg)
	layoutDir := store.LayoutDir(tag)

	pw.Step("Assembling OCI layout...")
	info, err := assembleOCILayout(layoutDir, vmConfig, kernelTarPath, kernelDigest, kernelSize, rootfsTarPath, rootfsDigest, rootfsSize, blobStore)
	if err != nil {
		os.RemoveAll(layoutDir) //nolint:errcheck,gosec // best-effort cleanup of partial layout
		return nil, fmt.Errorf("assemble OCI layout: %w", err)
	}
	pw.Detail("sha256:" + info.manifestDigest[:12])

	// Save tag in local index.
	pw.Step("Saving tag...")
	if err := store.SaveTag(tag, layoutDir, info.manifestDigest); err != nil {
		return nil, fmt.Errorf("save tag: %w", err)
	}
	pw.Detail(tag)

	// Register blob references for GC tracking.
	if err := AddBlobRefs(cfg, info.manifestDigest, info.blobDigests, info.blobSizes); err != nil {
		return nil, fmt.Errorf("register blob refs: %w", err)
	}

	return &BuildResult{
		Tag:            tag,
		KernelVersion:  ki.Version,
		ManifestDigest: info.manifestDigest,
		LayoutPath:     layoutDir,
	}, nil
}

// applyStep executes a single Cocoonfile RUN or COPY step via virt-customize.
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

// readPartUUID dynamically detects the root partition and reads its GPT PARTUUID.
// It uses guestfish inspect-os to find the root device, then extracts the disk
// and partition number to query the PARTUUID via part-get-gpt-guid.
func readPartUUID(ctx context.Context, imagePath string) (string, error) {
	// Use guestfish inspect to find the root partition dynamically.
	// inspect-get-mountpoints returns mountpoints mapped to devices.
	script := fmt.Sprintf(`add %s
run
inspect-os
`, imagePath)
	cmd := exec.CommandContext(ctx, "guestfish") //nolint:gosec
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("guestfish inspect-os: %w", err)
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return "", fmt.Errorf("guestfish inspect-os returned no root device")
	}
	// If multiple roots, take the first line.
	if idx := strings.IndexByte(root, '\n'); idx >= 0 {
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

	script2 := fmt.Sprintf(`add %s
run
part-get-gpt-guid %s %d
`, imagePath, disk, partNum)
	cmd2 := exec.CommandContext(ctx, "guestfish") //nolint:gosec
	cmd2.Stdin = strings.NewReader(script2)
	out2, err := cmd2.Output()
	if err != nil {
		return "", fmt.Errorf("guestfish part-get-gpt-guid %s %d: %w", disk, partNum, err)
	}
	uuid := strings.TrimSpace(string(out2))
	if uuid == "" {
		return "", fmt.Errorf("empty PARTUUID for %s partition %d", disk, partNum)
	}
	return uuid, nil
}

// parseDevicePartition splits a device path like "/dev/sda2" into disk "/dev/sda" and partition 2.
// Also handles nvme style like "/dev/nvme0n1p2" -> "/dev/nvme0n1" and 2.
func parseDevicePartition(device string) (string, int, error) {
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
	script := fmt.Sprintf(`tar-out / %s
`, tarPath)
	cmd := exec.CommandContext(ctx, "guestfish", "--ro", "-a", imagePath, "-i") //nolint:gosec
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("guestfish tar-out: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// assembleOCILayout writes a standard OCI image layout to layoutDir.
// Blobs are stored in the shared BlobStore and hardlinked into the layout.
// Returns an assemblyInfo with the manifest digest and all blob digests/sizes
// for GC reference tracking.
func assembleOCILayout(
	layoutDir string,
	vmConfig *VMImageConfig,
	kernelTarPath, kernelDigest string, kernelSize int64,
	rootfsTarPath, rootfsDigest string, rootfsSize int64,
	blobStore *BlobStore,
) (*assemblyInfo, error) {
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

	// 2. Store layer tars in shared store.
	if _, err = blobStore.StoreBlob(kernelTarPath, kernelDigest); err != nil {
		return nil, fmt.Errorf("store kernel blob: %w", err)
	}
	if _, err = blobStore.StoreBlob(rootfsTarPath, rootfsDigest); err != nil {
		return nil, fmt.Errorf("store rootfs blob: %w", err)
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
		Layers: []ociDescriptor{
			{
				MediaType: MediaTypeKernelLayer,
				Digest:    "sha256:" + kernelDigest,
				Size:      kernelSize,
			},
			{
				MediaType: MediaTypeRootfsLayer,
				Digest:    "sha256:" + rootfsDigest,
				Size:      rootfsSize,
			},
		},
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
	for _, digest := range []string{configDigest, kernelDigest, rootfsDigest, manifestDigest} {
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

	return &assemblyInfo{
		manifestDigest: manifestDigest,
		blobDigests:    []string{configDigest, kernelDigest, rootfsDigest, manifestDigest},
		blobSizes:      []int64{configSize, kernelSize, rootfsSize, manifestSize},
	}, nil
}

// validateSafePath checks that a path contains only safe characters for embedding
// in guestfish scripts. This prevents shell injection via paths passed to guestfish
// commands like 'add', 'tar-out', and 'part-get-gpt-guid'.
// Used for both user-provided image paths and internal temp paths.
func validateSafePath(path string) error {
	if strings.Contains(path, "..") {
		return fmt.Errorf("path traversal not allowed in %q", path)
	}
	for _, c := range path {
		isAlpha := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		isDigit := c >= '0' && c <= '9'
		isSafe := c == '/' || c == '-' || c == '_' || c == '.' || c == '+' || c == '~' || c == '@' || c == ':' || c == '%' || c == ','
		if !isAlpha && !isDigit && !isSafe {
			return fmt.Errorf("unsafe character %q in path %q", string(c), path)
		}
	}
	return nil
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
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

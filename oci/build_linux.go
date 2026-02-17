//go:build linux

package oci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/CMGS/cocoon/config"
)

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

	tmpDir, err := os.MkdirTemp(cfg.TempDir(), "ocivm-build-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir) //nolint:errcheck // best-effort cleanup

	// If Cocoonfile has customization steps, work on a writable copy.
	workImage := imagePath
	if cf != nil && len(cf.Steps) > 0 {
		workCopy := filepath.Join(tmpDir, "work.qcow2")
		if err := copyFile(imagePath, workCopy); err != nil {
			return nil, fmt.Errorf("copy image for customization: %w", err)
		}
		workImage = workCopy

		// Apply each RUN/COPY step via virt-customize.
		for i, step := range cf.Steps {
			if err := applyStep(ctx, workImage, step); err != nil {
				return nil, fmt.Errorf("Cocoonfile step %d (%s): %w", i+1, step.Type, err)
			}
		}
	}

	// Step 1-3: List /boot and detect kernel.
	bootFiles, err := listBootFiles(ctx, workImage)
	if err != nil {
		return nil, fmt.Errorf("list boot files: %w", err)
	}

	ki, err := DetectKernel(bootFiles)
	if err != nil {
		return nil, fmt.Errorf("detect kernel: %w", err)
	}

	// Step 4: Extract kernel and initrd.
	kernelLocal := filepath.Join(tmpDir, "vmlinuz")
	initrdLocal := filepath.Join(tmpDir, "initrd.img")

	if err := extractGuestFile(ctx, workImage, ki.KernelPath, kernelLocal); err != nil {
		return nil, fmt.Errorf("extract kernel: %w", err)
	}
	if err := extractGuestFile(ctx, workImage, ki.InitrdPath, initrdLocal); err != nil {
		return nil, fmt.Errorf("extract initrd: %w", err)
	}

	// Build kernel layer tar.
	kernelTarPath := filepath.Join(tmpDir, "kernel-layer.tar")
	kernelDigest, kernelSize, err := buildKernelLayerTar(kernelLocal, initrdLocal, kernelTarPath)
	if err != nil {
		return nil, fmt.Errorf("build kernel layer: %w", err)
	}

	// Step 5: Read PARTUUID.
	partUUID, err := readPartUUID(ctx, workImage)
	if err != nil {
		return nil, fmt.Errorf("read PARTUUID: %w", err)
	}

	// Step 6: Extract rootfs as tar, rewrite deterministically.
	rawTarPath := filepath.Join(tmpDir, "rootfs-raw.tar")
	if err := extractRootfsTar(ctx, workImage, rawTarPath); err != nil {
		return nil, fmt.Errorf("extract rootfs: %w", err)
	}

	// Exclude kernel and initrd from rootfs (they're in the kernel layer).
	excludePaths := []string{
		strings.TrimPrefix(ki.KernelPath, "/"),
		strings.TrimPrefix(ki.InitrdPath, "/"),
	}
	rootfsTarPath := filepath.Join(tmpDir, "rootfs-layer.tar")
	rootfsDigest, rootfsSize, err := rewriteDeterministicTar(rawTarPath, rootfsTarPath, excludePaths)
	if err != nil {
		return nil, fmt.Errorf("rewrite rootfs tar: %w", err)
	}

	// Step 7-8: Build config blob and assemble OCI layout.
	arch := runtime.GOARCH
	vmConfig := &VMImageConfig{
		Arch:            arch,
		DefaultCPUs:     cfg.DefaultCPUs,
		DefaultMemoryMB: int(cfg.DefaultMemoryMB),
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

	store := NewStore(cfg)
	layoutDir := store.LayoutDir(tag)

	manifestDigest, err := assembleOCILayout(layoutDir, vmConfig, kernelTarPath, kernelDigest, kernelSize, rootfsTarPath, rootfsDigest, rootfsSize)
	if err != nil {
		return nil, fmt.Errorf("assemble OCI layout: %w", err)
	}

	// Save tag in local index.
	if err := store.SaveTag(tag, layoutDir, manifestDigest); err != nil {
		return nil, fmt.Errorf("save tag: %w", err)
	}

	return &BuildResult{
		Tag:            tag,
		KernelVersion:  ki.Version,
		ManifestDigest: manifestDigest,
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
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
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

// readPartUUID reads the GPT PARTUUID of partition 2 from the image.
func readPartUUID(ctx context.Context, imagePath string) (string, error) {
	// Use guestfish script via stdin since part-get-gpt-guid requires run first.
	script := fmt.Sprintf(`add %s
run
part-get-gpt-guid /dev/sda 2
`, imagePath)

	cmd := exec.CommandContext(ctx, "guestfish") //nolint:gosec
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("guestfish part-get-gpt-guid: %w", err)
	}

	uuid := strings.TrimSpace(string(out))
	if uuid == "" {
		return "", fmt.Errorf("empty PARTUUID from guestfish (is the image GPT-partitioned?)")
	}
	return uuid, nil
}

// extractRootfsTar extracts the root filesystem as a tar archive.
func extractRootfsTar(ctx context.Context, imagePath, tarPath string) error {
	if err := validateSafePath(imagePath); err != nil {
		return err
	}
	if err := validateSafePath(tarPath); err != nil {
		return err
	}

	script := fmt.Sprintf(`add %s
run
mount-ro /dev/sda2 /
tar-out / %s
`, imagePath, tarPath)

	cmd := exec.CommandContext(ctx, "guestfish") //nolint:gosec
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("guestfish tar-out: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// assembleOCILayout writes a standard OCI image layout to layoutDir.
// Returns the manifest digest (sha256 hex).
func assembleOCILayout(
	layoutDir string,
	vmConfig *VMImageConfig,
	kernelTarPath, kernelDigest string, kernelSize int64,
	rootfsTarPath, rootfsDigest string, rootfsSize int64,
) (string, error) {
	blobsDir := filepath.Join(layoutDir, "blobs", "sha256")
	if err := os.MkdirAll(blobsDir, 0o755); err != nil { //nolint:gosec // G301
		return "", fmt.Errorf("create blobs dir: %w", err)
	}

	// 1. Write config blob.
	configJSON, err := json.Marshal(vmConfig)
	if err != nil {
		return "", fmt.Errorf("marshal config: %w", err)
	}
	configDigest := sha256Hex(configJSON)
	configSize := int64(len(configJSON))
	if err := os.WriteFile(filepath.Join(blobsDir, configDigest), configJSON, 0o644); err != nil { //nolint:gosec // G306
		return "", fmt.Errorf("write config blob: %w", err)
	}

	// 2. Copy layer tars into blobs dir.
	if err := copyFile(kernelTarPath, filepath.Join(blobsDir, kernelDigest)); err != nil {
		return "", fmt.Errorf("copy kernel layer: %w", err)
	}
	if err := copyFile(rootfsTarPath, filepath.Join(blobsDir, rootfsDigest)); err != nil {
		return "", fmt.Errorf("copy rootfs layer: %w", err)
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
		return "", fmt.Errorf("marshal manifest: %w", err)
	}
	manifestDigest := sha256Hex(manifestJSON)
	manifestSize := int64(len(manifestJSON))
	if err := os.WriteFile(filepath.Join(blobsDir, manifestDigest), manifestJSON, 0o644); err != nil { //nolint:gosec // G306
		return "", fmt.Errorf("write manifest blob: %w", err)
	}

	// 4. Write oci-layout file.
	ociLayout := `{"imageLayoutVersion":"1.0.0"}`
	if err := os.WriteFile(filepath.Join(layoutDir, "oci-layout"), []byte(ociLayout+"\n"), 0o644); err != nil { //nolint:gosec // G306
		return "", fmt.Errorf("write oci-layout: %w", err)
	}

	// 5. Write index.json.
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
		return "", fmt.Errorf("marshal index: %w", err)
	}
	if err := os.WriteFile(filepath.Join(layoutDir, "index.json"), append(indexJSON, '\n'), 0o644); err != nil { //nolint:gosec // G306
		return "", fmt.Errorf("write index.json: %w", err)
	}

	return manifestDigest, nil
}

// validateSafePath checks that a path contains only safe characters for guestfish.
func validateSafePath(path string) error {
	for _, c := range path {
		isAlpha := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		isDigit := c >= '0' && c <= '9'
		isSafe := c == '/' || c == '-' || c == '_' || c == '.'
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

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src) //nolint:gosec // G304: src is a temp file from the build
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644) //nolint:gosec // G306
}

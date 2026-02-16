package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/image"
	"github.com/CMGS/cocoon/image/refcache"
	"github.com/CMGS/cocoon/lock/flock"
	"github.com/CMGS/cocoon/storage"
	"github.com/CMGS/cocoon/types"
)

// Compile-time interface check.
var _ image.Manager = (*manager)(nil)

// manager is the default implementation of the image.Manager interface.
// It coordinates image pulling, conversion, caching, and bootability
// verification using external tools (Buildah, qemu-img, libguestfs).
type manager struct {
	cfg    *config.CocoonConfig
	refCtr storage.ReferenceCounter
}

// New creates a new image.Manager backed by the given configuration.
// The configuration determines cache directories, lock paths, and tool locations.
// The ReferenceCounter is used to populate reference counts when listing cached images.
func New(cfg *config.CocoonConfig, refCtr storage.ReferenceCounter) image.Manager {
	return &manager{cfg: cfg, refCtr: refCtr}
}

// Pull downloads an OCI image or cloud image based on the reference type.
//
// Reference classification:
//   - OCI: contains "/" and no path separator at start (e.g., "docker.io/library/ubuntu:22.04")
//   - URL: starts with "http://" or "https://"
//   - Local file: starts with "/" or "./" or contains no "/" (existing file path)
func (m *manager) Pull(ctx context.Context, ref string) (*image.ImageIdentity, error) {
	imgType := classifyRef(ref)

	switch imgType {
	case image.ImageTypeOCI:
		return m.pullOCI(ctx, ref)
	case image.ImageTypeURL:
		return m.pullURL(ctx, ref)
	case image.ImageTypeLocalFile:
		return m.pullLocal(ctx, ref)
	default:
		return nil, fmt.Errorf("unsupported image reference type: %q", ref)
	}
}

// Convert transforms a pulled image into a qcow2 base image in the cache.
// It acquires a per-image conversion lock (Level 3) to prevent duplicate work
// when multiple callers request the same image concurrently.
//
// If the base image already exists in the cache (checked after acquiring the
// lock), conversion is skipped.
//
// The source image path is taken from identity.TempPath. The source format is
// auto-detected via qemu-img info. If the source is already qcow2, it is
// atomically copied to cache. If raw, it is converted with qemu-img convert.
func (m *manager) Convert(ctx context.Context, identity *image.ImageIdentity) (string, error) {
	baseKey := identity.BaseKey()
	basePath := m.cfg.BaseImagePath(baseKey)

	// Acquire per-image conversion lock (Level 3 in lock hierarchy).
	lockPath := m.cfg.ConversionLockPath(baseKey)
	fl := flock.New(lockPath)
	if err := fl.Lock(); err != nil {
		return "", fmt.Errorf("acquire conversion lock for %s: %w", baseKey, err)
	}
	defer func() {
		if err := fl.Unlock(); err != nil {
			log.Printf("warning: failed to release conversion lock for %s: %v", baseKey, err)
		}
	}()

	// Check cache after acquiring lock (another process may have completed
	// conversion while we waited).
	if _, err := os.Stat(basePath); err == nil {
		log.Printf("image %s: cache hit after lock acquisition, skipping conversion", baseKey)
		return basePath, nil
	}

	srcPath := identity.TempPath
	if srcPath == "" {
		return "", fmt.Errorf("convert %s: no source path (TempPath) set on identity", baseKey)
	}

	if _, err := os.Stat(srcPath); err != nil {
		return "", fmt.Errorf("convert %s: source file not found at %s: %w", baseKey, srcPath, err)
	}

	// Handle OCI image type: rootfs mount -> qcow2 conversion via guestfish.
	if identity.ImageType == image.ImageTypeOCI {
		if err := m.convertOCIImage(ctx, identity, basePath, baseKey); err != nil {
			return "", err
		}
		return basePath, nil
	}

	// Detect source image format.
	format, err := detectImageFormat(ctx, srcPath)
	if err != nil {
		return "", fmt.Errorf("convert %s: detect format: %w", baseKey, err)
	}
	log.Printf("image %s: detected source format: %s", baseKey, format)

	// Ensure cache directory exists.
	cacheDir := m.cfg.ImageCacheDir()
	if err := os.MkdirAll(cacheDir, 0o750); err != nil {
		return "", fmt.Errorf("convert %s: create cache dir: %w", baseKey, err)
	}

	// Use a temp file in the cache directory for atomic placement.
	tmpPath := basePath + ".tmp"
	defer func() { _ = os.Remove(tmpPath) }() // clean up on any error path

	switch format {
	case "qcow2":
		// Already qcow2: atomic copy (temp + fsync + rename).
		log.Printf("image %s: source is qcow2, performing atomic copy to cache", baseKey)
		if err := atomicCopyFile(srcPath, tmpPath); err != nil {
			return "", types.NewPermanentError(fmt.Errorf("convert %s: copy qcow2 to cache: %w", baseKey, err))
		}

	case "raw":
		// Convert raw to qcow2 using qemu-img.
		log.Printf("image %s: converting raw -> qcow2", baseKey)
		cmd := exec.CommandContext(ctx, "qemu-img", "convert", "-f", "raw", "-O", "qcow2", srcPath, tmpPath) //nolint:gosec // args are controlled internal paths, not user input
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", types.NewPermanentError(fmt.Errorf("convert %s: qemu-img convert: %s: %w", baseKey, string(out), err))
		}

	default:
		return "", types.NewPermanentError(fmt.Errorf("convert %s: unsupported source format %q (expected qcow2 or raw)", baseKey, format))
	}

	// Atomic rename into cache, then mark read-only so VMs only write to
	// their COW overlays and the shared base image stays immutable.
	if err := os.Rename(tmpPath, basePath); err != nil {
		return "", fmt.Errorf("convert %s: rename to cache path: %w", baseKey, err)
	}
	if err := os.Chmod(basePath, 0o444); err != nil {
		return "", fmt.Errorf("convert %s: chmod read-only: %w", baseKey, err)
	}

	// Clean up temp source if it was downloaded (not a user's local file).
	if identity.ImageType == image.ImageTypeURL {
		if err := os.Remove(srcPath); err != nil {
			log.Printf("warning: failed to clean up temp source %s: %v", srcPath, err)
		}
	}

	log.Printf("image %s: conversion complete -> %s", baseKey, basePath)
	return basePath, nil
}

// convertOCIImage handles the OCI-specific conversion path within Convert().
// It creates a temporary qcow2 from the OCI rootfs and atomically moves it
// into the cache at basePath. On failure, any associated buildah container is
// cleaned up.
func (m *manager) convertOCIImage(ctx context.Context, identity *image.ImageIdentity, basePath, baseKey string) error {
	log.Printf("image %s: converting OCI rootfs -> qcow2", baseKey)

	// Ensure cache directory exists.
	cacheDir := m.cfg.ImageCacheDir()
	if err := os.MkdirAll(cacheDir, 0o750); err != nil {
		return fmt.Errorf("convert %s: create cache dir: %w", baseKey, err)
	}

	tmpPath := basePath + ".tmp"
	defer func() { _ = os.Remove(tmpPath) }()

	diskSize := "10G" // Default size for OCI conversion
	if err := convertOCI(ctx, identity.TempPath, tmpPath, diskSize); err != nil {
		// Clean up buildah container on conversion failure.
		if identity.ContainerID != "" {
			cleanupBuildahContainer(identity.ContainerID, m.cfg)
		}
		return types.NewPermanentError(fmt.Errorf("convert OCI %s: %w", baseKey, err))
	}

	// Atomic rename into cache.
	if err := os.Rename(tmpPath, basePath); err != nil {
		return fmt.Errorf("convert %s: rename to cache: %w", baseKey, err)
	}

	// Cleanup buildah container.
	if identity.ContainerID != "" {
		cleanupBuildahContainer(identity.ContainerID, m.cfg)
	}

	log.Printf("image %s: OCI conversion complete -> %s", baseKey, basePath)
	return nil
}

// Prepare is the combined pull+convert+cache pipeline. It first checks whether
// a cached base image already exists for the given ref. If so, it skips pull
// and convert entirely. Otherwise it runs Pull + Convert and caches the result.
//
// For OCI images, the expensive pull+mount (buildah) steps are performed inside
// the per-image conversion lock so that concurrent creates for the same image
// result in only one pull. See docs/06-concurrency.md Section 1 for details.
func (m *manager) Prepare(ctx context.Context, ref string) (*image.ImageIdentity, string, error) {
	// Fast path: check refcache first for any ref format (handles short names, aliases, etc.).
	// This prevents short names like "ubuntu-22.04-cloudimg" from being misclassified as OCI
	// refs and sent into the buildah/skopeo pipeline when they already exist in the cache.
	if baseKey, found, _ := refcache.ResolveBaseKey(m.cfg, ref); found {
		basePath := m.cfg.BaseImagePath(baseKey)
		if _, statErr := os.Stat(basePath); statErr == nil {
			log.Printf("image %s: cache hit for %q, skipping pull", baseKey, ref)
			checksum, arch := parseBaseKey(baseKey)
			// Retrieve FullDigest from refcache entry via RefsForBaseKey.
			var fullDigest string
			if _, digest, err := refcache.RefsForBaseKey(m.cfg, baseKey); err == nil {
				fullDigest = digest
			}
			return &image.ImageIdentity{
				Checksum:   checksum,
				Arch:       arch,
				FullDigest: fullDigest,
				SourceRef:  ref,
				ImageType:  classifyRef(ref),
			}, basePath, nil
		}
	}

	imgType := classifyRef(ref)

	// OCI images use a special two-phase flow: identify outside lock,
	// pull+mount+convert inside lock.
	if imgType == image.ImageTypeOCI {
		return m.prepareOCI(ctx, ref)
	}

	// Cache miss: Pull determines identity and provides the source file path.
	// Convert acquires its own lock.
	identity, err := m.Pull(ctx, ref)
	if err != nil {
		return nil, "", fmt.Errorf("pull %q: %w", ref, err)
	}

	baseKey := identity.BaseKey()
	basePath := m.cfg.BaseImagePath(baseKey)

	// Fast path: check cache before converting.
	if _, statErr := os.Stat(basePath); statErr == nil {
		log.Printf("image %s: cache hit for %q, skipping conversion", baseKey, ref)
		return identity, basePath, nil
	}

	// Convert (includes its own per-image lock).
	basePath, err = m.Convert(ctx, identity)
	if err != nil {
		return nil, "", fmt.Errorf("convert %q (key=%s): %w", ref, baseKey, err)
	}

	return identity, basePath, nil
}

// prepareOCI implements the OCI-specific Prepare pipeline where the pull+mount
// happens inside the conversion lock to prevent parallel pulls of the same image.
//
// Flow:
//  1. Identify (skopeo inspect) — cheap, outside lock
//  2. Fast-path cache check — outside lock
//  3. Acquire per-image conversion lock (Level 3)
//  4. Double-check cache — inside lock
//  5. Pull + mount + convert — inside lock
//  6. Release lock
func (m *manager) prepareOCI(ctx context.Context, ref string) (*image.ImageIdentity, string, error) {
	// Phase 1: Identify — skopeo inspect only, no lock needed.
	identity, err := identifyOCIPlatform(ctx, ref)
	if err != nil {
		return nil, "", fmt.Errorf("identify OCI %q: %w", ref, err)
	}

	baseKey := identity.BaseKey()
	basePath := m.cfg.BaseImagePath(baseKey)

	// Phase 2: Fast-path cache check (no lock).
	if _, statErr := os.Stat(basePath); statErr == nil {
		log.Printf("image %s: cache hit for %q, skipping pull and conversion", baseKey, ref)
		return identity, basePath, nil
	}

	// Phase 3: Acquire per-image conversion lock (Level 3 in lock hierarchy).
	lockPath := m.cfg.ConversionLockPath(baseKey)
	fl := flock.New(lockPath)
	if err := fl.Lock(); err != nil {
		return nil, "", fmt.Errorf("acquire conversion lock for %s: %w", baseKey, err)
	}
	defer func() {
		if err := fl.Unlock(); err != nil {
			log.Printf("warning: failed to release conversion lock for %s: %v", baseKey, err)
		}
	}()

	// Phase 4: Double-check cache after acquiring lock (another process may
	// have completed the pull+convert while we waited).
	if _, statErr := os.Stat(basePath); statErr == nil {
		log.Printf("image %s: cache hit after lock acquisition for %q, skipping pull and conversion", baseKey, ref)
		return identity, basePath, nil
	}

	// Phase 5: Pull + mount (buildah) — inside lock.
	log.Printf("image %s: pulling OCI image %q", baseKey, ref)
	if err := pullAndMountOCIPlatform(ctx, m.cfg, identity); err != nil {
		// Clean up partially-created buildah container on failure.
		if identity.ContainerID != "" {
			cleanupBuildahContainer(identity.ContainerID, m.cfg)
		}
		return nil, "", fmt.Errorf("pull OCI %q: %w", ref, err)
	}

	// Phase 6: Convert OCI rootfs -> qcow2 — inside lock.
	log.Printf("image %s: converting OCI rootfs -> qcow2", baseKey)

	srcPath := identity.TempPath
	if srcPath == "" {
		return nil, "", fmt.Errorf("convert %s: no source path (TempPath) set after pull", baseKey)
	}

	if _, statErr := os.Stat(srcPath); statErr != nil {
		return nil, "", fmt.Errorf("convert %s: source rootfs not found at %s: %w", baseKey, srcPath, statErr)
	}

	// Ensure cache directory exists.
	cacheDir := m.cfg.ImageCacheDir()
	if err := os.MkdirAll(cacheDir, 0o750); err != nil {
		return nil, "", fmt.Errorf("convert %s: create cache dir: %w", baseKey, err)
	}

	tmpPath := basePath + ".tmp"
	defer func() { _ = os.Remove(tmpPath) }()

	diskSize := "10G" // Default size for OCI conversion
	if err := convertOCI(ctx, srcPath, tmpPath, diskSize); err != nil {
		// Clean up buildah container on conversion failure.
		if identity.ContainerID != "" {
			cleanupBuildahContainer(identity.ContainerID, m.cfg)
		}
		return nil, "", types.NewPermanentError(fmt.Errorf("convert OCI %s: %w", baseKey, err))
	}

	// Atomic rename into cache.
	if err := os.Rename(tmpPath, basePath); err != nil {
		return nil, "", fmt.Errorf("convert %s: rename to cache: %w", baseKey, err)
	}

	// Cleanup buildah container.
	if identity.ContainerID != "" {
		cleanupBuildahContainer(identity.ContainerID, m.cfg)
	}

	log.Printf("image %s: OCI pull+conversion complete -> %s", baseKey, basePath)
	return identity, basePath, nil
}

// VerifyBootability checks if an image meets the Cocoon boot contract.
// It uses a two-tier approach:
//
// Basic verification (always available):
//   - File exists and has non-zero size
//   - qemu-img check passes (image integrity)
//   - qemu-img info confirms valid qcow2 format
//   - Optimistically sets Bootable=true if qcow2 is valid
//   - Assumes UEFI boot mode is supported (Phase 1 only supports UEFI)
//
// Deep verification (platform-dependent):
//   - Linux: uses guestfish to inspect image contents (kernel, initrd, systemd, bootloader)
//   - Darwin: stub that adds a warning (guestfish not available)
func (m *manager) VerifyBootability(ctx context.Context, imagePath string) (*image.BootCheckResult, error) {
	info, err := os.Stat(imagePath)
	if err != nil {
		return nil, fmt.Errorf("image path does not exist: %w", err)
	}

	result := &image.BootCheckResult{
		Bootable:  false,
		BootModes: []string{},
		Errors:    []string{},
		Warnings:  []string{},
	}

	// Basic check: file must have non-zero size.
	if info.Size() == 0 {
		result.Errors = append(result.Errors, "image file is empty (zero bytes)")
		return result, nil
	}

	// Basic check: qemu-img check for image integrity.
	checkCmd := exec.CommandContext(ctx, "qemu-img", "check", imagePath)
	if out, checkErr := checkCmd.CombinedOutput(); checkErr != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("qemu-img check failed: %s", strings.TrimSpace(string(out))))
		log.Printf("image %s: qemu-img check failed: %v", imagePath, checkErr)
		return result, nil
	}

	// Basic check: verify format is qcow2.
	format, err := detectImageFormat(ctx, imagePath)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("failed to detect image format: %v", err))
		return result, nil
	}
	if format != "qcow2" {
		result.Errors = append(result.Errors, fmt.Sprintf("unexpected image format %q, expected qcow2", format))
		return result, nil
	}

	// Basic verification passed. Optimistically assume bootable.
	result.Bootable = true
	result.BootModes = []string{string(types.BootModeUEFI)}

	// Attempt deep verification (platform-specific).
	if err := deepVerifyBoot(imagePath, result); err != nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf("deep verification error: %v", err))
	}

	// After deep verification, re-evaluate bootability based on findings.
	// If deep verification populated specific fields, use those to check.
	// If deep verification was skipped (darwin or no guestfish), keep
	// optimistic result and add a warning.
	deepDone := result.KernelFound || result.InitrdFound || result.SystemdFound || result.BootloaderFound
	if !deepDone {
		// Deep verification did not run; keep optimistic result.
		result.Warnings = append(result.Warnings, "deep boot contract verification requires guestfish (not available)")
	} else {
		evaluateDeepVerification(result)
	}

	log.Printf("image %s: bootability check complete: bootable=%v, modes=%v, errors=%d, warnings=%d",
		imagePath, result.Bootable, result.BootModes, len(result.Errors), len(result.Warnings))

	return result, nil
}

// evaluateDeepVerification checks deep verification results and populates
// errors, warnings, and boot modes accordingly.
func evaluateDeepVerification(result *image.BootCheckResult) {
	// Deep verification ran, evaluate results strictly.
	result.Errors = nil // reset basic-level errors
	if !result.KernelFound {
		result.Errors = append(result.Errors, "kernel not found: no /boot/vmlinuz* detected")
	}
	if !result.InitrdFound {
		result.Errors = append(result.Errors, "initrd/initramfs not found: no /boot/initrd* or /boot/initramfs* detected")
	}
	if !result.SystemdFound {
		result.Errors = append(result.Errors, "systemd not found: /sbin/init must be systemd")
	}
	if !result.BootloaderFound {
		result.Errors = append(result.Errors, "UEFI bootloader not found in ESP")
	}
	// Determine boot modes from deep findings.
	result.BootModes = nil
	if result.KernelFound && result.BootloaderFound {
		// Phase 1: Only UEFI boot is supported. Direct kernel boot will be added in Phase 2
		// when OCI VM images with extracted kernel/initramfs are implemented.
		result.BootModes = append(result.BootModes, string(types.BootModeUEFI))
	}

	result.Bootable = len(result.Errors) == 0
}

// ListCached returns all cached base images found in the image cache directory.
// Each cached image is identified by its filename pattern: {checksum_16}_{arch}.qcow2.
func (m *manager) ListCached(ctx context.Context) ([]*image.CachedImage, error) {
	cacheDir := m.cfg.ImageCacheDir()

	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read cache directory: %w", err)
	}

	var images []*image.CachedImage
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".qcow2") {
			continue
		}

		// Extract base_key from filename by removing .qcow2 suffix.
		baseKey := strings.TrimSuffix(name, ".qcow2")

		// Validate base_key format.
		if _, _, err := types.ParseBaseKey(baseKey); err != nil {
			log.Printf("warning: skipping invalid cache entry: %s (%v)", name, err)
			continue
		}

		info, err := entry.Info()
		if err != nil {
			log.Printf("warning: cannot stat cache entry %s: %v", name, err)
			continue
		}

		refCount := 0
		if m.refCtr != nil {
			if refs, refErr := m.refCtr.GetReferences(baseKey); refErr == nil {
				refCount = len(refs)
			}
		}

		images = append(images, &image.CachedImage{
			BaseKey:   baseKey,
			Path:      filepath.Join(cacheDir, name),
			Size:      info.Size(),
			CreatedAt: info.ModTime(),
			RefCount:  refCount,
		})
	}

	return images, nil
}

// RemoveCached removes a cached base image identified by its base_key.
// The base_key format is {checksum_16}_{arch}.
// It refuses to remove images that are still referenced by VMs.
func (m *manager) RemoveCached(ctx context.Context, baseKey string) error {
	if _, _, err := types.ParseBaseKey(baseKey); err != nil {
		return fmt.Errorf("invalid base_key: %w", err)
	}

	// Refuse to remove images still referenced by VMs.
	if m.refCtr != nil {
		referenced, err := m.refCtr.IsReferenced(baseKey)
		if err != nil {
			return fmt.Errorf("check references for %s: %w", baseKey, err)
		}
		if referenced {
			refs, _ := m.refCtr.GetReferences(baseKey)
			return fmt.Errorf("image %s is still referenced by VMs: %v", baseKey, refs)
		}
	}

	basePath := m.cfg.BaseImagePath(baseKey)
	if _, err := os.Stat(basePath); os.IsNotExist(err) {
		return fmt.Errorf("cached image not found: %s", baseKey)
	}

	if err := os.Remove(basePath); err != nil {
		return fmt.Errorf("remove cached image %s: %w", baseKey, err)
	}

	log.Printf("image cache: removed %s", baseKey)
	return nil
}

// --- Internal helpers ---

// classifyRef determines the image.ImageType for a given reference string.
func classifyRef(ref string) image.ImageType {
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return image.ImageTypeURL
	}

	// Local file: check if path exists on disk.
	if strings.HasPrefix(ref, "/") || strings.HasPrefix(ref, "./") || strings.HasPrefix(ref, "../") {
		return image.ImageTypeLocalFile
	}

	// If the reference has no scheme and exists as a file, treat it as local.
	if _, err := os.Stat(ref); err == nil {
		return image.ImageTypeLocalFile
	}

	// Default: treat as OCI registry reference.
	return image.ImageTypeOCI
}

// pullOCI handles pulling an OCI image from a container registry.
// It only performs the identify phase (skopeo inspect) to compute the
// content-addressed identity. The expensive pull+mount steps are deferred
// to inside the conversion lock in Prepare(). For callers using Pull()
// directly (outside the Prepare pipeline), the identity will not have
// TempPath or ContainerID set.
func (m *manager) pullOCI(ctx context.Context, ref string) (*image.ImageIdentity, error) {
	return identifyOCIPlatform(ctx, ref)
}

// pullURL handles downloading an image from an HTTP/HTTPS URL.
// It downloads the file to a temp directory, computes the SHA-256 checksum
// during download using io.TeeReader, and returns the ImageIdentity with
// the temp file path set for subsequent conversion.
func (m *manager) pullURL(ctx context.Context, ref string) (*image.ImageIdentity, error) {
	log.Printf("image pull (URL): %s", ref)

	// Ensure temp directory exists.
	tempDir := m.cfg.TempDir()
	if err := os.MkdirAll(tempDir, 0o755); err != nil { //nolint:gosec // G301: temp dir needs world-readable access for image operations
		return nil, types.NewPermanentError(fmt.Errorf("create temp dir: %w", err))
	}

	// Create temp file for download.
	tmpFile, err := os.CreateTemp(tempDir, "pull-*.img")
	if err != nil {
		return nil, types.NewPermanentError(fmt.Errorf("create temp file for download: %w", err))
	}
	tmpPath := tmpFile.Name()

	// Clean up temp file on error.
	success := false
	defer func() {
		if !success {
			_ = tmpFile.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	// Create HTTP request with context for cancellation.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref, nil)
	if err != nil {
		return nil, fmt.Errorf("create HTTP request for %s: %w", ref, err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// Classify network-level errors as transient.
		httpErr := fmt.Errorf("HTTP GET %s: %w", ref, err)
		var netErr net.Error
		if errors.As(err, &netErr) {
			return nil, types.NewTransientError(httpErr)
		}
		return nil, types.NewTransientError(httpErr)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		httpErr := fmt.Errorf("HTTP GET %s: unexpected status %d %s", ref, resp.StatusCode, resp.Status)
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return nil, types.NewTransientError(httpErr)
		}
		if resp.StatusCode >= 400 {
			return nil, types.NewPermanentError(httpErr)
		}
		return nil, httpErr
	}

	// Stream body to temp file while computing SHA-256 checksum.
	h := sha256.New()
	reader := io.TeeReader(resp.Body, h)

	contentLength := resp.ContentLength
	var written int64
	buf := make([]byte, 32*1024) // 32KB buffer
	lastLogPct := -1

	for {
		n, readErr := reader.Read(buf)
		if n > 0 {
			if _, writeErr := tmpFile.Write(buf[:n]); writeErr != nil {
				return nil, types.NewPermanentError(fmt.Errorf("write to temp file %s: %w", tmpPath, writeErr))
			}
			written += int64(n)

			// Log progress percentage if content length is known.
			if contentLength > 0 {
				pct := int(written * 100 / contentLength)
				if pct/10 > lastLogPct/10 { // log every 10%
					log.Printf("image pull (URL): %s - %d%% (%d / %d bytes)", ref, pct, written, contentLength)
					lastLogPct = pct
				}
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return nil, fmt.Errorf("read HTTP response body from %s: %w", ref, readErr)
		}
	}

	// Flush and sync temp file.
	if err := tmpFile.Sync(); err != nil {
		return nil, fmt.Errorf("sync temp file %s: %w", tmpPath, err)
	}
	if err := tmpFile.Close(); err != nil {
		return nil, fmt.Errorf("close temp file %s: %w", tmpPath, err)
	}

	log.Printf("image pull (URL): %s - downloaded %d bytes to %s", ref, written, tmpPath)

	// Compute identity from checksum.
	fullDigest := hex.EncodeToString(h.Sum(nil))
	checksum := fullDigest[:checksumHexLen]
	arch := goarchToOCI(runtime.GOARCH)

	identity := &image.ImageIdentity{
		Checksum:   checksum,
		Arch:       arch,
		FullDigest: fullDigest,
		SourceRef:  ref,
		ImageType:  image.ImageTypeURL,
		TempPath:   tmpPath,
	}

	success = true
	log.Printf("image pull (URL): %s -> base_key=%s", ref, identity.BaseKey())
	return identity, nil
}

// pullLocal handles a local file reference. It computes the file checksum to
// determine the image identity and sets TempPath to the absolute path so
// Convert can find the source file.
func (m *manager) pullLocal(_ context.Context, ref string) (*image.ImageIdentity, error) {
	absPath, err := filepath.Abs(ref)
	if err != nil {
		return nil, fmt.Errorf("resolve absolute path for %q: %w", ref, err)
	}

	if _, statErr := os.Stat(absPath); statErr != nil {
		return nil, types.NewPermanentError(fmt.Errorf("local image file not found: %w", statErr))
	}

	fullDigest, checksum, err := computeFileChecksum(absPath)
	if err != nil {
		return nil, fmt.Errorf("compute checksum for %q: %w", ref, err)
	}

	arch := defaultArch()

	identity := &image.ImageIdentity{
		Checksum:   checksum,
		Arch:       arch,
		FullDigest: fullDigest,
		SourceRef:  ref,
		ImageType:  image.ImageTypeLocalFile,
		TempPath:   absPath,
	}

	log.Printf("image pull (local): %s -> base_key=%s", ref, identity.BaseKey())
	return identity, nil
}

// Ensure time import is used (referenced in CachedImage.CreatedAt via info.ModTime()).
var _ = time.Now

// atomicCopyFile copies src to dst using a write + fsync pattern.
// The caller is responsible for the final rename if needed; this function
// writes directly to dst.
func atomicCopyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // G304: src is an internal image cache path
	if err != nil {
		return fmt.Errorf("open source %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.Create(dst) //nolint:gosec // G304: dst is an internal cache path
	if err != nil {
		return fmt.Errorf("create destination %s: %w", dst, err)
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("copy %s -> %s: %w", src, dst, err)
	}

	// Fsync to ensure data is flushed to disk before rename.
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("sync %s: %w", dst, err)
	}

	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("close %s: %w", dst, err)
	}

	return nil
}

// parseBaseKey splits a base_key ("{checksum}_{arch}") into its components.
func parseBaseKey(baseKey string) (checksum, arch string) {
	if i := strings.LastIndex(baseKey, "_"); i > 0 {
		return baseKey[:i], baseKey[i+1:]
	}
	return baseKey, ""
}

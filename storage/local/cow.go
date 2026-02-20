package local

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/storage"
	"github.com/CMGS/cocoon/utils"
)

// Compile-time interface check.
var _ storage.COWManager = (*fileCOWManager)(nil)

// fileCOWManager implements COWManager using qemu-img for overlay creation
// and standard filesystem operations for base image management.
type fileCOWManager struct {
	cfg *config.CocoonConfig
}

// NewCOWManager creates a COWManager that stores base images in
// cache/images/ and overlays under vms/{vmID}/overlay.qcow2.
func NewCOWManager(cfg *config.CocoonConfig) storage.COWManager {
	return &fileCOWManager{cfg: cfg}
}

// CreateBaseImage copies srcPath into the image cache as {baseKey}.qcow2.
// The operation is idempotent: if the destination already exists, it returns nil.
func (m *fileCOWManager) CreateBaseImage(srcPath, baseKey string) error {
	dstPath := m.cfg.BaseImagePath(baseKey)

	// Idempotent: skip if already cached.
	if _, err := os.Stat(dstPath); err == nil {
		return nil
	}

	// Ensure the cache directory exists.
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil { //nolint:gosec // G301: image cache dir needs world-readable access for VM processes
		return fmt.Errorf("create image cache dir: %w", err)
	}

	// Copy to a temp file in the same directory, then rename for atomicity.
	tmpFile, err := os.CreateTemp(filepath.Dir(dstPath), ".tmp-base-*")
	if err != nil {
		return fmt.Errorf("create temp file for base image: %w", err)
	}
	tmpPath := tmpFile.Name()

	defer func() {
		// Clean up temp on failure.
		if err != nil {
			_ = os.Remove(tmpPath)
		}
	}()

	src, err := os.Open(srcPath) //nolint:gosec // G304: srcPath is an internal image cache path, not user input
	if err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("open source image %s: %w", srcPath, err)
	}
	defer func() { _ = src.Close() }()

	if _, err = io.Copy(tmpFile, src); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("copy base image: %w", err)
	}
	if err = tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("sync base image: %w", err)
	}
	if err = tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp base image: %w", err)
	}

	if err = os.Rename(tmpPath, dstPath); err != nil {
		return fmt.Errorf("rename base image into cache: %w", err)
	}

	// Mark the base image as read-only for all users. Base images are
	// immutable -- VMs use COW overlays, never write to the base.
	if err = os.Chmod(dstPath, 0o444); err != nil { //nolint:gosec // G302: intentionally world-readable; base images are immutable, VMs use COW overlays
		return fmt.Errorf("chmod base image read-only: %w", err)
	}
	return nil
}

// CreateOverlay creates a COW overlay backed by {baseKey}.qcow2.
//
// The overlay is written to /var/lib/cocoon/vms/{vmID}/overlay.qcow2 using
// `qemu-img create -f qcow2 -F qcow2 -b <backing>`.
//
// No global lock is required because each VM directory is unique.
func (m *fileCOWManager) CreateOverlay(ctx context.Context, baseKey, vmID, diskSize string) (string, error) {
	basePath := m.cfg.BaseImagePath(baseKey)
	if _, err := os.Stat(basePath); err != nil {
		return "", fmt.Errorf("base image not found for key %s: %w", baseKey, err)
	}

	vmDir := m.cfg.VMPersistDir(vmID)
	if err := os.MkdirAll(vmDir, 0o700); err != nil { //nolint:gosec // G301: restricted to owner — VM persistent state
		return "", fmt.Errorf("create VM directory: %w", err)
	}

	overlayPath := m.cfg.VMOverlayPath(vmID)

	// qemu-img create -f qcow2 -F qcow2 -b <backing> <overlay>
	cmd := exec.CommandContext(ctx, //nolint:gosec // G204: args are controlled internal paths, not user input
		"qemu-img", "create",
		"-f", "qcow2",
		"-F", "qcow2",
		"-b", basePath,
		overlayPath,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("qemu-img create overlay: %s: %w", string(output), err)
	}

	// Optionally resize the overlay to the requested disk size.
	if diskSize != "" {
		resizeCmd := exec.CommandContext(ctx, "qemu-img", "resize", overlayPath, diskSize) //nolint:gosec // G204: args are controlled internal values, not user input
		resizeOut, resizeErr := resizeCmd.CombinedOutput()
		if resizeErr != nil {
			_ = os.Remove(overlayPath)
			return "", fmt.Errorf("qemu-img resize overlay to %s: %s: %w", diskSize, string(resizeOut), resizeErr)
		}
	}

	// Fsync the overlay file and its parent directory to ensure qcow2
	// metadata (header, L1/L2 tables, refcounts) written by qemu-img
	// create and resize is persisted to disk before the VM boots.
	if err := syncFile(overlayPath); err != nil {
		_ = os.Remove(overlayPath)
		return "", fmt.Errorf("fsync overlay after create: %w", err)
	}
	if err := utils.SyncParentDir(filepath.Dir(overlayPath)); err != nil {
		_ = os.Remove(overlayPath)
		return "", fmt.Errorf("fsync overlay directory: %w", err)
	}

	return overlayPath, nil
}

// RepairOverlay runs `qemu-img check --repair=leaks` on the overlay for vmID.
// This repairs leaked clusters that may result from an unclean VM shutdown
// (e.g., SIGKILL of the Cloud Hypervisor process). The repair is idempotent:
// a clean overlay passes the check with no modifications.
func (m *fileCOWManager) RepairOverlay(ctx context.Context, vmID string) error {
	overlayPath := m.cfg.VMOverlayPath(vmID)
	if _, err := os.Stat(overlayPath); err != nil {
		return fmt.Errorf("overlay not found for %s: %w", vmID, err)
	}

	cmd := exec.CommandContext(ctx, "qemu-img", "check", "--repair=leaks", overlayPath) //nolint:gosec // G204: overlayPath is a controlled internal path
	output, err := cmd.CombinedOutput()
	if err != nil {
		// qemu-img check exits 2 for corrected leaks (repairs applied),
		// 3 for uncorrectable corruption. Exit 0 means clean.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 2 {
			log.Printf("qemu-img repaired leaked clusters in overlay for %s: %s", vmID, string(output))
			return nil
		}
		return fmt.Errorf("qemu-img check overlay for %s: %s: %w", vmID, string(output), err)
	}
	return nil
}

// syncFile opens a file and fsyncs it to flush page-cache writes to disk.
func syncFile(path string) error {
	f, err := os.Open(path) //nolint:gosec // G304: path is a controlled internal path
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}

// RemoveOverlay permanently removes the overlay disk file for vmID.
//
// The VM directory itself is intentionally preserved for the caller to handle
// (for example, manager.Delete() will remove config/metadata/log references).
func (m *fileCOWManager) RemoveOverlay(vmID string) error {
	overlayPath := m.cfg.VMOverlayPath(vmID)
	if _, err := os.Stat(overlayPath); os.IsNotExist(err) {
		return nil // already gone
	}
	return os.Remove(overlayPath)
}

// GetOverlayInfo returns metadata about an existing overlay by running
// `qemu-img info --output=json` and combining it with filesystem stat data.
func (m *fileCOWManager) GetOverlayInfo(ctx context.Context, vmID string) (*storage.OverlayInfo, error) {
	overlayPath := m.cfg.VMOverlayPath(vmID)

	fi, err := os.Stat(overlayPath)
	if err != nil {
		return nil, fmt.Errorf("stat overlay %s: %w", overlayPath, err)
	}

	// Run qemu-img info to get backing file and size details.
	cmd := exec.CommandContext(ctx, "qemu-img", "info", "--output=json", overlayPath) //nolint:gosec // G204: overlayPath is a controlled internal path
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("qemu-img info: %w", err)
	}

	var qemuInfo struct {
		VirtualSize   int64  `json:"virtual-size"`
		ActualSize    int64  `json:"actual-size"`
		Format        string `json:"format"`
		BackingFile   string `json:"backing-filename"`
		BackingFormat string `json:"backing-format"`
		FullBacking   string `json:"full-backing-filename"`
	}
	if err := json.Unmarshal(output, &qemuInfo); err != nil {
		return nil, fmt.Errorf("parse qemu-img info output: %w", err)
	}

	info := &storage.OverlayInfo{
		VMID:          vmID,
		OverlayPath:   overlayPath,
		BackingFile:   qemuInfo.BackingFile,
		VirtualSize:   qemuInfo.VirtualSize,
		ActualSize:    qemuInfo.ActualSize,
		Format:        qemuInfo.Format,
		BackingFormat: qemuInfo.BackingFormat,
		CreatedAt:     fi.ModTime().UTC().Truncate(time.Second),
	}
	return info, nil
}

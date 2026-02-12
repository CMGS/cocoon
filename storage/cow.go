package storage

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/projecteru2/cocoon/config"
)

// Compile-time interface check.
var _ COWManager = (*fileCOWManager)(nil)

// fileCOWManager implements COWManager using qemu-img for overlay creation
// and standard filesystem operations for base image management.
type fileCOWManager struct {
	cfg *config.CocoonConfig
}

// NewCOWManager creates a COWManager that stores base images in
// cache/images/ and overlays under vms/{vmID}/overlay.qcow2.
func NewCOWManager(cfg *config.CocoonConfig) COWManager {
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
	if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
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
			os.Remove(tmpPath)
		}
	}()

	src, err := os.Open(srcPath)
	if err != nil {
		tmpFile.Close()
		return fmt.Errorf("open source image %s: %w", srcPath, err)
	}
	defer src.Close()

	if _, err = io.Copy(tmpFile, src); err != nil {
		tmpFile.Close()
		return fmt.Errorf("copy base image: %w", err)
	}
	if err = tmpFile.Sync(); err != nil {
		tmpFile.Close()
		return fmt.Errorf("sync base image: %w", err)
	}
	if err = tmpFile.Chmod(0644); err != nil {
		tmpFile.Close()
		return fmt.Errorf("chmod base image: %w", err)
	}
	if err = tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp base image: %w", err)
	}

	if err = os.Rename(tmpPath, dstPath); err != nil {
		return fmt.Errorf("rename base image into cache: %w", err)
	}
	return nil
}

// CreateOverlay creates a COW overlay backed by {baseKey}.qcow2.
//
// The overlay is written to /var/lib/cocoon/vms/{vmID}/overlay.qcow2 using
// `qemu-img create -f qcow2 -F qcow2 -b <backing>`.
//
// No global lock is required because each VM directory is unique.
func (m *fileCOWManager) CreateOverlay(baseKey, vmID string) (string, error) {
	basePath := m.cfg.BaseImagePath(baseKey)
	if _, err := os.Stat(basePath); err != nil {
		return "", fmt.Errorf("base image not found for key %s: %w", baseKey, err)
	}

	vmDir := m.cfg.VMPersistDir(vmID)
	if err := os.MkdirAll(vmDir, 0755); err != nil {
		return "", fmt.Errorf("create VM directory: %w", err)
	}

	overlayPath := m.cfg.VMOverlayPath(vmID)

	// qemu-img create -f qcow2 -F qcow2 -b <backing> <overlay>
	cmd := exec.Command(
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

	return overlayPath, nil
}

// RemoveOverlay removes the entire VM persistent directory
// (/var/lib/cocoon/vms/{vmID}), which contains overlay.qcow2, config.json,
// metadata.json, etc.
func (m *fileCOWManager) RemoveOverlay(vmID string) error {
	vmDir := m.cfg.VMPersistDir(vmID)
	if _, err := os.Stat(vmDir); os.IsNotExist(err) {
		return nil // already gone
	}
	if err := os.RemoveAll(vmDir); err != nil {
		return fmt.Errorf("remove VM directory %s: %w", vmDir, err)
	}
	return nil
}

// GetOverlayInfo returns metadata about an existing overlay by running
// `qemu-img info --output=json` and combining it with filesystem stat data.
func (m *fileCOWManager) GetOverlayInfo(vmID string) (*OverlayInfo, error) {
	overlayPath := m.cfg.VMOverlayPath(vmID)

	fi, err := os.Stat(overlayPath)
	if err != nil {
		return nil, fmt.Errorf("stat overlay %s: %w", overlayPath, err)
	}

	// Run qemu-img info to get backing file and size details.
	cmd := exec.Command("qemu-img", "info", "--output=json", overlayPath)
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

	info := &OverlayInfo{
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

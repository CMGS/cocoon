package storage

import "time"

// OverlayInfo holds metadata about a COW overlay image.
// Fields map to the output of `qemu-img info --output=json`.
type OverlayInfo struct {
	// VMID is the owning VM identifier (vm-{ulid}).
	VMID string `json:"vm_id"`

	// OverlayPath is the absolute path to overlay.qcow2.
	OverlayPath string `json:"overlay_path"`

	// BackingFile is the absolute path to the base image.
	BackingFile string `json:"backing_file"`

	// VirtualSize is the maximum size of the overlay in bytes.
	VirtualSize int64 `json:"virtual_size"`

	// ActualSize is the current disk usage of the overlay in bytes.
	ActualSize int64 `json:"actual_size"`

	// Format is the image format (always "qcow2").
	Format string `json:"format"`

	// BackingFormat is the format of the backing file (always "qcow2").
	BackingFormat string `json:"backing_format"`

	// CreatedAt is the overlay file creation time.
	CreatedAt time.Time `json:"created_at"`
}

package types

// VMConfig represents the immutable VM configuration (config.json).
// Written once at VM creation and never modified.
type VMConfig struct {
	// Identity
	VMID string `json:"vm_id"`
	Name string `json:"name"`

	// Image provenance
	ImageRef       string `json:"image_ref"`
	BaseKey        string `json:"base_key"`         // {checksum_16}_{arch}
	BaseDigestFull string `json:"base_digest_full"` // Full SHA-256 (64 hex chars)
	Arch           string `json:"arch"`

	// Boot configuration
	BootStrategy  BootStrategy `json:"boot_strategy"`
	FirmwarePath  string       `json:"firmware_path"`
	KernelPath    string       `json:"kernel_path,omitempty"`
	InitramfsPath string       `json:"initramfs_path,omitempty"`
	Cmdline       string       `json:"cmdline,omitempty"`
	TPMSocketPath string       `json:"tpm_socket_path,omitempty"`

	// Resources
	CPUs     int    `json:"cpus"`
	MemoryMB int64  `json:"memory_mb"`
	DiskSize string `json:"disk_size"`

	// Storage paths (derived, stored for fast lookup)
	BaseImagePath string `json:"base_image_path"`
	OverlayPath   string `json:"overlay_path"`
	SerialLog     string `json:"serial_log"`
	SocketPath    string `json:"socket_path"`

	// Timestamps
	CreatedAt string `json:"created_at"` // RFC 3339

	// Schema version for migration
	SchemaVersion int `json:"schema_version"`
}

// CurrentConfigSchemaVersion is the current config.json schema version.
const CurrentConfigSchemaVersion = 1

// Default resource values.
const (
	DefaultCPUs     = 2
	DefaultMemoryMB = 2048
	DefaultDiskSize = "10G"
)

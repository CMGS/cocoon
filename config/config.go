package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// CocoonConfig holds global Cocoon configuration.
type CocoonConfig struct {
	// Root directory for persistent data.
	RootDir string `json:"root_dir"`
	// Runtime directory (tmpfs, cleared on reboot).
	RuntimeDir string `json:"runtime_dir"`
	// Log directory.
	LogDir string `json:"log_dir"`

	// Cloud Hypervisor binary path.
	CHBinary string `json:"ch_binary"`

	// Firmware paths.
	PVHFirmwarePath  string `json:"pvh_firmware_path"`
	UEFIFirmwarePath string `json:"uefi_firmware_path"`

	// Buildah storage root (for OCI operations).
	BuildahRoot string `json:"buildah_root"`

	// Default VM resource values.
	DefaultCPUs     int    `json:"default_cpus"`
	DefaultMemoryMB int64  `json:"default_memory_mb"`
	DefaultDiskSize string `json:"default_disk_size"`

	// GC configuration.
	GCGracePeriodHours int `json:"gc_grace_period_hours"`
	GCTrashRetentDays  int `json:"gc_trash_retention_days"`

	// Boot timeout in seconds.
	BootTimeoutSeconds int `json:"boot_timeout_seconds"`
	// Stop timeout in seconds.
	StopTimeoutSeconds int `json:"stop_timeout_seconds"`
}

// DefaultConfig returns a CocoonConfig with default values.
func DefaultConfig() *CocoonConfig {
	return &CocoonConfig{
		RootDir:    "/var/lib/cocoon",
		RuntimeDir: "/run/cocoon",
		LogDir:     "/var/log/cocoon",

		CHBinary:         "cloud-hypervisor",
		PVHFirmwarePath:  "/var/lib/cocoon/firmware/hypervisor-fw",
		UEFIFirmwarePath: "/var/lib/cocoon/firmware/CLOUDHV.fd",
		BuildahRoot:      "/var/lib/cocoon/buildah",

		DefaultCPUs:     2,
		DefaultMemoryMB: 2048,
		DefaultDiskSize: "10G",

		GCGracePeriodHours: 24,
		GCTrashRetentDays:  7,

		BootTimeoutSeconds: 60,
		StopTimeoutSeconds: 30,
	}
}

// LoadConfig loads configuration from file, falling back to defaults.
func LoadConfig(path string) (*CocoonConfig, error) {
	cfg := DefaultConfig()
	if path == "" {
		return cfg, nil
	}

	data, err := os.ReadFile(path) //nolint:gosec // G304: config file path is from CLI flag or env var, intentional
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}

	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}

// Derived path helpers.

func (c *CocoonConfig) CacheDir() string {
	return filepath.Join(c.RootDir, "cache")
}

func (c *CocoonConfig) ImageCacheDir() string {
	return filepath.Join(c.RootDir, "cache", "images")
}

func (c *CocoonConfig) ManifestCacheDir() string {
	return filepath.Join(c.RootDir, "cache", "manifests")
}

func (c *CocoonConfig) ConversionLockDir() string {
	return filepath.Join(c.RootDir, "cache", "locks")
}

func (c *CocoonConfig) VMDir() string {
	return filepath.Join(c.RootDir, "vms")
}

func (c *CocoonConfig) TempDir() string {
	return filepath.Join(c.RootDir, "temp")
}

func (c *CocoonConfig) TrashDir() string {
	return filepath.Join(c.RootDir, "trash")
}

func (c *CocoonConfig) FirmwareDir() string {
	return filepath.Join(c.RootDir, "firmware")
}

func (c *CocoonConfig) DBDir() string {
	return filepath.Join(c.RootDir, "db")
}

func (c *CocoonConfig) ReferencesFile() string {
	return filepath.Join(c.RootDir, "db", "references.json")
}

func (c *CocoonConfig) ReferencesLock() string {
	return filepath.Join(c.RootDir, "db", "references.lock")
}

func (c *CocoonConfig) GCLock() string {
	return filepath.Join(c.RootDir, "db", "gc.lock")
}

func (c *CocoonConfig) NameIndexFile() string {
	return filepath.Join(c.RootDir, "db", "name-index.json")
}

func (c *CocoonConfig) NameIndexLock() string {
	return filepath.Join(c.RootDir, "db", "name-index.lock")
}

// Per-VM paths.

func (c *CocoonConfig) VMPersistDir(vmID string) string {
	return filepath.Join(c.RootDir, "vms", vmID)
}

func (c *CocoonConfig) VMRuntimeDir(vmID string) string {
	return filepath.Join(c.RuntimeDir, "vms", vmID)
}

func (c *CocoonConfig) VMConfigPath(vmID string) string {
	return filepath.Join(c.RootDir, "vms", vmID, "config.json")
}

func (c *CocoonConfig) VMMetadataPath(vmID string) string {
	return filepath.Join(c.RootDir, "vms", vmID, "metadata.json")
}

func (c *CocoonConfig) VMMetadataLock(vmID string) string {
	return filepath.Join(c.RootDir, "vms", vmID, "metadata.lock")
}

func (c *CocoonConfig) VMOverlayPath(vmID string) string {
	return filepath.Join(c.RootDir, "vms", vmID, "overlay.qcow2")
}

func (c *CocoonConfig) VMSocketPath(vmID string) string {
	return filepath.Join(c.RuntimeDir, "vms", vmID, "api.sock")
}

func (c *CocoonConfig) VMPIDPath(vmID string) string {
	return filepath.Join(c.RuntimeDir, "vms", vmID, "ch.pid")
}

func (c *CocoonConfig) VMSerialLogPath(vmID string) string {
	return filepath.Join(c.LogDir, vmID+"-serial.log")
}

func (c *CocoonConfig) BaseImagePath(baseKey string) string {
	return filepath.Join(c.RootDir, "cache", "images", baseKey+".qcow2")
}

func (c *CocoonConfig) ConversionLockPath(baseKey string) string {
	return filepath.Join(c.RootDir, "cache", "locks", baseKey+".lock")
}

// EnsureDirs creates all required directories.
func (c *CocoonConfig) EnsureDirs() error {
	dirs := []string{
		c.DBDir(),
		c.ImageCacheDir(),
		c.ManifestCacheDir(),
		c.ConversionLockDir(),
		c.VMDir(),
		c.TempDir(),
		c.TrashDir(),
		c.FirmwareDir(),
		c.BuildahRoot,
		filepath.Join(c.RuntimeDir, "vms"),
		c.LogDir,
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: cocoon directories need world-readable access for VM processes
			return fmt.Errorf("create directory %s: %w", dir, err)
		}
	}
	return nil
}

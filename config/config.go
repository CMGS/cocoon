package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// UEFIFallbackPaths lists system OVMF firmware paths to probe when the
// primary UEFI firmware (CLOUDHV.fd) is not found. These are deprecated
// fallbacks; users should install CLOUDHV.fd via 'cocoon firmware install'.
var UEFIFallbackPaths = []string{
	"/usr/share/OVMF/OVMF_CODE.fd",
	"/usr/share/edk2/ovmf/OVMF_CODE.fd",
}

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

	// BootSuccessPatterns are regex patterns indicating successful boot.
	// If empty, defaults are used.
	BootSuccessPatterns []string `json:"boot_success_patterns,omitempty"`
	// BootFailurePatterns are regex patterns indicating boot failure.
	// If empty, defaults are used.
	BootFailurePatterns []string `json:"boot_failure_patterns,omitempty"`
}

// DefaultConfig returns a CocoonConfig with default values.
func DefaultConfig() *CocoonConfig {
	return &CocoonConfig{
		RootDir:    "/var/lib/cocoon",
		RuntimeDir: "/run/cocoon",
		LogDir:     "/var/log/cocoon",

		CHBinary:         "cloud-hypervisor",
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

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate checks that the configuration values are within acceptable ranges.
func (c *CocoonConfig) Validate() error {
	if c.DefaultCPUs <= 0 {
		return fmt.Errorf("config: DefaultCPUs must be > 0, got %d", c.DefaultCPUs)
	}
	if c.DefaultMemoryMB <= 0 {
		return fmt.Errorf("config: DefaultMemoryMB must be > 0, got %d", c.DefaultMemoryMB)
	}
	// Guard against overflow: MemoryMB * 1024 * 1024 must fit in int64.
	// 8 PB / (1024*1024) = 8_589_934_592 MB. Cap at 1 TB (1_048_576 MB)
	// which is well above any practical use case.
	const maxMemoryMB int64 = 1_048_576 // 1 TB
	if c.DefaultMemoryMB > maxMemoryMB {
		return fmt.Errorf("config: DefaultMemoryMB must be <= %d (1 TB), got %d", maxMemoryMB, c.DefaultMemoryMB)
	}
	if c.GCGracePeriodHours < 0 {
		return fmt.Errorf("config: GCGracePeriodHours must be >= 0, got %d", c.GCGracePeriodHours)
	}
	if c.BootTimeoutSeconds < 0 {
		return fmt.Errorf("config: BootTimeoutSeconds must be >= 0, got %d", c.BootTimeoutSeconds)
	}
	return nil
}

// RebaseRootDir updates RootDir and re-derives any config paths that were
// relative to the old root. Paths explicitly set to non-default locations
// (not under oldRoot) are left unchanged.
func (c *CocoonConfig) RebaseRootDir(newRoot string) {
	oldRoot := c.RootDir
	c.RootDir = newRoot

	rebase := func(path string) string {
		if strings.HasPrefix(path, oldRoot+"/") {
			return filepath.Join(newRoot, path[len(oldRoot):])
		}
		return path
	}

	c.BuildahRoot = rebase(c.BuildahRoot)
	c.UEFIFirmwarePath = rebase(c.UEFIFirmwarePath)
}

// BootSuccessPatternsOrDefault returns the configured boot success patterns,
// or sensible defaults if none are configured.
func (c *CocoonConfig) BootSuccessPatternsOrDefault() []string {
	if len(c.BootSuccessPatterns) > 0 {
		return c.BootSuccessPatterns
	}
	return []string{
		`login:`,
		`Reached target.*Login`,
		`systemd .* running`,
	}
}

// BootFailurePatternsOrDefault returns the configured boot failure patterns,
// or sensible defaults if none are configured.
func (c *CocoonConfig) BootFailurePatternsOrDefault() []string {
	if len(c.BootFailurePatterns) > 0 {
		return c.BootFailurePatterns
	}
	return []string{
		`Kernel panic`,
		`not syncing`,
		`No working init found`,
		`Failed to execute /init`,
	}
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

func (c *CocoonConfig) VMTPMStateDir(vmID string) string {
	return filepath.Join(c.RootDir, "vms", vmID, "tpm")
}

func (c *CocoonConfig) VMTPMSocketPath(vmID string) string {
	return filepath.Join(c.RuntimeDir, "vms", vmID, "swtpm.sock")
}

func (c *CocoonConfig) VMSwtpmPIDPath(vmID string) string {
	return filepath.Join(c.RuntimeDir, "vms", vmID, "swtpm.pid")
}

func (c *CocoonConfig) VMSwtpmLogPath(vmID string) string {
	return filepath.Join(c.LogDir, vmID+"-swtpm.log")
}

func (c *CocoonConfig) VMCHLogPath(vmID string) string {
	return filepath.Join(c.LogDir, vmID+"-ch.log")
}

// BaseImagePath returns the path to a cached base image.
// The baseKey MUST have been validated via types.ParseBaseKey before calling.
func (c *CocoonConfig) BaseImagePath(baseKey string) string {
	// Defense-in-depth: reject path separators and traversal.
	if strings.ContainsAny(baseKey, "/\\") || strings.Contains(baseKey, "..") {
		return filepath.Join(c.RootDir, "cache", "images", "invalid-key.qcow2")
	}
	return filepath.Join(c.RootDir, "cache", "images", baseKey+".qcow2")
}

// ConversionLockPath returns the path to a per-image conversion lock.
// The baseKey MUST have been validated via types.ParseBaseKey before calling.
func (c *CocoonConfig) ConversionLockPath(baseKey string) string {
	// Defense-in-depth: reject path separators and traversal.
	if strings.ContainsAny(baseKey, "/\\") || strings.Contains(baseKey, "..") {
		return filepath.Join(c.RootDir, "cache", "locks", "invalid-key.lock")
	}
	return filepath.Join(c.RootDir, "cache", "locks", baseKey+".lock")
}

func (c *CocoonConfig) OCICacheDir() string {
	return filepath.Join(c.RootDir, "cache", "oci")
}

func (c *CocoonConfig) OCIBlobDir() string {
	return filepath.Join(c.RootDir, "cache", "oci", "blobs", "sha256")
}

func (c *CocoonConfig) OCILayoutDir() string {
	return filepath.Join(c.RootDir, "cache", "oci", "layouts")
}

func (c *CocoonConfig) OCILayerRefsFile() string {
	return filepath.Join(c.RootDir, "db", "oci-layer-refs.json")
}

func (c *CocoonConfig) OCILayerRefsLock() string {
	return filepath.Join(c.RootDir, "db", "oci-layer-refs.lock")
}

func (c *CocoonConfig) OCIBuildTagIndex() string {
	return filepath.Join(c.RootDir, "db", "oci-build-tags.json")
}

func (c *CocoonConfig) OCIBuildTagLock() string {
	return filepath.Join(c.RootDir, "db", "oci-build-tags.lock")
}

func (c *CocoonConfig) OCIBuildTxnLock() string {
	return filepath.Join(c.RootDir, "db", "oci-build-txn.lock")
}

// EnsureDirs creates all required directories.
func (c *CocoonConfig) EnsureDirs() error {
	dirs := []string{
		c.DBDir(),
		c.ImageCacheDir(),
		c.ManifestCacheDir(),
		c.ConversionLockDir(),
		c.OCIBlobDir(),
		c.OCILayoutDir(),
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

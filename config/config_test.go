package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	// Root directories.
	if cfg.RootDir != "/var/lib/cocoon" {
		t.Errorf("RootDir = %q, want /var/lib/cocoon", cfg.RootDir)
	}
	if cfg.RuntimeDir != "/run/cocoon" {
		t.Errorf("RuntimeDir = %q, want /run/cocoon", cfg.RuntimeDir)
	}
	if cfg.LogDir != "/var/log/cocoon" {
		t.Errorf("LogDir = %q, want /var/log/cocoon", cfg.LogDir)
	}

	// Default VM resources.
	if cfg.DefaultCPUs != 2 {
		t.Errorf("DefaultCPUs = %d, want 2", cfg.DefaultCPUs)
	}
	if cfg.DefaultMemoryMB != 2048 {
		t.Errorf("DefaultMemoryMB = %d, want 2048", cfg.DefaultMemoryMB)
	}

	// Timeouts.
	if cfg.BootTimeoutSeconds != 60 {
		t.Errorf("BootTimeoutSeconds = %d, want 60", cfg.BootTimeoutSeconds)
	}

	// Firmware paths.
	if cfg.UEFIFirmwarePath != "/var/lib/cocoon/firmware/CLOUDHV.fd" {
		t.Errorf("UEFIFirmwarePath = %q, want /var/lib/cocoon/firmware/CLOUDHV.fd", cfg.UEFIFirmwarePath)
	}

	// BuildahRoot.
	if cfg.BuildahRoot != "/var/lib/cocoon/buildah" {
		t.Errorf("BuildahRoot = %q, want /var/lib/cocoon/buildah", cfg.BuildahRoot)
	}
}

func TestLoadConfig_EmptyPath(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig(\"\") error: %v", err)
	}
	if cfg.RootDir != "/var/lib/cocoon" {
		t.Errorf("expected default RootDir, got %q", cfg.RootDir)
	}
}

func TestLoadConfig_NonExistentFile(t *testing.T) {
	cfg, err := LoadConfig("/nonexistent/path/cocoon.json")
	if err != nil {
		t.Fatalf("LoadConfig(nonexistent) error: %v", err)
	}
	// Should return defaults.
	if cfg.RootDir != "/var/lib/cocoon" {
		t.Errorf("expected default RootDir, got %q", cfg.RootDir)
	}
}

func TestLoadConfig_ValidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	// Only override a few fields; the rest should stay at defaults.
	partial := map[string]any{
		"root_dir":     "/custom/root",
		"default_cpus": 8,
	}
	data, err := json.Marshal(partial)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig error: %v", err)
	}
	if cfg.RootDir != "/custom/root" {
		t.Errorf("RootDir = %q, want /custom/root", cfg.RootDir)
	}
	if cfg.DefaultCPUs != 8 {
		t.Errorf("DefaultCPUs = %d, want 8", cfg.DefaultCPUs)
	}
	// Unspecified field keeps default.
	if cfg.DefaultMemoryMB != 2048 {
		t.Errorf("DefaultMemoryMB = %d, want 2048 (default)", cfg.DefaultMemoryMB)
	}
	if cfg.BootTimeoutSeconds != 60 {
		t.Errorf("BootTimeoutSeconds = %d, want 60 (default)", cfg.BootTimeoutSeconds)
	}
}

func TestLoadConfig_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte("{not valid json}"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestRebaseRootDir(t *testing.T) {
	cfg := DefaultConfig()
	cfg.RebaseRootDir("/tmp/test")

	if cfg.RootDir != "/tmp/test" {
		t.Errorf("RootDir = %q, want /tmp/test", cfg.RootDir)
	}
	if cfg.BuildahRoot != "/tmp/test/buildah" {
		t.Errorf("BuildahRoot = %q, want /tmp/test/buildah", cfg.BuildahRoot)
	}
	if cfg.UEFIFirmwarePath != "/tmp/test/firmware/CLOUDHV.fd" {
		t.Errorf("UEFIFirmwarePath = %q, want /tmp/test/firmware/CLOUDHV.fd", cfg.UEFIFirmwarePath)
	}
}

func TestRebaseRootDir_NonDefaultPathUnchanged(t *testing.T) {
	cfg := DefaultConfig()
	// Set a path that is NOT under the default root.
	cfg.BuildahRoot = "/opt/custom/buildah"
	cfg.RebaseRootDir("/tmp/test")

	// Should NOT be rebased because it was not under the old root.
	if cfg.BuildahRoot != "/opt/custom/buildah" {
		t.Errorf("BuildahRoot = %q, want /opt/custom/buildah (unchanged)", cfg.BuildahRoot)
	}
}

func TestDerivedPathHelpers(t *testing.T) {
	cfg := DefaultConfig()

	tests := []struct {
		name string
		got  string
		want string
	}{
		{"VMConfigPath", cfg.VMConfigPath("vm-123"), "/var/lib/cocoon/vms/vm-123/config.json"},
		{"VMMetadataPath", cfg.VMMetadataPath("vm-123"), "/var/lib/cocoon/vms/vm-123/metadata.json"},
		{"VMSocketPath", cfg.VMSocketPath("vm-123"), "/run/cocoon/vms/vm-123/api.sock"},
		{"VMSerialLogPath", cfg.VMSerialLogPath("vm-123"), "/var/log/cocoon/vm-123-serial.log"},
		{"DBDir", cfg.DBDir(), "/var/lib/cocoon/db"},
		{"ImageCacheDir", cfg.ImageCacheDir(), "/var/lib/cocoon/cache/images"},
		{"BaseImagePath", cfg.BaseImagePath("abc123_amd64"), "/var/lib/cocoon/cache/images/abc123_amd64.qcow2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("%s = %q, want %q", tt.name, tt.got, tt.want)
			}
		})
	}
}

func TestBootSuccessPatternsOrDefault(t *testing.T) {
	// Default patterns.
	cfg := DefaultConfig()
	patterns := cfg.BootSuccessPatternsOrDefault()
	if len(patterns) != 3 {
		t.Errorf("expected 3 default success patterns, got %d", len(patterns))
	}

	// Custom patterns.
	cfg.BootSuccessPatterns = []string{"custom-pattern"}
	patterns = cfg.BootSuccessPatternsOrDefault()
	if len(patterns) != 1 || patterns[0] != "custom-pattern" {
		t.Errorf("expected [custom-pattern], got %v", patterns)
	}
}

func TestBootFailurePatternsOrDefault(t *testing.T) {
	// Default patterns.
	cfg := DefaultConfig()
	patterns := cfg.BootFailurePatternsOrDefault()
	if len(patterns) != 4 {
		t.Errorf("expected 4 default failure patterns, got %d", len(patterns))
	}

	// Custom patterns.
	cfg.BootFailurePatterns = []string{"my-panic", "my-error"}
	patterns = cfg.BootFailurePatternsOrDefault()
	if len(patterns) != 2 {
		t.Errorf("expected 2 custom failure patterns, got %d", len(patterns))
	}
	if patterns[0] != "my-panic" {
		t.Errorf("patterns[0] = %q, want my-panic", patterns[0])
	}
}

func TestEnsureDirs(t *testing.T) {
	root := t.TempDir()
	runtimeDir := filepath.Join(root, "runtime")
	logDir := filepath.Join(root, "logs")

	cfg := DefaultConfig()
	cfg.RootDir = root
	cfg.RuntimeDir = runtimeDir
	cfg.LogDir = logDir
	cfg.BuildahRoot = filepath.Join(root, "buildah")

	if err := cfg.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs error: %v", err)
	}

	// Verify all expected directories exist.
	expectedDirs := []string{
		cfg.DBDir(),
		cfg.ImageCacheDir(),
		cfg.ManifestCacheDir(),
		cfg.ConversionLockDir(),
		cfg.OCIBlobDir(),
		cfg.OCILayoutDir(),
		cfg.VMDir(),
		cfg.TempDir(),
		cfg.TrashDir(),
		cfg.FirmwareDir(),
		cfg.BuildahRoot,
		filepath.Join(cfg.RuntimeDir, "vms"),
		cfg.LogDir,
	}
	for _, dir := range expectedDirs {
		info, err := os.Stat(dir)
		if err != nil {
			t.Errorf("directory %q does not exist: %v", dir, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%q is not a directory", dir)
		}
	}
}

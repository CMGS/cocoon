package local

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/CMGS/cocoon/config"
)

func TestRemoveOverlay_RemovesOnlyOverlayFile(t *testing.T) {
	root := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.RebaseRootDir(root)
	cfg.RuntimeDir = filepath.Join(root, "run")
	cfg.LogDir = filepath.Join(root, "log")
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}

	mgr := NewCOWManager(cfg)
	vmID := "vm-test-001"
	vmDir := cfg.VMPersistDir(vmID)
	if err := os.MkdirAll(vmDir, 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatalf("MkdirAll vmDir: %v", err)
	}

	overlayPath := cfg.VMOverlayPath(vmID)
	if err := os.WriteFile(overlayPath, []byte("overlay"), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatalf("WriteFile overlay: %v", err)
	}
	configPath := cfg.VMConfigPath(vmID)
	if err := os.WriteFile(configPath, []byte("{}"), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatalf("WriteFile config: %v", err)
	}

	if err := mgr.RemoveOverlay(vmID); err != nil {
		t.Fatalf("RemoveOverlay: %v", err)
	}

	if _, err := os.Stat(overlayPath); !os.IsNotExist(err) {
		t.Fatalf("overlay should be removed from vmDir, stat err=%v", err)
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("config should remain in vmDir, stat err=%v", err)
	}

	trashEntries, err := os.ReadDir(cfg.TrashDir())
	if err != nil {
		t.Fatalf("ReadDir trash: %v", err)
	}
	if len(trashEntries) != 1 {
		t.Fatalf("trash entry count=%d, want 1", len(trashEntries))
	}
	if filepath.Ext(trashEntries[0].Name()) != ".qcow2" {
		t.Fatalf("trash entry %q should be qcow2 file", trashEntries[0].Name())
	}
}

func TestRemoveOverlay_NoOverlayNoop(t *testing.T) {
	root := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.RebaseRootDir(root)
	cfg.RuntimeDir = filepath.Join(root, "run")
	cfg.LogDir = filepath.Join(root, "log")
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}

	mgr := NewCOWManager(cfg)
	vmID := "vm-test-002"
	vmDir := cfg.VMPersistDir(vmID)
	if err := os.MkdirAll(vmDir, 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatalf("MkdirAll vmDir: %v", err)
	}

	if err := mgr.RemoveOverlay(vmID); err != nil {
		t.Fatalf("RemoveOverlay should no-op when overlay missing: %v", err)
	}
	if _, err := os.Stat(vmDir); err != nil {
		t.Fatalf("vmDir should remain, stat err=%v", err)
	}
}

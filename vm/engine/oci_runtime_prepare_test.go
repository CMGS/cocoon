package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPromoteOCIRuntimeCacheDir_NewEntry(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	finalDir := filepath.Join(baseDir, "runtime", "abc123")
	workDir := filepath.Join(baseDir, "work-new")
	if err := os.MkdirAll(filepath.Join(workDir, "rootfs"), 0o755); err != nil { //nolint:gosec // test workspace
		t.Fatalf("mkdir work rootfs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "rootfs", "marker"), []byte("new"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(finalDir), 0o755); err != nil { //nolint:gosec // test workspace
		t.Fatalf("mkdir final parent: %v", err)
	}
	if err := promoteOCIRuntimeCacheDir(workDir, finalDir); err != nil {
		t.Fatalf("promoteOCIRuntimeCacheDir: %v", err)
	}

	if _, err := os.Stat(filepath.Join(finalDir, "rootfs", "marker")); err != nil {
		t.Fatalf("expected promoted rootfs marker: %v", err)
	}
	if _, err := os.Stat(finalDir + ".new"); !os.IsNotExist(err) {
		t.Fatalf("expected no .new dir, err=%v", err)
	}
	if _, err := os.Stat(finalDir + ".old"); !os.IsNotExist(err) {
		t.Fatalf("expected no .old dir, err=%v", err)
	}
}

func TestPromoteOCIRuntimeCacheDir_RotatesExistingEntry(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	finalDir := filepath.Join(baseDir, "runtime", "abc123")
	if err := os.MkdirAll(filepath.Join(finalDir, "rootfs"), 0o755); err != nil { //nolint:gosec // test workspace
		t.Fatalf("mkdir final rootfs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(finalDir, "rootfs", "marker"), []byte("old"), 0o644); err != nil {
		t.Fatalf("write old marker: %v", err)
	}

	workDir := filepath.Join(baseDir, "work-new")
	if err := os.MkdirAll(filepath.Join(workDir, "rootfs"), 0o755); err != nil { //nolint:gosec // test workspace
		t.Fatalf("mkdir work rootfs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "rootfs", "marker"), []byte("new"), 0o644); err != nil {
		t.Fatalf("write new marker: %v", err)
	}

	if err := promoteOCIRuntimeCacheDir(workDir, finalDir); err != nil {
		t.Fatalf("promoteOCIRuntimeCacheDir: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(finalDir, "rootfs", "marker"))
	if err != nil {
		t.Fatalf("read promoted marker: %v", err)
	}
	if string(got) != "new" {
		t.Fatalf("marker=%q, want %q", string(got), "new")
	}
	if _, err := os.Stat(finalDir + ".old"); !os.IsNotExist(err) {
		t.Fatalf("expected no .old dir, err=%v", err)
	}
}

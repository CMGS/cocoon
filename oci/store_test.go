package oci

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CMGS/cocoon/config"
)

func testConfig(t *testing.T) *config.CocoonConfig {
	t.Helper()
	tmpDir := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.RootDir = tmpDir
	cfg.RuntimeDir = filepath.Join(tmpDir, "run")
	cfg.LogDir = filepath.Join(tmpDir, "log")
	cfg.BuildahRoot = filepath.Join(tmpDir, "buildah")
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}
	return cfg
}

func TestStoreLayoutDir(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)
	store := NewStore(cfg)

	// Same tag should produce same directory.
	dir1 := store.LayoutDir("myregistry.io/ubuntu-vm:22.04")
	dir2 := store.LayoutDir("myregistry.io/ubuntu-vm:22.04")
	if dir1 != dir2 {
		t.Errorf("non-deterministic LayoutDir: %s != %s", dir1, dir2)
	}

	// Different tags should produce different directories.
	dir3 := store.LayoutDir("myregistry.io/ubuntu-vm:24.04")
	if dir1 == dir3 {
		t.Errorf("different tags produced same dir: %s", dir1)
	}

	// Should be under OCILayoutDir.
	if !strings.HasPrefix(dir1, cfg.OCILayoutDir()) {
		t.Errorf("LayoutDir not under OCILayoutDir: %s", dir1)
	}
}

func TestStoreSaveResolve(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)
	store := NewStore(cfg)

	tag := "myregistry.io/test-vm:1.0"
	layoutDir := store.LayoutDir(tag)
	os.MkdirAll(layoutDir, 0o755)

	// Save a tag.
	err := store.SaveTag(tag, layoutDir, "abc123")
	if err != nil {
		t.Fatalf("SaveTag: %v", err)
	}

	// Resolve the tag.
	resolved, err := store.ResolveTag(tag)
	if err != nil {
		t.Fatalf("ResolveTag: %v", err)
	}
	if resolved != layoutDir {
		t.Errorf("ResolveTag = %q, want %q", resolved, layoutDir)
	}

	// Resolve unknown tag should fail.
	_, err = store.ResolveTag("unknown:tag")
	if err == nil {
		t.Error("expected error for unknown tag")
	}
}

func TestStoreListTags(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)
	store := NewStore(cfg)

	// Empty list initially.
	tags, err := store.ListTags()
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if len(tags) != 0 {
		t.Errorf("expected empty list, got %d", len(tags))
	}

	// Add two tags.
	dir1 := store.LayoutDir("tag1")
	dir2 := store.LayoutDir("tag2")
	os.MkdirAll(dir1, 0o755)
	os.MkdirAll(dir2, 0o755)

	store.SaveTag("tag1", dir1, "digest1")
	store.SaveTag("tag2", dir2, "digest2")

	tags, err = store.ListTags()
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if len(tags) != 2 {
		t.Errorf("expected 2 tags, got %d", len(tags))
	}

	// Should be sorted newest first, so tag2 should be first.
	if tags[0].Tag != "tag2" {
		t.Errorf("expected tag2 first (newest), got %s", tags[0].Tag)
	}
}

func TestStoreOverwriteTag(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)
	store := NewStore(cfg)

	tag := "myregistry.io/test:latest"
	dir1 := store.LayoutDir(tag)
	os.MkdirAll(dir1, 0o755)

	store.SaveTag(tag, dir1, "digest-v1")

	// Overwrite with new digest.
	store.SaveTag(tag, dir1, "digest-v2")

	tags, err := store.ListTags()
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if len(tags) != 1 {
		t.Errorf("expected 1 tag after overwrite, got %d", len(tags))
	}
	if tags[0].ManifestDigest != "digest-v2" {
		t.Errorf("expected digest-v2, got %s", tags[0].ManifestDigest)
	}
}

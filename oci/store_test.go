package oci

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
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
	if got := len(filepath.Base(dir1)); got != 64 {
		t.Errorf("LayoutDir hash length = %d, want 64", got)
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

	entry, err := store.GetTag(tag)
	if err != nil {
		t.Fatalf("GetTag: %v", err)
	}
	if entry.Tag != tag {
		t.Errorf("GetTag.Tag = %q, want %q", entry.Tag, tag)
	}
	if entry.LayoutPath != layoutDir {
		t.Errorf("GetTag.LayoutPath = %q, want %q", entry.LayoutPath, layoutDir)
	}
	if entry.ManifestDigest != "abc123" {
		t.Errorf("GetTag.ManifestDigest = %q, want %q", entry.ManifestDigest, "abc123")
	}

	// Resolve unknown tag should fail.
	_, err = store.ResolveTag("unknown:tag")
	if err == nil {
		t.Error("expected error for unknown tag")
	}
	if _, err := store.GetTag("unknown:tag"); err == nil {
		t.Error("expected error for unknown tag in GetTag")
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

	if err := store.SaveTag("tag1", dir1, "digest1"); err != nil {
		t.Fatalf("SaveTag tag1: %v", err)
	}
	if err := store.SaveTag("tag2", dir2, "digest2"); err != nil {
		t.Fatalf("SaveTag tag2: %v", err)
	}

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

	if err := store.SaveTag(tag, dir1, "digest-v1"); err != nil {
		t.Fatalf("SaveTag digest-v1: %v", err)
	}

	// Overwrite with new digest.
	if err := store.SaveTag(tag, dir1, "digest-v2"); err != nil {
		t.Fatalf("SaveTag digest-v2: %v", err)
	}

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

func TestStoreHasTagAndRemoveMissingLayout(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)
	store := NewStore(cfg)

	tag := "myregistry.io/test:stale"
	layoutPath := store.LayoutDir("stale-layout-key")

	// Save tag entry without creating layout directory (stale index state).
	if err := store.SaveTag(tag, layoutPath, "manifest-stale"); err != nil {
		t.Fatalf("SaveTag: %v", err)
	}

	exists, err := store.HasTag(tag)
	if err != nil {
		t.Fatalf("HasTag: %v", err)
	}
	if !exists {
		t.Fatalf("HasTag(%q)=false, want true", tag)
	}

	manifest, zeroRefBlobs, err := store.RemoveTag(tag)
	if err != nil {
		t.Fatalf("RemoveTag: %v", err)
	}
	if manifest != "manifest-stale" {
		t.Fatalf("RemoveTag manifest=%q, want manifest-stale", manifest)
	}
	if len(zeroRefBlobs) != 0 {
		t.Fatalf("RemoveTag zeroRefBlobs=%v, want empty", zeroRefBlobs)
	}

	exists, err = store.HasTag(tag)
	if err != nil {
		t.Fatalf("HasTag(after remove): %v", err)
	}
	if exists {
		t.Fatalf("HasTag(%q)=true after remove, want false", tag)
	}
}

func TestStoreRemoveTagSharedManifestRefs(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)
	store := NewStore(cfg)

	manifest := "manifest-shared"
	blob := testHexDigest(301)
	if err := AddBlobRefs(cfg, manifest, []string{blob}, []int64{123}); err != nil {
		t.Fatalf("AddBlobRefs: %v", err)
	}

	dir1 := store.LayoutDir("tag-shared-1")
	dir2 := store.LayoutDir("tag-shared-2")
	if err := os.MkdirAll(dir1, 0o755); err != nil {
		t.Fatalf("MkdirAll dir1: %v", err)
	}
	if err := os.MkdirAll(dir2, 0o755); err != nil {
		t.Fatalf("MkdirAll dir2: %v", err)
	}

	if err := store.SaveTag("tag-shared-1", dir1, manifest); err != nil {
		t.Fatalf("SaveTag tag-shared-1: %v", err)
	}
	if err := store.SaveTag("tag-shared-2", dir2, manifest); err != nil {
		t.Fatalf("SaveTag tag-shared-2: %v", err)
	}

	if _, zeroRef, err := store.RemoveTag("tag-shared-1"); err != nil {
		t.Fatalf("RemoveTag tag-shared-1: %v", err)
	} else if len(zeroRef) != 0 {
		t.Fatalf("expected no zero-ref blobs while shared tag remains, got %v", zeroRef)
	}

	trackedAfterFirstRemove, err := GetAllTrackedBlobs(cfg)
	if err != nil {
		t.Fatalf("GetAllTrackedBlobs after first remove: %v", err)
	}
	if !slices.Contains(trackedAfterFirstRemove, blob) {
		t.Fatalf("expected %q to remain tracked after removing first shared tag, got %v", blob, trackedAfterFirstRemove)
	}

	if _, zeroRef, err := store.RemoveTag("tag-shared-2"); err != nil {
		t.Fatalf("RemoveTag tag-shared-2: %v", err)
	} else if !slices.Contains(zeroRef, blob) {
		t.Fatalf("expected %q to become zero-ref after removing last shared tag, got %v", blob, zeroRef)
	}
}

func TestStoreRemoveTagSharedLayoutKeepsLayoutUntilLastTag(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)
	store := NewStore(cfg)

	layoutDir := store.LayoutDir("shared-layout")
	if err := os.MkdirAll(layoutDir, 0o755); err != nil {
		t.Fatalf("MkdirAll layout: %v", err)
	}

	const manifest = "manifest-shared-layout"
	if err := store.SaveTag("tag-layout-1", layoutDir, manifest); err != nil {
		t.Fatalf("SaveTag tag-layout-1: %v", err)
	}
	if err := store.SaveTag("tag-layout-2", layoutDir, manifest); err != nil {
		t.Fatalf("SaveTag tag-layout-2: %v", err)
	}

	if _, _, err := store.RemoveTag("tag-layout-1"); err != nil {
		t.Fatalf("RemoveTag tag-layout-1: %v", err)
	}
	if _, err := os.Stat(layoutDir); err != nil {
		t.Fatalf("layout should remain while another tag still references it: %v", err)
	}
	if _, err := store.ResolveTag("tag-layout-2"); err != nil {
		t.Fatalf("ResolveTag tag-layout-2 after removing first tag: %v", err)
	}

	if _, _, err := store.RemoveTag("tag-layout-2"); err != nil {
		t.Fatalf("RemoveTag tag-layout-2: %v", err)
	}
	if _, err := os.Stat(layoutDir); !os.IsNotExist(err) {
		t.Fatalf("layout should be removed after last tag is deleted, stat err=%v", err)
	}
}

func TestStoreRemoveTag_ReferencedByVM(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)
	store := NewStore(cfg)

	// Use a valid sha256: manifest digest so the runtime-ref check fires.
	manifestDigest := "sha256:" + testHexDigest(999)
	runtimeKey := testHexDigest(999)

	layoutDir := store.LayoutDir("referenced-tag")
	if err := os.MkdirAll(layoutDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := store.SaveTag("referenced-tag", layoutDir, manifestDigest); err != nil {
		t.Fatalf("SaveTag: %v", err)
	}

	// Pin the runtime key to a VM.
	if err := AddRuntimeRef(cfg, runtimeKey, "vm-pinned"); err != nil {
		t.Fatalf("AddRuntimeRef: %v", err)
	}

	// RemoveTag must refuse while the VM still references the image.
	_, _, err := store.RemoveTag("referenced-tag")
	if err == nil {
		t.Fatal("expected error removing OCI image referenced by VM")
	}
	if !strings.Contains(err.Error(), "still referenced") {
		t.Fatalf("unexpected error: %v", err)
	}

	// After removing the VM ref, RemoveTag should succeed.
	if err := RemoveRuntimeRef(cfg, runtimeKey, "vm-pinned"); err != nil {
		t.Fatalf("RemoveRuntimeRef: %v", err)
	}
	_, _, err = store.RemoveTag("referenced-tag")
	if err != nil {
		t.Fatalf("RemoveTag after unpin: %v", err)
	}
}

func TestStoreRemoveTag_ReferencedByVM_BareHex(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)
	store := NewStore(cfg)

	// Build path stores bare hex (no "sha256:" prefix) in the tag index.
	runtimeKey := testHexDigest(998)
	bareHexManifest := runtimeKey // no prefix

	layoutDir := store.LayoutDir("bare-hex-tag")
	if err := os.MkdirAll(layoutDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := store.SaveTag("bare-hex-tag", layoutDir, bareHexManifest); err != nil {
		t.Fatalf("SaveTag: %v", err)
	}

	// Pin the runtime key to a VM.
	if err := AddRuntimeRef(cfg, runtimeKey, "vm-bare"); err != nil {
		t.Fatalf("AddRuntimeRef: %v", err)
	}

	// RemoveTag must refuse — bare hex manifest should still match the runtime key.
	_, _, err := store.RemoveTag("bare-hex-tag")
	if err == nil {
		t.Fatal("expected error removing tag with bare hex manifest referenced by VM")
	}
	if !strings.Contains(err.Error(), "still referenced") {
		t.Fatalf("unexpected error: %v", err)
	}

	// Cleanup: unpin and remove.
	if err := RemoveRuntimeRef(cfg, runtimeKey, "vm-bare"); err != nil {
		t.Fatalf("RemoveRuntimeRef: %v", err)
	}
	_, _, err = store.RemoveTag("bare-hex-tag")
	if err != nil {
		t.Fatalf("RemoveTag after unpin: %v", err)
	}
}

func TestStoreSaveTag_ReferencedByVM(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)
	store := NewStore(cfg)

	// Use a valid sha256: manifest digest so the runtime-ref check fires.
	manifestDigest := "sha256:" + testHexDigest(800)
	runtimeKey := testHexDigest(800)

	layoutDir := store.LayoutDir("save-ref-tag")
	if err := os.MkdirAll(layoutDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// Save the initial tag.
	if err := store.SaveTag("save-ref-tag", layoutDir, manifestDigest); err != nil {
		t.Fatalf("SaveTag initial: %v", err)
	}

	// Pin the runtime key to a VM.
	if err := AddRuntimeRef(cfg, runtimeKey, "vm-save-pin"); err != nil {
		t.Fatalf("AddRuntimeRef: %v", err)
	}

	// Overwriting with a different digest must fail while VM references exist.
	newDigest := "sha256:" + testHexDigest(801)
	err := store.SaveTag("save-ref-tag", layoutDir, newDigest)
	if err == nil {
		t.Fatal("expected error overwriting tag referenced by VM")
	}
	if !strings.Contains(err.Error(), "still referenced") {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the tag still has the original digest (overwrite was rejected).
	entry, err := store.GetTag("save-ref-tag")
	if err != nil {
		t.Fatalf("GetTag after rejected overwrite: %v", err)
	}
	if entry.ManifestDigest != manifestDigest {
		t.Fatalf("digest changed despite rejection: got %s, want %s", entry.ManifestDigest, manifestDigest)
	}

	// Remove the VM ref, then overwrite should succeed.
	if err := RemoveRuntimeRef(cfg, runtimeKey, "vm-save-pin"); err != nil {
		t.Fatalf("RemoveRuntimeRef: %v", err)
	}
	if err := store.SaveTag("save-ref-tag", layoutDir, newDigest); err != nil {
		t.Fatalf("SaveTag after unpin: %v", err)
	}

	// Verify overwrite succeeded.
	entry, err = store.GetTag("save-ref-tag")
	if err != nil {
		t.Fatalf("GetTag after overwrite: %v", err)
	}
	if entry.ManifestDigest != newDigest {
		t.Fatalf("digest not updated: got %s, want %s", entry.ManifestDigest, newDigest)
	}
}

func TestStoreSaveTag_CleanupFailureCounter(t *testing.T) {
	cfg := testConfig(t)
	store := NewStore(cfg)

	layoutDir := store.LayoutDir("cleanup-counter")
	if err := os.MkdirAll(layoutDir, 0o755); err != nil {
		t.Fatalf("MkdirAll layout: %v", err)
	}

	store.removeBlobRefsFn = func(*config.CocoonConfig, string) ([]string, error) {
		return nil, errors.New("boom")
	}

	if err := store.SaveTag("cleanup-counter", layoutDir, "digest-v1"); err != nil {
		t.Fatalf("SaveTag digest-v1: %v", err)
	}

	before := BlobRefCleanupFailureCount()
	if err := store.SaveTag("cleanup-counter", layoutDir, "digest-v2"); err != nil {
		t.Fatalf("SaveTag digest-v2: %v", err)
	}
	after := BlobRefCleanupFailureCount()
	if after != before+1 {
		t.Fatalf("BlobRefCleanupFailureCount delta=%d, want 1", after-before)
	}
}

func TestStoreSaveTag_OverwriteCleansOldLayoutAndBlobs(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)
	store := NewStore(cfg)

	oldManifest := "sha256:" + testHexDigest(900)
	newManifest := "sha256:" + testHexDigest(901)
	oldBlob := testHexDigest(902)

	// Create old layout directory.
	oldLayoutDir := store.LayoutDir("overwrite-cleanup-old")
	if err := os.MkdirAll(oldLayoutDir, 0o755); err != nil {
		t.Fatalf("MkdirAll old layout: %v", err)
	}
	// Register blob refs for the old manifest.
	if err := AddBlobRefs(cfg, oldManifest, []string{oldBlob}, []int64{100}); err != nil {
		t.Fatalf("AddBlobRefs: %v", err)
	}
	// Create the actual blob file in shared store.
	blobStore := NewBlobStore(cfg)
	blobPath := blobStore.blobPath(oldBlob)
	if err := os.MkdirAll(cfg.OCIBlobDir(), 0o755); err != nil {
		t.Fatalf("MkdirAll blob dir: %v", err)
	}
	if err := os.WriteFile(blobPath, []byte("fake-blob"), 0o644); err != nil {
		t.Fatalf("write blob: %v", err)
	}

	// Save initial tag.
	if err := store.SaveTag("overwrite-cleanup", oldLayoutDir, oldManifest); err != nil {
		t.Fatalf("SaveTag initial: %v", err)
	}

	// Overwrite with new manifest and new layout.
	newLayoutDir := store.LayoutDir("overwrite-cleanup-new")
	if err := os.MkdirAll(newLayoutDir, 0o755); err != nil {
		t.Fatalf("MkdirAll new layout: %v", err)
	}
	if err := store.SaveTag("overwrite-cleanup", newLayoutDir, newManifest); err != nil {
		t.Fatalf("SaveTag overwrite: %v", err)
	}

	// Old layout directory should be removed.
	if _, err := os.Stat(oldLayoutDir); !os.IsNotExist(err) {
		t.Fatalf("old layout dir should be removed, stat err=%v", err)
	}
	// Old blob file should be removed (zero refs after overwrite).
	if _, err := os.Stat(blobPath); !os.IsNotExist(err) {
		t.Fatalf("old blob should be removed, stat err=%v", err)
	}
	// New layout should still exist.
	if _, err := os.Stat(newLayoutDir); err != nil {
		t.Fatalf("new layout dir should exist: %v", err)
	}
}

func TestStoreCheckTagOverwriteSafe(t *testing.T) {
	t.Parallel()

	t.Run("non-existent tag returns nil", func(t *testing.T) {
		t.Parallel()
		cfg := testConfig(t)
		store := NewStore(cfg)

		if err := store.CheckTagOverwriteSafe("does-not-exist:latest"); err != nil {
			t.Fatalf("expected nil for non-existent tag, got: %v", err)
		}
	})

	t.Run("existing tag with no VM refs returns nil", func(t *testing.T) {
		t.Parallel()
		cfg := testConfig(t)
		store := NewStore(cfg)

		// Use a valid sha256: digest so the runtime-ref lookup path fires.
		manifestDigest := "sha256:" + testHexDigest(700)
		layoutDir := store.LayoutDir("safe-tag")
		if err := os.MkdirAll(layoutDir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := store.SaveTag("safe-tag", layoutDir, manifestDigest); err != nil {
			t.Fatalf("SaveTag: %v", err)
		}

		if err := store.CheckTagOverwriteSafe("safe-tag"); err != nil {
			t.Fatalf("expected nil for unreferenced tag, got: %v", err)
		}
	})

	t.Run("existing tag with VM refs returns error", func(t *testing.T) {
		t.Parallel()
		cfg := testConfig(t)
		store := NewStore(cfg)

		manifestDigest := "sha256:" + testHexDigest(701)
		runtimeKey := testHexDigest(701)
		layoutDir := store.LayoutDir("pinned-tag")
		if err := os.MkdirAll(layoutDir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := store.SaveTag("pinned-tag", layoutDir, manifestDigest); err != nil {
			t.Fatalf("SaveTag: %v", err)
		}

		// Pin the runtime key to a VM.
		if err := AddRuntimeRef(cfg, runtimeKey, "vm-check-safe"); err != nil {
			t.Fatalf("AddRuntimeRef: %v", err)
		}

		err := store.CheckTagOverwriteSafe("pinned-tag")
		if err == nil {
			t.Fatal("expected error for tag referenced by VM")
		}
		if !strings.Contains(err.Error(), "still referenced") {
			t.Fatalf("expected 'still referenced' in error, got: %v", err)
		}

		// After removing the VM ref, check should pass.
		if err := RemoveRuntimeRef(cfg, runtimeKey, "vm-check-safe"); err != nil {
			t.Fatalf("RemoveRuntimeRef: %v", err)
		}
		if err := store.CheckTagOverwriteSafe("pinned-tag"); err != nil {
			t.Fatalf("expected nil after unpin, got: %v", err)
		}
	})
}

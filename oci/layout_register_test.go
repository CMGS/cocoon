package oci

import (
	"os"
	"slices"
	"testing"
)

func TestCleanupManifestRefsIfUnreferenced(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)
	store := NewStore(cfg)

	manifest := "manifest-cleanup"
	blob := testHexDigest(401)
	if err := AddBlobRefs(cfg, manifest, []string{blob}, []int64{10}); err != nil {
		t.Fatalf("AddBlobRefs: %v", err)
	}

	// No tag references this manifest: cleanup should remove refs.
	if err := store.withTxnLock(func() error {
		return store.cleanupManifestRefsIfUnreferencedTxnLocked(manifest)
	}); err != nil {
		t.Fatalf("cleanupManifestRefsIfUnreferenced (unreferenced): %v", err)
	}
	tracked, err := GetAllTrackedBlobs(cfg)
	if err != nil {
		t.Fatalf("GetAllTrackedBlobs after unreferenced cleanup: %v", err)
	}
	if slices.Contains(tracked, blob) {
		t.Fatalf("expected %q to be removed after unreferenced cleanup, tracked=%v", blob, tracked)
	}

	// Re-add refs, add a referencing tag, and ensure cleanup does not remove.
	if err := AddBlobRefs(cfg, manifest, []string{blob}, []int64{10}); err != nil {
		t.Fatalf("AddBlobRefs (re-add): %v", err)
	}
	tag := "tag-manifest-cleanup"
	dir := store.LayoutDir(tag)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll layout: %v", err)
	}
	if err := store.SaveTag(tag, dir, manifest); err != nil {
		t.Fatalf("SaveTag: %v", err)
	}

	if err := store.withTxnLock(func() error {
		return store.cleanupManifestRefsIfUnreferencedTxnLocked(manifest)
	}); err != nil {
		t.Fatalf("cleanupManifestRefsIfUnreferenced (referenced): %v", err)
	}
	tracked, err = GetAllTrackedBlobs(cfg)
	if err != nil {
		t.Fatalf("GetAllTrackedBlobs after referenced cleanup: %v", err)
	}
	if !slices.Contains(tracked, blob) {
		t.Fatalf("expected %q to remain tracked while manifest is still referenced, tracked=%v", blob, tracked)
	}
}

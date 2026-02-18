package local

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/CMGS/cocoon/config"
)

// --- OCI GC tests ---

// createFakeOCILayout creates a minimal OCI layout directory with an index.json.
func createFakeOCILayout(t *testing.T, layoutsDir, name string, mtime time.Time) string {
	t.Helper()
	dir := filepath.Join(layoutsDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// Write a minimal index.json so the directory looks like a layout.
	if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chtimes(dir, mtime, mtime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	return dir
}

// createFakeBlob creates a fake blob file in the blob store directory.
func createFakeBlob(t *testing.T, blobDir, digest string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(blobDir, digest)
	if err := os.WriteFile(path, []byte("FAKE-BLOB-"+digest), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
}

// writeTagIndex writes a minimal OCI tag index JSON to the config's tag index path.
func writeTagIndex(t *testing.T, cfg testableConfig, entries map[string]ociTagEntry) {
	t.Helper()
	idx := ociTagIndex{Tags: make(map[string]ociTagEntry)}
	for tag, entry := range entries {
		idx.Tags[tag] = entry
	}
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		t.Fatalf("marshal tag index: %v", err)
	}
	path := cfg.cfg.OCIBuildTagIndex()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// writeLayerRefs writes a minimal OCI layer refs index JSON.
func writeLayerRefs(t *testing.T, cfg testableConfig, blobs map[string][]string) {
	t.Helper()
	idx := ociLayerRefsIndex{Blobs: make(map[string]ociBlobRefEntry)}
	for digest, manifests := range blobs {
		idx.Blobs[digest] = ociBlobRefEntry{
			ManifestDigests: manifests,
			Size:            100,
			CreatedAt:       time.Now().Add(-1 * time.Hour),
		}
	}
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		t.Fatalf("marshal layer refs: %v", err)
	}
	path := cfg.cfg.OCILayerRefsFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

type testableConfig struct {
	cfg *config.CocoonConfig
}

// --- CollectOrphanedOCILayouts tests ---

func TestOCIGC_CollectOrphanedLayouts_Empty(t *testing.T) {
	cfg := newTestConfig(t)
	gc := NewGarbageCollector(cfg)

	collected, err := gc.CollectOrphanedOCILayouts()
	if err != nil {
		t.Fatalf("CollectOrphanedOCILayouts: %v", err)
	}
	if len(collected) != 0 {
		t.Errorf("expected 0 collected, got %d", len(collected))
	}
}

func TestOCIGC_CollectOrphanedLayouts_RemovesOrphans(t *testing.T) {
	cfg := newTestConfig(t)
	gc := NewGarbageCollector(cfg)

	layoutsDir := cfg.OCILayoutDir()
	oldTime := time.Now().Add(-10 * time.Minute) // older than grace period

	// Create two layouts: one referenced, one orphaned.
	referencedDir := createFakeOCILayout(t, layoutsDir, "referenced-layout", oldTime)
	createFakeOCILayout(t, layoutsDir, "orphaned-layout", oldTime)

	// Write tag index referencing only the first layout.
	tc := testableConfig{cfg: cfg}
	writeTagIndex(t, tc, map[string]ociTagEntry{
		"my-tag": {LayoutPath: referencedDir, ManifestDigest: "sha256:fake"},
	})

	collected, err := gc.CollectOrphanedOCILayouts()
	if err != nil {
		t.Fatalf("CollectOrphanedOCILayouts: %v", err)
	}
	if len(collected) != 1 {
		t.Fatalf("expected 1 collected, got %d: %v", len(collected), collected)
	}
	if collected[0] != "orphaned-layout" {
		t.Errorf("expected orphaned-layout, got %s", collected[0])
	}

	// Verify referenced layout still exists.
	if _, err := os.Stat(referencedDir); err != nil {
		t.Errorf("referenced layout should still exist: %v", err)
	}
}

func TestOCIGC_CollectOrphanedLayouts_GracePeriod(t *testing.T) {
	cfg := newTestConfig(t)
	gc := NewGarbageCollector(cfg)

	layoutsDir := cfg.OCILayoutDir()

	// Create an orphaned layout that is recent (within grace period).
	createFakeOCILayout(t, layoutsDir, "recent-orphan", time.Now())

	// No tags at all -- the layout is orphaned but recent.
	tc := testableConfig{cfg: cfg}
	writeTagIndex(t, tc, map[string]ociTagEntry{})

	collected, err := gc.CollectOrphanedOCILayouts()
	if err != nil {
		t.Fatalf("CollectOrphanedOCILayouts: %v", err)
	}
	if len(collected) != 0 {
		t.Errorf("expected 0 collected (grace period), got %d: %v", len(collected), collected)
	}
}

// --- CollectUnreferencedOCIBlobs tests ---

func TestOCIGC_CollectUnreferencedBlobs_Empty(t *testing.T) {
	cfg := newTestConfig(t)
	gc := NewGarbageCollector(cfg)

	collected, err := gc.CollectUnreferencedOCIBlobs()
	if err != nil {
		t.Fatalf("CollectUnreferencedOCIBlobs: %v", err)
	}
	if len(collected) != 0 {
		t.Errorf("expected 0 collected, got %d", len(collected))
	}
}

func TestOCIGC_CollectUnreferencedBlobs_RemovesUnreferenced(t *testing.T) {
	cfg := newTestConfig(t)
	gc := NewGarbageCollector(cfg)

	blobDir := cfg.OCIBlobDir()
	oldTime := time.Now().Add(-10 * time.Minute) // older than grace period

	// Create two blobs: one referenced, one unreferenced.
	createFakeBlob(t, blobDir, "aaaa1111", oldTime)
	createFakeBlob(t, blobDir, "bbbb2222", oldTime)

	// Write layer refs: only aaaa1111 is referenced.
	tc := testableConfig{cfg: cfg}
	writeLayerRefs(t, tc, map[string][]string{
		"aaaa1111": {"manifest-abc"},
		"bbbb2222": {}, // zero refs
	})

	collected, err := gc.CollectUnreferencedOCIBlobs()
	if err != nil {
		t.Fatalf("CollectUnreferencedOCIBlobs: %v", err)
	}
	if len(collected) != 1 {
		t.Fatalf("expected 1 collected, got %d: %v", len(collected), collected)
	}
	if collected[0] != "bbbb2222" {
		t.Errorf("expected bbbb2222, got %s", collected[0])
	}

	// Verify referenced blob still exists.
	if _, err := os.Stat(filepath.Join(blobDir, "aaaa1111")); err != nil {
		t.Errorf("referenced blob should still exist: %v", err)
	}
}

func TestOCIGC_CollectUnreferencedBlobs_GracePeriod(t *testing.T) {
	cfg := newTestConfig(t)
	gc := NewGarbageCollector(cfg)

	blobDir := cfg.OCIBlobDir()

	// Create an unreferenced blob that is recent (within grace period).
	createFakeBlob(t, blobDir, "cccc3333", time.Now())

	// Write layer refs with zero manifest refs.
	tc := testableConfig{cfg: cfg}
	writeLayerRefs(t, tc, map[string][]string{
		"cccc3333": {},
	})

	collected, err := gc.CollectUnreferencedOCIBlobs()
	if err != nil {
		t.Fatalf("CollectUnreferencedOCIBlobs: %v", err)
	}
	if len(collected) != 0 {
		t.Errorf("expected 0 collected (grace period), got %d: %v", len(collected), collected)
	}
}

func TestOCIGC_CollectUnreferencedBlobs_UntrackedBlob(t *testing.T) {
	cfg := newTestConfig(t)
	gc := NewGarbageCollector(cfg)

	blobDir := cfg.OCIBlobDir()
	oldTime := time.Now().Add(-10 * time.Minute)

	// Create a blob that has no entry in layer refs at all (untracked).
	createFakeBlob(t, blobDir, "dddd4444", oldTime)

	// Empty layer refs.
	tc := testableConfig{cfg: cfg}
	writeLayerRefs(t, tc, map[string][]string{})

	collected, err := gc.CollectUnreferencedOCIBlobs()
	if err != nil {
		t.Fatalf("CollectUnreferencedOCIBlobs: %v", err)
	}
	if len(collected) != 1 {
		t.Fatalf("expected 1 collected (untracked blob), got %d: %v", len(collected), collected)
	}
	if collected[0] != "dddd4444" {
		t.Errorf("expected dddd4444, got %s", collected[0])
	}
}

// --- CollectStaleOCITags tests ---

func TestOCIGC_CollectStaleOCITags_Empty(t *testing.T) {
	cfg := newTestConfig(t)
	gc := NewGarbageCollector(cfg)

	collected, err := gc.CollectStaleOCITags()
	if err != nil {
		t.Fatalf("CollectStaleOCITags: %v", err)
	}
	if len(collected) != 0 {
		t.Errorf("expected 0 collected, got %d", len(collected))
	}
}

func TestOCIGC_CollectStaleOCITags_RemovesStale(t *testing.T) {
	cfg := newTestConfig(t)
	gc := NewGarbageCollector(cfg)

	layoutsDir := cfg.OCILayoutDir()
	oldTime := time.Now().Add(-10 * time.Minute)

	// Create one layout that exists, and reference a missing layout.
	existingLayout := createFakeOCILayout(t, layoutsDir, "existing-layout", oldTime)
	missingLayoutPath := filepath.Join(layoutsDir, "missing-layout")

	tc := testableConfig{cfg: cfg}
	writeTagIndex(t, tc, map[string]ociTagEntry{
		"live-tag":  {LayoutPath: existingLayout, ManifestDigest: "sha256:live"},
		"stale-tag": {LayoutPath: missingLayoutPath, ManifestDigest: "sha256:stale"},
	})

	collected, err := gc.CollectStaleOCITags()
	if err != nil {
		t.Fatalf("CollectStaleOCITags: %v", err)
	}
	if len(collected) != 1 {
		t.Fatalf("expected 1 collected, got %d: %v", len(collected), collected)
	}
	if collected[0] != "stale-tag" {
		t.Errorf("expected stale-tag, got %s", collected[0])
	}
}

func TestOCIGC_CollectStaleOCITags_SharedManifest(t *testing.T) {
	cfg := newTestConfig(t)
	gc := NewGarbageCollector(cfg)

	layoutsDir := cfg.OCILayoutDir()
	oldTime := time.Now().Add(-10 * time.Minute)

	// Two tags share the same manifest digest, but one has a missing layout.
	existingLayout := createFakeOCILayout(t, layoutsDir, "existing-layout", oldTime)
	missingLayoutPath := filepath.Join(layoutsDir, "missing-layout")

	tc := testableConfig{cfg: cfg}
	writeTagIndex(t, tc, map[string]ociTagEntry{
		"live-tag":  {LayoutPath: existingLayout, ManifestDigest: "sha256:shared"},
		"stale-tag": {LayoutPath: missingLayoutPath, ManifestDigest: "sha256:shared"},
	})

	// Write layer refs with a blob referencing the shared manifest.
	writeLayerRefs(t, tc, map[string][]string{
		"blob-aaa": {"sha256:shared"},
	})

	collected, err := gc.CollectStaleOCITags()
	if err != nil {
		t.Fatalf("CollectStaleOCITags: %v", err)
	}
	if len(collected) != 1 {
		t.Fatalf("expected 1 stale tag, got %d: %v", len(collected), collected)
	}

	// The shared manifest should NOT be cleaned up because live-tag still uses it.
	// Verify blob still has the manifest reference.
	var layerRefs ociLayerRefsIndex
	data, err := os.ReadFile(cfg.OCILayerRefsFile())
	if err != nil {
		t.Fatalf("read layer refs: %v", err)
	}
	if err := json.Unmarshal(data, &layerRefs); err != nil {
		t.Fatalf("unmarshal layer refs: %v", err)
	}

	blobEntry, ok := layerRefs.Blobs["blob-aaa"]
	if !ok {
		t.Fatal("blob-aaa should still be in layer refs")
	}
	if len(blobEntry.ManifestDigests) != 1 || blobEntry.ManifestDigests[0] != "sha256:shared" {
		t.Errorf("blob-aaa manifest digests = %v, want [sha256:shared]", blobEntry.ManifestDigests)
	}
}

func TestOCIGC_CollectStaleOCITags_CascadesBlobCleanup(t *testing.T) {
	cfg := newTestConfig(t)
	gc := NewGarbageCollector(cfg)

	layoutsDir := cfg.OCILayoutDir()
	blobDir := cfg.OCIBlobDir()
	oldTime := time.Now().Add(-10 * time.Minute)

	// Create a live layout and a missing layout.
	existingLayout := createFakeOCILayout(t, layoutsDir, "existing-layout", oldTime)
	missingLayoutPath := filepath.Join(layoutsDir, "missing-layout")

	tc := testableConfig{cfg: cfg}
	writeTagIndex(t, tc, map[string]ociTagEntry{
		"live-tag":  {LayoutPath: existingLayout, ManifestDigest: "sha256:live-manifest"},
		"stale-tag": {LayoutPath: missingLayoutPath, ManifestDigest: "sha256:stale-manifest"},
	})

	// Create blobs: one referenced by live manifest, one only by stale manifest.
	createFakeBlob(t, blobDir, "blob-live", oldTime)
	createFakeBlob(t, blobDir, "blob-stale", oldTime)
	writeLayerRefs(t, tc, map[string][]string{
		"blob-live":  {"sha256:live-manifest"},
		"blob-stale": {"sha256:stale-manifest"},
	})

	collected, err := gc.CollectStaleOCITags()
	if err != nil {
		t.Fatalf("CollectStaleOCITags: %v", err)
	}
	if len(collected) != 1 {
		t.Fatalf("expected 1 stale tag, got %d: %v", len(collected), collected)
	}

	// blob-stale should be deleted (its only manifest ref was orphaned).
	if _, err := os.Stat(filepath.Join(blobDir, "blob-stale")); !os.IsNotExist(err) {
		t.Error("blob-stale should be deleted after cascade cleanup")
	}
	// blob-live should still exist.
	if _, err := os.Stat(filepath.Join(blobDir, "blob-live")); err != nil {
		t.Errorf("blob-live should still exist: %v", err)
	}
}

// --- CollectOrphanedOCIManifestRefs tests ---

func TestOCIGC_CollectOrphanedManifestRefs_Empty(t *testing.T) {
	cfg := newTestConfig(t)
	gc := NewGarbageCollector(cfg)

	collected, err := gc.CollectOrphanedOCIManifestRefs()
	if err != nil {
		t.Fatalf("CollectOrphanedOCIManifestRefs: %v", err)
	}
	if len(collected) != 0 {
		t.Errorf("expected 0 collected, got %d", len(collected))
	}
}

func TestOCIGC_CollectOrphanedManifestRefs_CleansOrphans(t *testing.T) {
	cfg := newTestConfig(t)
	gc := NewGarbageCollector(cfg)

	layoutsDir := cfg.OCILayoutDir()
	blobDir := cfg.OCIBlobDir()
	oldTime := time.Now().Add(-10 * time.Minute)

	// Create a live tag with an existing layout.
	existingLayout := createFakeOCILayout(t, layoutsDir, "existing-layout", oldTime)

	tc := testableConfig{cfg: cfg}
	writeTagIndex(t, tc, map[string]ociTagEntry{
		"live-tag": {LayoutPath: existingLayout, ManifestDigest: "sha256:live-manifest"},
	})

	// Create blobs with an orphaned manifest ref.
	createFakeBlob(t, blobDir, "blob-shared", oldTime)
	writeLayerRefs(t, tc, map[string][]string{
		"blob-shared": {"sha256:live-manifest", "sha256:orphaned-manifest"},
	})

	collected, err := gc.CollectOrphanedOCIManifestRefs()
	if err != nil {
		t.Fatalf("CollectOrphanedOCIManifestRefs: %v", err)
	}
	if len(collected) != 1 {
		t.Fatalf("expected 1 orphaned manifest, got %d: %v", len(collected), collected)
	}

	// blob-shared should still exist because it still has a live manifest ref.
	if _, err := os.Stat(filepath.Join(blobDir, "blob-shared")); err != nil {
		t.Errorf("blob-shared should still exist: %v", err)
	}

	// Verify the orphaned manifest was removed from blob refs.
	var layerRefs ociLayerRefsIndex
	data, err := os.ReadFile(cfg.OCILayerRefsFile())
	if err != nil {
		t.Fatalf("read layer refs: %v", err)
	}
	if err := json.Unmarshal(data, &layerRefs); err != nil {
		t.Fatalf("unmarshal layer refs: %v", err)
	}

	blobEntry, ok := layerRefs.Blobs["blob-shared"]
	if !ok {
		t.Fatal("blob-shared should still be in layer refs")
	}
	if len(blobEntry.ManifestDigests) != 1 || blobEntry.ManifestDigests[0] != "sha256:live-manifest" {
		t.Errorf("blob-shared manifest digests = %v, want [sha256:live-manifest]", blobEntry.ManifestDigests)
	}
}

func TestOCIGC_CollectOrphanedManifestRefs_GracePeriod(t *testing.T) {
	cfg := newTestConfig(t)
	gc := NewGarbageCollector(cfg)

	blobDir := cfg.OCIBlobDir()

	// No tags at all (all manifests are orphaned).
	tc := testableConfig{cfg: cfg}
	writeTagIndex(t, tc, map[string]ociTagEntry{})

	// Create a recent blob referencing an orphaned manifest.
	createFakeBlob(t, blobDir, "recent-blob", time.Now())
	writeLayerRefs(t, tc, map[string][]string{
		"recent-blob": {"sha256:orphaned-manifest"},
	})

	collected, err := gc.CollectOrphanedOCIManifestRefs()
	if err != nil {
		t.Fatalf("CollectOrphanedOCIManifestRefs: %v", err)
	}

	// The orphaned manifest should be collected (manifest ref is removed).
	if len(collected) != 1 {
		t.Fatalf("expected 1 orphaned manifest, got %d: %v", len(collected), collected)
	}

	// But the blob file should be preserved due to grace period (it's recent).
	if _, err := os.Stat(filepath.Join(blobDir, "recent-blob")); err != nil {
		t.Errorf("recent blob should be preserved by grace period: %v", err)
	}
}

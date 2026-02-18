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
func writeTagIndex(t *testing.T, cfg testableConfig, entries map[string]string) {
	t.Helper()
	idx := ociTagIndex{Tags: make(map[string]ociTagEntry)}
	for tag, layoutPath := range entries {
		idx.Tags[tag] = ociTagEntry{LayoutPath: layoutPath, ManifestDigest: "sha256:fake"}
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
	writeTagIndex(t, tc, map[string]string{
		"my-tag": referencedDir,
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
	writeTagIndex(t, tc, map[string]string{})

	collected, err := gc.CollectOrphanedOCILayouts()
	if err != nil {
		t.Fatalf("CollectOrphanedOCILayouts: %v", err)
	}
	if len(collected) != 0 {
		t.Errorf("expected 0 collected (grace period), got %d: %v", len(collected), collected)
	}
}

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

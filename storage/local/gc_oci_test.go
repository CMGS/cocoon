package local

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/lock/flock"
	"github.com/CMGS/cocoon/oci"
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
func writeTagIndex(t *testing.T, cfg testableConfig, entries map[string]oci.TagEntry) {
	t.Helper()
	idx := oci.TagIndex{Tags: make(map[string]oci.TagEntry)}
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
	idx := oci.LayerRefsIndex{Blobs: make(map[string]oci.BlobRefEntry)}
	for digest, manifests := range blobs {
		idx.Blobs[digest] = oci.BlobRefEntry{
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

func writeRuntimeRefs(t *testing.T, cfg testableConfig, refs map[string][]string) {
	t.Helper()
	idx := oci.RuntimeRefsIndex{Runtimes: make(map[string]oci.RuntimeRefEntry)}
	for runtimeKey, vmIDs := range refs {
		idx.Runtimes[runtimeKey] = oci.RuntimeRefEntry{
			Refs:      vmIDs,
			CreatedAt: time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339),
		}
	}
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		t.Fatalf("marshal runtime refs: %v", err)
	}
	path := cfg.cfg.OCIRuntimeRefsFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func writeRuntimeEntryMeta(t *testing.T, cfg *config.CocoonConfig, runtimeKey string) {
	t.Helper()
	const (
		kernelDigest = "1111111111111111111111111111111111111111111111111111111111111111"
		rootfsDigest = "2222222222222222222222222222222222222222222222222222222222222222"
	)

	entryDir := cfg.OCIRuntimeEntryDir(runtimeKey)
	if err := os.MkdirAll(entryDir, 0o755); err != nil {
		t.Fatalf("MkdirAll entry dir: %v", err)
	}
	meta := &oci.OCIRuntimeEntryMeta{
		ManifestDigest:     "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		KernelLayerDigest:  kernelDigest,
		RootfsLayerDigests: []string{rootfsDigest},
		Arch:               "amd64",
		VirtioFSTag:        "cocoon-rootfs",
	}
	if err := oci.WriteEntryMeta(cfg.OCIRuntimeEntryMetaPath(runtimeKey), meta); err != nil {
		t.Fatalf("write entry meta: %v", err)
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
	writeTagIndex(t, tc, map[string]oci.TagEntry{
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
	writeTagIndex(t, tc, map[string]oci.TagEntry{})

	collected, err := gc.CollectOrphanedOCILayouts()
	if err != nil {
		t.Fatalf("CollectOrphanedOCILayouts: %v", err)
	}
	if len(collected) != 0 {
		t.Errorf("expected 0 collected (grace period), got %d: %v", len(collected), collected)
	}
}

func TestOCIGC_CollectOrphanedLayouts_WaitsForTxnLock(t *testing.T) {
	cfg := newTestConfig(t)
	gc := NewGarbageCollector(cfg)

	layoutsDir := cfg.OCILayoutDir()
	oldTime := time.Now().Add(-10 * time.Minute)
	createFakeOCILayout(t, layoutsDir, "txn-blocked-orphan", oldTime)
	writeTagIndex(t, testableConfig{cfg: cfg}, map[string]oci.TagEntry{})

	// Hold txn lock to simulate an in-progress build finalization.
	txnLock := flock.New(cfg.OCIBuildTxnLock())
	if err := txnLock.Lock(); err != nil {
		t.Fatalf("lock txn: %v", err)
	}

	type result struct {
		collected []string
		err       error
	}
	done := make(chan result, 1)
	go func() {
		collected, err := gc.CollectOrphanedOCILayouts()
		done <- result{collected: collected, err: err}
	}()

	select {
	case r := <-done:
		t.Fatalf("CollectOrphanedOCILayouts returned while txn lock held: collected=%v err=%v", r.collected, r.err)
	case <-time.After(200 * time.Millisecond):
		// Expected: blocked on txn lock.
	}

	if err := txnLock.Unlock(); err != nil {
		t.Fatalf("unlock txn: %v", err)
	}

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("CollectOrphanedOCILayouts: %v", r.err)
		}
		if len(r.collected) != 1 || r.collected[0] != "txn-blocked-orphan" {
			t.Fatalf("collected=%v, want [txn-blocked-orphan]", r.collected)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CollectOrphanedOCILayouts did not complete after txn lock release")
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
	writeTagIndex(t, tc, map[string]oci.TagEntry{
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
	writeTagIndex(t, tc, map[string]oci.TagEntry{
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
	var layerRefs oci.LayerRefsIndex
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
	writeTagIndex(t, tc, map[string]oci.TagEntry{
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

func TestOCIGC_CollectStaleOCITags_CascadeRespectsBlobGracePeriod(t *testing.T) {
	cfg := newTestConfig(t)
	gc := NewGarbageCollector(cfg)

	layoutsDir := cfg.OCILayoutDir()
	blobDir := cfg.OCIBlobDir()
	oldTime := time.Now().Add(-10 * time.Minute)

	// Create a live layout and a missing layout.
	existingLayout := createFakeOCILayout(t, layoutsDir, "existing-layout", oldTime)
	missingLayoutPath := filepath.Join(layoutsDir, "missing-layout")

	tc := testableConfig{cfg: cfg}
	writeTagIndex(t, tc, map[string]oci.TagEntry{
		"live-tag":  {LayoutPath: existingLayout, ManifestDigest: "sha256:live-manifest"},
		"stale-tag": {LayoutPath: missingLayoutPath, ManifestDigest: "sha256:stale-manifest"},
	})

	// Create a RECENT blob (within 5-min grace period) only referenced by the stale manifest.
	createFakeBlob(t, blobDir, "recent-blob", time.Now())
	writeLayerRefs(t, tc, map[string][]string{
		"recent-blob": {"sha256:stale-manifest"},
	})

	collected, err := gc.CollectStaleOCITags()
	if err != nil {
		t.Fatalf("CollectStaleOCITags: %v", err)
	}
	if len(collected) != 1 {
		t.Fatalf("expected 1 stale tag, got %d: %v", len(collected), collected)
	}

	// The blob file should be preserved because it's within the grace period,
	// even though its manifest ref was removed.
	if _, err := os.Stat(filepath.Join(blobDir, "recent-blob")); err != nil {
		t.Errorf("recent blob should be preserved by grace period: %v", err)
	}

	// The layer-refs entry should be kept (zero-ref but file preserved by grace).
	// Phase 6 will handle final cleanup once the grace period expires.
	var layerRefs oci.LayerRefsIndex
	data, err := os.ReadFile(cfg.OCILayerRefsFile())
	if err != nil {
		t.Fatalf("read layer refs: %v", err)
	}
	if err := json.Unmarshal(data, &layerRefs); err != nil {
		t.Fatalf("unmarshal layer refs: %v", err)
	}
	blobEntry, ok := layerRefs.Blobs["recent-blob"]
	if !ok {
		t.Fatal("recent-blob layer-refs entry should be kept (file preserved by grace)")
	}
	if len(blobEntry.ManifestDigests) != 0 {
		t.Errorf("recent-blob manifest digests should be empty, got %v", blobEntry.ManifestDigests)
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
	writeTagIndex(t, tc, map[string]oci.TagEntry{
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
	var layerRefs oci.LayerRefsIndex
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
	writeTagIndex(t, tc, map[string]oci.TagEntry{})

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

	// The layer-refs entry should be kept (zero-ref but file preserved by grace).
	var layerRefs oci.LayerRefsIndex
	data, err := os.ReadFile(cfg.OCILayerRefsFile())
	if err != nil {
		t.Fatalf("read layer refs: %v", err)
	}
	if err := json.Unmarshal(data, &layerRefs); err != nil {
		t.Fatalf("unmarshal layer refs: %v", err)
	}
	blobEntry, ok := layerRefs.Blobs["recent-blob"]
	if !ok {
		t.Fatal("recent-blob layer-refs entry should be kept (file preserved by grace)")
	}
	if len(blobEntry.ManifestDigests) != 0 {
		t.Errorf("recent-blob manifest digests should be empty, got %v", blobEntry.ManifestDigests)
	}
}

func TestOCIGC_CollectUnreferencedOCIRuntimeCaches_RemovesUnpinned(t *testing.T) {
	cfg := newTestConfig(t)
	gc, ok := NewGarbageCollector(cfg).(*fileGarbageCollector)
	if !ok {
		t.Fatal("type assertion to *fileGarbageCollector failed")
	}

	oldTime := time.Now().Add(-10 * time.Minute)

	// Create entry dirs under entries/.
	pinnedEntry := cfg.OCIRuntimeEntryDir("1111222233334444")
	unpinnedEntry := cfg.OCIRuntimeEntryDir("aaaa2222bbbb3333")
	if err := os.MkdirAll(pinnedEntry, 0o755); err != nil {
		t.Fatalf("mkdir pinned entry: %v", err)
	}
	if err := os.MkdirAll(unpinnedEntry, 0o755); err != nil {
		t.Fatalf("mkdir unpinned entry: %v", err)
	}
	writeRuntimeEntryMeta(t, cfg, "1111222233334444")
	_ = os.Chtimes(pinnedEntry, oldTime, oldTime)
	_ = os.Chtimes(unpinnedEntry, oldTime, oldTime)

	tc := testableConfig{cfg: cfg}
	writeRuntimeRefs(t, tc, map[string][]string{
		"1111222233334444": {"vm-01HF00PINNED000000000000"},
	})

	collected, err := gc.CollectUnreferencedOCIRuntimeCaches()
	if err != nil {
		t.Fatalf("CollectUnreferencedOCIRuntimeCaches: %v", err)
	}
	if len(collected) != 1 || collected[0] != "entry:aaaa2222bbbb3333" {
		t.Fatalf("collected=%v, want [entry:aaaa2222bbbb3333]", collected)
	}
	if _, err := os.Stat(unpinnedEntry); !os.IsNotExist(err) {
		t.Fatalf("unpinned entry dir should be removed, stat err=%v", err)
	}
	if _, err := os.Stat(pinnedEntry); err != nil {
		t.Fatalf("pinned entry dir should remain: %v", err)
	}
}

func TestOCIGC_CollectUnreferencedOCIRuntimeCaches_GracePeriod(t *testing.T) {
	cfg := newTestConfig(t)
	gc, ok := NewGarbageCollector(cfg).(*fileGarbageCollector)
	if !ok {
		t.Fatal("type assertion to *fileGarbageCollector failed")
	}

	recent := cfg.OCIRuntimeEntryDir("abcdabcdabcdabcd")
	if err := os.MkdirAll(recent, 0o755); err != nil {
		t.Fatalf("mkdir recent entry: %v", err)
	}
	writeRuntimeEntryMeta(t, cfg, "abcdabcdabcdabcd")

	collected, err := gc.CollectUnreferencedOCIRuntimeCaches()
	if err != nil {
		t.Fatalf("CollectUnreferencedOCIRuntimeCaches: %v", err)
	}
	if len(collected) != 0 {
		t.Fatalf("expected 0 collected due to grace period, got %v", collected)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatalf("recent entry dir should remain: %v", err)
	}
}

func TestOCIGC_CollectUnreferencedOCIRuntimeCaches_CleansStaleZeroRefEntry(t *testing.T) {
	cfg := newTestConfig(t)
	gc, ok := NewGarbageCollector(cfg).(*fileGarbageCollector)
	if !ok {
		t.Fatal("type assertion to *fileGarbageCollector failed")
	}
	tc := testableConfig{cfg: cfg}

	writeRuntimeRefs(t, tc, map[string][]string{
		"deadbeefdeadbeef": {},
	})

	collected, err := gc.CollectUnreferencedOCIRuntimeCaches()
	if err != nil {
		t.Fatalf("CollectUnreferencedOCIRuntimeCaches: %v", err)
	}
	if len(collected) != 0 {
		t.Fatalf("expected no dirs collected, got %v", collected)
	}

	var refs oci.RuntimeRefsIndex
	data, err := os.ReadFile(cfg.OCIRuntimeRefsFile())
	if err != nil {
		t.Fatalf("read runtime refs: %v", err)
	}
	if err := json.Unmarshal(data, &refs); err != nil {
		t.Fatalf("unmarshal runtime refs: %v", err)
	}
	if _, exists := refs.Runtimes["deadbeefdeadbeef"]; exists {
		t.Fatalf("expected stale zero-ref runtime entry to be removed")
	}
}

func TestOCIGC_CollectUnreferencedOCIRuntimeCaches_LayerMarkAndSweep(t *testing.T) {
	cfg := newTestConfig(t)
	gc, ok := NewGarbageCollector(cfg).(*fileGarbageCollector)
	if !ok {
		t.Fatal("type assertion to *fileGarbageCollector failed")
	}

	oldTime := time.Now().Add(-10 * time.Minute)

	// Create a pinned entry that references layer "aaa...".
	const (
		referencedLayer   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		unreferencedLayer = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		kernelLayer       = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	)
	entryDir := cfg.OCIRuntimeEntryDir("pinned-key")
	if err := os.MkdirAll(entryDir, 0o755); err != nil {
		t.Fatalf("mkdir entry dir: %v", err)
	}
	meta := &oci.OCIRuntimeEntryMeta{
		ManifestDigest:     "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		KernelLayerDigest:  kernelLayer,
		RootfsLayerDigests: []string{referencedLayer},
		Arch:               "amd64",
		VirtioFSTag:        "cocoon-rootfs",
	}
	if err := oci.WriteEntryMeta(cfg.OCIRuntimeEntryMetaPath("pinned-key"), meta); err != nil {
		t.Fatalf("write entry meta: %v", err)
	}

	tc := testableConfig{cfg: cfg}
	writeRuntimeRefs(t, tc, map[string][]string{
		"pinned-key": {"vm-PINNED"},
	})

	// Create layer dirs.
	for _, hex := range []string{referencedLayer, unreferencedLayer, kernelLayer} {
		layerDir := cfg.OCIRuntimeLayerDir(hex)
		if err := os.MkdirAll(layerDir, 0o755); err != nil {
			t.Fatalf("mkdir layer %s: %v", hex, err)
		}
		_ = os.Chtimes(layerDir, oldTime, oldTime)
	}

	collected, err := gc.CollectUnreferencedOCIRuntimeCaches()
	if err != nil {
		t.Fatalf("CollectUnreferencedOCIRuntimeCaches: %v", err)
	}

	// Only unreferenced layer should be collected.
	foundUnreferenced := false
	for _, c := range collected {
		if c == "layer:"+unreferencedLayer {
			foundUnreferenced = true
		}
	}
	if !foundUnreferenced {
		t.Fatalf("expected layer:%s in collected, got %v", unreferencedLayer, collected)
	}

	// Referenced layers should still exist.
	if _, err := os.Stat(cfg.OCIRuntimeLayerDir(referencedLayer)); err != nil {
		t.Fatalf("referenced layer should remain: %v", err)
	}
	if _, err := os.Stat(cfg.OCIRuntimeLayerDir(kernelLayer)); err != nil {
		t.Fatalf("kernel layer should remain: %v", err)
	}
	// Unreferenced layer should be gone.
	if _, err := os.Stat(cfg.OCIRuntimeLayerDir(unreferencedLayer)); !os.IsNotExist(err) {
		t.Fatalf("unreferenced layer should be removed, stat err=%v", err)
	}
}

func TestOCIGC_CollectUnreferencedOCIRuntimeCaches_LayerSweepFailsOnUnreadableEntryMeta(t *testing.T) {
	cfg := newTestConfig(t)
	gc, ok := NewGarbageCollector(cfg).(*fileGarbageCollector)
	if !ok {
		t.Fatal("type assertion to *fileGarbageCollector failed")
	}

	oldTime := time.Now().Add(-10 * time.Minute)
	const (
		referencedLayer = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
		otherLayer      = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
		runtimeKey      = "pinned-unreadable-meta"
	)

	entryDir := cfg.OCIRuntimeEntryDir(runtimeKey)
	if err := os.MkdirAll(entryDir, 0o755); err != nil {
		t.Fatalf("mkdir entry dir: %v", err)
	}
	// Corrupt meta.json to simulate unreadable metadata while entry is still pinned.
	if err := os.WriteFile(cfg.OCIRuntimeEntryMetaPath(runtimeKey), []byte("{"), 0o644); err != nil {
		t.Fatalf("write corrupt entry meta: %v", err)
	}

	tc := testableConfig{cfg: cfg}
	writeRuntimeRefs(t, tc, map[string][]string{
		runtimeKey: {"vm-PINNED"},
	})

	for _, hex := range []string{referencedLayer, otherLayer} {
		layerDir := cfg.OCIRuntimeLayerDir(hex)
		if err := os.MkdirAll(layerDir, 0o755); err != nil {
			t.Fatalf("mkdir layer %s: %v", hex, err)
		}
		_ = os.Chtimes(layerDir, oldTime, oldTime)
	}

	_, err := gc.CollectUnreferencedOCIRuntimeCaches()
	if err == nil {
		t.Fatal("expected CollectUnreferencedOCIRuntimeCaches to fail on unreadable entry meta")
	}
	if !strings.Contains(err.Error(), "read OCI runtime entry meta for "+runtimeKey) {
		t.Fatalf("unexpected error: %v", err)
	}

	// Fail-safe behavior: no layer should be removed when reference scan fails.
	if _, statErr := os.Stat(cfg.OCIRuntimeLayerDir(referencedLayer)); statErr != nil {
		t.Fatalf("referenced layer should remain after failed sweep: %v", statErr)
	}
	if _, statErr := os.Stat(cfg.OCIRuntimeLayerDir(otherLayer)); statErr != nil {
		t.Fatalf("other layer should remain after failed sweep: %v", statErr)
	}
}

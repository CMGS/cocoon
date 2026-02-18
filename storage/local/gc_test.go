package local

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/CMGS/cocoon/config"
)

// --- Test helpers ---

// createFakeImage creates a fake .qcow2 file in the image cache with the
// given baseKey name. The file mtime is set to the given time.
func createFakeImage(t *testing.T, cfg *config.CocoonConfig, baseKey string, mtime time.Time) string {
	t.Helper()
	path := cfg.BaseImagePath(baseKey)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", dir, err)
	}
	if err := os.WriteFile(path, []byte("FAKE-IMAGE-"+baseKey), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("Chtimes %s: %v", path, err)
	}
	return path
}

// --- Basic GC tests ---

func TestGC_CollectUnreferenced_Empty(t *testing.T) {
	cfg := newTestConfig(t)
	gc := NewGarbageCollector(cfg)

	collected, err := gc.CollectUnreferencedImages()
	if err != nil {
		t.Fatalf("CollectUnreferencedImages: %v", err)
	}
	if len(collected) != 0 {
		t.Errorf("collected = %v, want empty", collected)
	}
}

func TestGC_SafeDelete(t *testing.T) {
	cfg := newTestConfig(t)
	gc := NewGarbageCollector(cfg)
	rc := NewReferenceCounter(cfg)

	old := time.Now().Add(-48 * time.Hour)

	// Create two images: one referenced, one unreferenced.
	referencedKey := "aaaa111122223333_amd64"
	unreferencedKey := "bbbb444455556666_amd64"

	createFakeImage(t, cfg, referencedKey, old)
	createFakeImage(t, cfg, unreferencedKey, old)

	// Pin the first image.
	if err := rc.AddReference(referencedKey, "vm-001", "digest", "ref"); err != nil {
		t.Fatalf("AddReference: %v", err)
	}

	// Run GC — all unreferenced images are collected immediately.
	collected, err := gc.CollectUnreferencedImages()
	if err != nil {
		t.Fatalf("CollectUnreferencedImages: %v", err)
	}

	// Only the unreferenced image should be collected.
	if len(collected) != 1 {
		t.Fatalf("collected count = %d, want 1; collected = %v", len(collected), collected)
	}
	if collected[0] != unreferencedKey {
		t.Errorf("collected[0] = %q, want %q", collected[0], unreferencedKey)
	}

	// Verify referenced image still exists on disk.
	if _, err := os.Stat(cfg.BaseImagePath(referencedKey)); err != nil {
		t.Errorf("referenced image was removed (should have been preserved): %v", err)
	}

	// Verify unreferenced image was permanently deleted.
	if _, err := os.Stat(cfg.BaseImagePath(unreferencedKey)); !os.IsNotExist(err) {
		t.Errorf("unreferenced image still exists at original path (should have been deleted)")
	}
}

// --- Concurrent tests ---

// TestGC_ConcurrentWithRefCount verifies that GC and reference counting
// operations can run concurrently without data corruption. Multiple
// goroutines perform AddReference/RemoveReference while a GC cycle runs.
// The referenced images must survive GC. Must pass under -race.
func TestGC_ConcurrentWithRefCount(t *testing.T) {
	cfg := newTestConfig(t)
	gc := NewGarbageCollector(cfg)
	rc := NewReferenceCounter(cfg)

	const numKeys = 5
	old := time.Now().Add(-48 * time.Hour)

	// Create images and pin them with references.
	baseKeys := make([]string, numKeys)
	for i := 0; i < numKeys; i++ {
		baseKeys[i] = fmt.Sprintf("%016x_amd64", i)
		createFakeImage(t, cfg, baseKeys[i], old)
		if err := rc.AddReference(baseKeys[i], fmt.Sprintf("vm-%03d", i), "d", "r"); err != nil {
			t.Fatalf("AddReference key %d: %v", i, err)
		}
	}

	// Also create some unreferenced images that GC should collect.
	const numUnreferenced = 3
	unreferencedKeys := make([]string, numUnreferenced)
	for i := 0; i < numUnreferenced; i++ {
		unreferencedKeys[i] = fmt.Sprintf("%016x_amd64", 100+i)
		createFakeImage(t, cfg, unreferencedKeys[i], old)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 100)

	// Goroutine 1: Run GC.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := gc.CollectUnreferencedImages(); err != nil {
			errCh <- fmt.Errorf("GC: %w", err)
		}
	}()

	// Goroutines 2..N+1: Churn references (add then remove additional VMs).
	for i := 0; i < numKeys; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			key := baseKeys[idx]
			extraVM := fmt.Sprintf("vm-extra-%03d", idx)
			if err := rc.AddReference(key, extraVM, "d", "r"); err != nil {
				errCh <- fmt.Errorf("AddReference extra %d: %w", idx, err)
				return
			}
			if err := rc.RemoveReference(key, extraVM); err != nil {
				errCh <- fmt.Errorf("RemoveReference extra %d: %w", idx, err)
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("concurrent error: %v", err)
	}

	// All referenced images must still exist (GC must not have deleted them).
	for _, key := range baseKeys {
		if _, err := os.Stat(cfg.BaseImagePath(key)); err != nil {
			t.Errorf("referenced image %s was removed by GC: %v", key, err)
		}
	}

	// Each referenced key should still have exactly 1 reference (the original VM).
	for i, key := range baseKeys {
		refs, err := rc.GetReferences(key)
		if err != nil {
			t.Fatalf("GetReferences key %s: %v", key, err)
		}
		expectedVM := fmt.Sprintf("vm-%03d", i)
		if len(refs) != 1 || refs[0] != expectedVM {
			t.Errorf("key %s: refs = %v, want [%s]", key, refs, expectedVM)
		}
	}
}

// TestGC_ConcurrentMultipleGCRuns verifies that multiple concurrent GC runs
// do not conflict or cause double-deletion. The gc.lock (Level 1) should
// serialize them. Must pass under -race.
func TestGC_ConcurrentMultipleGCRuns(t *testing.T) {
	cfg := newTestConfig(t)
	gc := NewGarbageCollector(cfg)

	old := time.Now().Add(-48 * time.Hour)

	// Create unreferenced images.
	const numImages = 5
	for i := 0; i < numImages; i++ {
		key := fmt.Sprintf("%016x_amd64", 200+i)
		createFakeImage(t, cfg, key, old)
	}

	const numGCRuns = 5
	var wg sync.WaitGroup
	results := make([][]string, numGCRuns)
	errs := make([]error, numGCRuns)

	wg.Add(numGCRuns)
	for i := 0; i < numGCRuns; i++ {
		go func(idx int) {
			defer wg.Done()
			collected, err := gc.CollectUnreferencedImages()
			results[idx] = collected
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("GC run %d: error: %v", i, err)
		}
	}

	// Count total collected across all runs. The sum should be exactly
	// numImages (each image collected once, by whichever GC run got to it).
	totalCollected := 0
	for _, r := range results {
		totalCollected += len(r)
	}
	if totalCollected != numImages {
		t.Errorf("total collected = %d, want %d (each image collected exactly once)", totalCollected, numImages)
	}

	// All original image paths should be gone.
	for i := 0; i < numImages; i++ {
		key := fmt.Sprintf("%016x_amd64", 200+i)
		if _, err := os.Stat(cfg.BaseImagePath(key)); !os.IsNotExist(err) {
			t.Errorf("image %s still exists after concurrent GC runs", key)
		}
	}
}

// TestGC_AddReferenceDuringGC verifies the TOCTOU safety: if a new reference
// is added for an image BEFORE GC checks it, GC must not collect it. This
// tests the atomic check-and-delete pattern inside withRefsLock.
func TestGC_AddReferenceDuringGC(t *testing.T) {
	cfg := newTestConfig(t)
	gc := NewGarbageCollector(cfg)
	rc := NewReferenceCounter(cfg)

	old := time.Now().Add(-48 * time.Hour)
	baseKey := "ddddeeeeffffaaaa_amd64"
	createFakeImage(t, cfg, baseKey, old)

	// Run AddReference and GC concurrently.
	var wg sync.WaitGroup
	var gcErr error
	var gcCollected []string

	wg.Add(2)

	go func() {
		defer wg.Done()
		// Add reference. If this happens before GC checks this key,
		// GC should skip it; if after, GC may have already collected it.
		if err := rc.AddReference(baseKey, "vm-race", "d", "r"); err != nil {
			t.Errorf("AddReference during GC: %v", err) //nolint:testifylint
		}
	}()

	go func() {
		defer wg.Done()
		gcCollected, gcErr = gc.CollectUnreferencedImages()
	}()

	wg.Wait()

	if gcErr != nil {
		t.Fatalf("GC error: %v", gcErr)
	}

	// Two valid outcomes, both safe:
	// 1. GC checked the key BEFORE AddReference -> image collected, reference
	//    points to deleted file (acceptable; the ref is to a since-collected image).
	// 2. GC checked the key AFTER AddReference -> image preserved.
	//
	// In neither case should there be a crash, race, or data corruption.
	// We just verify no errors occurred and the program state is consistent.
	t.Logf("GC collected %d items: %v", len(gcCollected), gcCollected)

	if len(gcCollected) == 0 {
		// GC saw the reference and skipped the image. Verify it's still on disk.
		if _, err := os.Stat(cfg.BaseImagePath(baseKey)); err != nil {
			t.Errorf("image not collected but also not on disk: %v", err)
		}
	}
	// If gcCollected contains baseKey, the image was deleted before
	// AddReference ran. That's also a valid outcome.
}

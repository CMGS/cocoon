package local

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/CMGS/cocoon/config"
)

// newTestConfig creates a CocoonConfig rooted in a temp directory with all
// required subdirectories pre-created.
func newTestConfig(t *testing.T) *config.CocoonConfig {
	t.Helper()
	root := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.RebaseRootDir(root)
	cfg.RuntimeDir = filepath.Join(root, "run")
	cfg.LogDir = filepath.Join(root, "log")
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}
	return cfg
}

// --- Basic reference counter tests ---

func TestRefCount_AddAndGet(t *testing.T) {
	cfg := newTestConfig(t)
	rc := NewReferenceCounter(cfg)

	baseKey := "abcdef0123456789_amd64"
	vmID := "vm-001"

	if err := rc.AddReference(baseKey, vmID, "digest-full-64-chars", "docker.io/test:latest"); err != nil {
		t.Fatalf("AddReference: %v", err)
	}

	refs, err := rc.GetReferences(baseKey)
	if err != nil {
		t.Fatalf("GetReferences: %v", err)
	}
	if len(refs) != 1 || refs[0] != vmID {
		t.Errorf("refs = %v, want [%s]", refs, vmID)
	}

	ok, err := rc.IsReferenced(baseKey)
	if err != nil {
		t.Fatalf("IsReferenced: %v", err)
	}
	if !ok {
		t.Error("IsReferenced = false, want true")
	}
}

func TestRefCount_AddIdempotent(t *testing.T) {
	cfg := newTestConfig(t)
	rc := NewReferenceCounter(cfg)

	baseKey := "abcdef0123456789_amd64"
	vmID := "vm-001"

	for i := 0; i < 5; i++ {
		if err := rc.AddReference(baseKey, vmID, "digest", "ref"); err != nil {
			t.Fatalf("AddReference (call %d): %v", i, err)
		}
	}

	refs, err := rc.GetReferences(baseKey)
	if err != nil {
		t.Fatalf("GetReferences: %v", err)
	}
	if len(refs) != 1 {
		t.Errorf("refs count = %d, want 1 (idempotent add)", len(refs))
	}
}

func TestRefCount_RemoveLastDeletesEntry(t *testing.T) {
	cfg := newTestConfig(t)
	rc := NewReferenceCounter(cfg)

	baseKey := "abcdef0123456789_amd64"

	if err := rc.AddReference(baseKey, "vm-001", "d", "r"); err != nil {
		t.Fatalf("AddReference: %v", err)
	}
	if err := rc.RemoveReference(baseKey, "vm-001"); err != nil {
		t.Fatalf("RemoveReference: %v", err)
	}

	refs, err := rc.GetReferences(baseKey)
	if err != nil {
		t.Fatalf("GetReferences: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("refs = %v, want empty after removing last reference", refs)
	}

	ok, err := rc.IsReferenced(baseKey)
	if err != nil {
		t.Fatalf("IsReferenced: %v", err)
	}
	if ok {
		t.Error("IsReferenced = true, want false after removing last reference")
	}
}

func TestRefCount_RemoveNonExistentIsNoOp(t *testing.T) {
	cfg := newTestConfig(t)
	rc := NewReferenceCounter(cfg)

	// Removing from a key that was never added should not error.
	if err := rc.RemoveReference("abcdef0123456789_amd64", "vm-nonexistent"); err != nil {
		t.Fatalf("RemoveReference for non-existent key: %v", err)
	}
}

// --- Concurrent tests ---

// TestRefCount_ConcurrentAddRemove exercises concurrent Add and Remove
// operations on the SAME key. N goroutines each add a unique vmID, then
// remove it. The final ref count must be 0. This must pass under -race.
func TestRefCount_ConcurrentAddRemove(t *testing.T) {
	cfg := newTestConfig(t)
	rc := NewReferenceCounter(cfg)

	baseKey := "abcdef0123456789_amd64"
	const N = 20

	var wg sync.WaitGroup
	errs := make([]error, N*2) // N adds + N removes

	// Phase 1: All goroutines add their unique vmID concurrently.
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(idx int) {
			defer wg.Done()
			vmID := fmt.Sprintf("vm-%03d", idx)
			errs[idx] = rc.AddReference(baseKey, vmID, "digest", "ref")
		}(i)
	}
	wg.Wait()

	for i := 0; i < N; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: AddReference error: %v", i, errs[i])
		}
	}

	// Verify all N VMs are referenced.
	refs, err := rc.GetReferences(baseKey)
	if err != nil {
		t.Fatalf("GetReferences after adds: %v", err)
	}
	if len(refs) != N {
		t.Fatalf("refs count after adds = %d, want %d", len(refs), N)
	}

	// Phase 2: All goroutines remove their vmID concurrently.
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(idx int) {
			defer wg.Done()
			vmID := fmt.Sprintf("vm-%03d", idx)
			errs[N+idx] = rc.RemoveReference(baseKey, vmID)
		}(i)
	}
	wg.Wait()

	for i := 0; i < N; i++ {
		if errs[N+i] != nil {
			t.Fatalf("goroutine %d: RemoveReference error: %v", i, errs[N+i])
		}
	}

	// Verify final count is 0.
	refs, err = rc.GetReferences(baseKey)
	if err != nil {
		t.Fatalf("GetReferences after removes: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("refs count after concurrent add/remove = %d, want 0", len(refs))
	}

	ok, err := rc.IsReferenced(baseKey)
	if err != nil {
		t.Fatalf("IsReferenced: %v", err)
	}
	if ok {
		t.Error("IsReferenced = true after all removes, want false")
	}
}

// TestRefCount_ConcurrentMultiKey exercises concurrent operations on DIFFERENT
// keys. Each goroutine operates exclusively on its own baseKey, ensuring the
// per-key locking does not cause cross-key interference. Must pass under -race.
func TestRefCount_ConcurrentMultiKey(t *testing.T) {
	cfg := newTestConfig(t)
	rc := NewReferenceCounter(cfg)

	const N = 10         // number of distinct keys
	const refsPerKey = 5 // VMs per key

	var wg sync.WaitGroup
	errCh := make(chan error, N*refsPerKey*2) // buffered for all ops

	// Each goroutine adds refsPerKey VMs to its own baseKey.
	wg.Add(N)
	for k := 0; k < N; k++ {
		go func(keyIdx int) {
			defer wg.Done()
			baseKey := fmt.Sprintf("%016x_amd64", keyIdx)
			for v := 0; v < refsPerKey; v++ {
				vmID := fmt.Sprintf("vm-k%d-v%d", keyIdx, v)
				if err := rc.AddReference(baseKey, vmID, "digest", "ref"); err != nil {
					errCh <- fmt.Errorf("key %d add vm %d: %w", keyIdx, v, err)
				}
			}
		}(k)
	}
	wg.Wait()

	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent multi-key add error: %v", err)
	}

	// Verify each key has exactly refsPerKey references.
	for k := 0; k < N; k++ {
		baseKey := fmt.Sprintf("%016x_amd64", k)
		refs, err := rc.GetReferences(baseKey)
		if err != nil {
			t.Fatalf("GetReferences key %d: %v", k, err)
		}
		if len(refs) != refsPerKey {
			t.Errorf("key %d: refs count = %d, want %d", k, len(refs), refsPerKey)
		}
	}

	// Concurrently remove all references from all keys.
	errCh2 := make(chan error, N*refsPerKey)
	wg.Add(N)
	for k := 0; k < N; k++ {
		go func(keyIdx int) {
			defer wg.Done()
			baseKey := fmt.Sprintf("%016x_amd64", keyIdx)
			for v := 0; v < refsPerKey; v++ {
				vmID := fmt.Sprintf("vm-k%d-v%d", keyIdx, v)
				if err := rc.RemoveReference(baseKey, vmID); err != nil {
					errCh2 <- fmt.Errorf("key %d remove vm %d: %w", keyIdx, v, err)
				}
			}
		}(k)
	}
	wg.Wait()

	close(errCh2)
	for err := range errCh2 {
		t.Fatalf("concurrent multi-key remove error: %v", err)
	}

	// Verify all keys are unreferenced.
	for k := 0; k < N; k++ {
		baseKey := fmt.Sprintf("%016x_amd64", k)
		refs, err := rc.GetReferences(baseKey)
		if err != nil {
			t.Fatalf("GetReferences key %d after remove: %v", k, err)
		}
		if len(refs) != 0 {
			t.Errorf("key %d: refs count after remove = %d, want 0", k, len(refs))
		}
	}
}

// TestRefCount_ConcurrentAddRemoveInterleaved exercises a mixed workload
// where adds and removes for the same key happen concurrently. The final
// invariant: all added vmIDs that are NOT also removed must be present.
func TestRefCount_ConcurrentAddRemoveInterleaved(t *testing.T) {
	cfg := newTestConfig(t)
	rc := NewReferenceCounter(cfg)

	baseKey := "1111111111111111_amd64"
	const addCount = 20
	const removeCount = 10 // remove only first 10

	// Add all first so remove can find them.
	for i := 0; i < addCount; i++ {
		vmID := fmt.Sprintf("vm-%03d", i)
		if err := rc.AddReference(baseKey, vmID, "d", "r"); err != nil {
			t.Fatalf("setup AddReference vm-%03d: %v", i, err)
		}
	}

	// Now concurrently: add addCount more, and remove the first removeCount.
	var wg sync.WaitGroup
	errCh := make(chan error, addCount+removeCount)

	// Additional adds (new vmIDs).
	wg.Add(addCount)
	for i := 0; i < addCount; i++ {
		go func(idx int) {
			defer wg.Done()
			vmID := fmt.Sprintf("vm-new-%03d", idx)
			if err := rc.AddReference(baseKey, vmID, "d", "r"); err != nil {
				errCh <- err
			}
		}(i)
	}

	// Remove the first removeCount of the original VMs.
	wg.Add(removeCount)
	for i := 0; i < removeCount; i++ {
		go func(idx int) {
			defer wg.Done()
			vmID := fmt.Sprintf("vm-%03d", idx)
			if err := rc.RemoveReference(baseKey, vmID); err != nil {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()

	close(errCh)
	for err := range errCh {
		t.Fatalf("interleaved op error: %v", err)
	}

	// Expected: (addCount - removeCount) original + addCount new = 30.
	expectedCount := (addCount - removeCount) + addCount
	refs, err := rc.GetReferences(baseKey)
	if err != nil {
		t.Fatalf("GetReferences: %v", err)
	}
	if len(refs) != expectedCount {
		t.Errorf("refs count = %d, want %d", len(refs), expectedCount)
	}
}

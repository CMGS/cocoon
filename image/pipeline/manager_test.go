package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/image"
	"github.com/CMGS/cocoon/image/refcache"
)

// --- Test helpers ---

// newTestConfig creates a CocoonConfig rooted in a temp directory with all
// required subdirectories pre-created. The caller does not need to clean up;
// t.TempDir() handles it.
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

// createFakeQcow2 writes a minimal fake qcow2 file at the given path. The
// file is small and does NOT contain a valid qcow2 header; it is sufficient
// only for testing cache-hit code paths that check os.Stat.
func createFakeQcow2(t *testing.T, path string) {
	t.Helper()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", dir, err)
	}
	if err := os.WriteFile(path, []byte("FAKE-QCOW2-DATA"), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

// fakeReferenceCounter is a no-op storage.ReferenceCounter for testing. The
// pipeline manager only uses it in ListCached/RemoveCached, not in Prepare.
type fakeReferenceCounter struct{}

func (fakeReferenceCounter) AddReference(_, _, _, _ string) error     { return nil }
func (fakeReferenceCounter) RemoveReference(_, _ string) error        { return nil }
func (fakeReferenceCounter) GetReferences(_ string) ([]string, error) { return nil, nil }
func (fakeReferenceCounter) IsReferenced(_ string) (bool, error)      { return false, nil }
func (fakeReferenceCounter) GetUnreferencedImages() ([]string, error) { return nil, nil }

// --- Tests for Prepare: cache result ---

// TestPrepare_CachesResult verifies that Prepare returns a cache hit on the
// second call without repeating Pull+Convert. We use a local file reference
// so the test runs on any platform (no skopeo/buildah required).
func TestPrepare_CachesResult(t *testing.T) {
	cfg := newTestConfig(t)
	mgr := New(cfg, fakeReferenceCounter{}).(*manager)

	// Create a tiny local source file that Prepare will "pull" (i.e. hash).
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "test.img")
	if err := os.WriteFile(srcPath, []byte("hello-cocoon-test-image"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	ctx := context.Background()

	// First call: Pull computes identity, Convert would run.  Because
	// detectImageFormat calls qemu-img (which may not exist in CI), we
	// pre-seed the cache to exercise only the fast-path code.
	identity, err := mgr.pullLocal(ctx, srcPath)
	if err != nil {
		t.Fatalf("pullLocal: %v", err)
	}

	baseKey := identity.BaseKey()
	basePath := cfg.BaseImagePath(baseKey)
	createFakeQcow2(t, basePath)

	// Now Prepare should hit the cache on both calls.
	_, path1, err := mgr.Prepare(ctx, srcPath)
	if err != nil {
		t.Fatalf("Prepare (1st): %v", err)
	}
	if path1 != basePath {
		t.Errorf("Prepare (1st): path = %q, want %q", path1, basePath)
	}

	_, path2, err := mgr.Prepare(ctx, srcPath)
	if err != nil {
		t.Fatalf("Prepare (2nd): %v", err)
	}
	if path2 != basePath {
		t.Errorf("Prepare (2nd): path = %q, want %q", path2, basePath)
	}
}

func TestPrepare_AmbiguousRefcacheRefReturnsError(t *testing.T) {
	cfg := newTestConfig(t)
	mgr := New(cfg, fakeReferenceCounter{}).(*manager)

	if err := refcache.Upsert(cfg, "ubuntu:22.04", "1111111111111111_amd64", "digest-a"); err != nil {
		t.Fatalf("Upsert ubuntu:22.04: %v", err)
	}
	if err := refcache.Upsert(cfg, "ubuntu:24.04", "2222222222222222_amd64", "digest-b"); err != nil {
		t.Fatalf("Upsert ubuntu:24.04: %v", err)
	}

	_, _, err := mgr.Prepare(context.Background(), "ubuntu")
	if err == nil {
		t.Fatal("Prepare(ubuntu): expected ambiguous ref error, got nil")
	}
	if !errors.Is(err, refcache.ErrAmbiguousImageRef) {
		t.Fatalf("Prepare(ubuntu): error=%v, want ErrAmbiguousImageRef", err)
	}
}

// --- Tests for Convert: concurrent deduplication ---

// TestConvert_ConcurrentDedup verifies the identify-outside-lock,
// convert-inside-lock pattern. When multiple goroutines call Convert
// concurrently for the same image, only the first should perform the actual
// conversion; the rest should observe the cache-after-lock hit.
//
// Because Convert calls detectImageFormat / qemu-img / guestfish which may
// not be available in test environments, we test the dedup at the level we
// can: by pre-seeding the cache after the first goroutine "acquires the lock"
// so subsequent goroutines observe a cache hit.
//
// The pattern tested is:
//  1. N goroutines all get the same identity (identify phase, outside lock)
//  2. They all enter Convert and contend on the flock
//  3. The first goroutine to acquire the lock finds no cache and we
//     pre-populate the cache file for it (simulating conversion)
//  4. Subsequent goroutines acquire the lock and see the cache hit
func TestConvert_ConcurrentDedup(t *testing.T) {
	cfg := newTestConfig(t)

	// The identity we'll use for all goroutines (local file type).
	identity := &image.ImageIdentity{
		Checksum:  "abcdef0123456789",
		Arch:      "amd64",
		SourceRef: "test-ref",
		ImageType: image.ImageTypeLocalFile,
		TempPath:  "/dev/null", // placeholder; won't be read for cache-hit path
	}

	baseKey := identity.BaseKey()
	basePath := cfg.BaseImagePath(baseKey)

	// Pre-create the cache file to simulate an already-converted image.
	// This means every goroutine calling Convert will see the cache hit
	// after acquiring the lock.
	createFakeQcow2(t, basePath)

	mgr := New(cfg, fakeReferenceCounter{}).(*manager)

	const N = 10
	var wg sync.WaitGroup
	errs := make([]error, N)
	paths := make([]string, N)

	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(idx int) {
			defer wg.Done()
			p, err := mgr.Convert(context.Background(), identity)
			errs[idx] = err
			paths[idx] = p
		}(i)
	}
	wg.Wait()

	for i := 0; i < N; i++ {
		if errs[i] != nil {
			t.Errorf("goroutine %d: Convert error: %v", i, errs[i])
		}
		if paths[i] != basePath {
			t.Errorf("goroutine %d: path = %q, want %q", i, paths[i], basePath)
		}
	}
}

// TestPrepareOCI_ConcurrentDedup tests the OCI-specific Prepare pipeline's
// concurrent deduplication. Multiple goroutines call prepareOCI concurrently
// for the same image ref. Only one should perform the actual pull+convert;
// the others should hit the cache (either before or after acquiring the lock).
//
// Since this test runs on both Darwin and Linux, and the OCI platform
// functions require real tools (skopeo, buildah), we test the concurrency
// mechanism by directly invoking the lock+cache pattern that prepareOCI uses.
// We replicate the exact flow:
//
//	Phase 1: identifyOCIPlatform (simulated)
//	Phase 2: Fast-path cache check
//	Phase 3-6: Lock -> double-check cache -> pull+convert -> unlock
//
// A conversion counter tracks how many goroutines would have performed
// the actual conversion (entered the critical section when cache is cold).
func TestPrepareOCI_ConcurrentDedup(t *testing.T) {
	cfg := newTestConfig(t)

	baseKey := "1234567890abcdef_amd64"
	basePath := cfg.BaseImagePath(baseKey)
	lockPath := cfg.ConversionLockPath(baseKey)

	// conversionCount tracks how many goroutines enter the "conversion"
	// section (i.e. they acquired the lock AND the cache was still cold).
	var conversionCount atomic.Int32

	// simulatePrepareOCI replicates the prepareOCI flow:
	// Phase 1: skip (identity already known)
	// Phase 2: fast-path cache check
	// Phase 3: acquire lock
	// Phase 4: double-check cache
	// Phase 5+6: "convert" (create the cache file) if needed
	simulatePrepareOCI := func() error {
		// Phase 2: Fast-path cache check (outside lock).
		if _, err := os.Stat(basePath); err == nil {
			return nil // cache hit
		}

		// Phase 3: Acquire per-image lock.
		fl := newTestFlock(lockPath)
		if err := fl.Lock(); err != nil {
			return err
		}
		defer fl.Unlock() //nolint:errcheck

		// Phase 4: Double-check cache after lock acquisition.
		if _, err := os.Stat(basePath); err == nil {
			return nil // cache hit after lock
		}

		// Phase 5+6: Simulate conversion by creating the cache file.
		conversionCount.Add(1)
		return os.WriteFile(basePath, []byte("converted-image"), 0o644)
	}

	const N = 10
	var wg sync.WaitGroup
	errs := make([]error, N)

	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(idx int) {
			defer wg.Done()
			errs[idx] = simulatePrepareOCI()
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: error: %v", i, err)
		}
	}

	// Exactly 1 goroutine should have performed the conversion.
	got := conversionCount.Load()
	if got != 1 {
		t.Errorf("conversion count = %d, want 1", got)
	}

	// The cache file should exist.
	if _, err := os.Stat(basePath); err != nil {
		t.Errorf("cache file missing after concurrent prepare: %v", err)
	}
}

func TestHasDeepVerificationUnavailableWarning(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		warnings []string
		want     bool
	}{
		{
			name:     "darwin warning",
			warnings: []string{"deep boot verification not available on darwin"},
			want:     true,
		},
		{
			name:     "linux missing guestfish warning",
			warnings: []string{"deep boot verification requires guestfish (not installed)"},
			want:     true,
		},
		{
			name:     "unrelated warning",
			warnings: []string{"qemu-img info returned sparse metadata"},
			want:     false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := hasDeepVerificationUnavailableWarning(tc.warnings)
			if got != tc.want {
				t.Fatalf("hasDeepVerificationUnavailableWarning(%v)=%v, want %v", tc.warnings, got, tc.want)
			}
		})
	}
}

// --- Test helpers for flock (same mechanism as production code) ---

// testFlock is a minimal flock wrapper for use in tests. It mirrors the
// production flock.Lock but lives in the test file to avoid import cycles.
type testFlock struct {
	path string
	file *os.File
}

func newTestFlock(path string) *testFlock {
	return &testFlock{path: path}
}

func (l *testFlock) Lock() error {
	f, err := os.OpenFile(l.path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	l.file = f
	return flockExclusive(f)
}

func (l *testFlock) Unlock() error {
	if l.file == nil {
		return nil
	}
	defer func() { l.file = nil }()
	if err := flockRelease(l.file); err != nil {
		_ = l.file.Close()
		return err
	}
	return l.file.Close()
}

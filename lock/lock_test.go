package lock

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestLockUnlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.lock")
	l := New(path)

	if err := l.Lock(); err != nil {
		t.Fatalf("Lock error: %v", err)
	}
	if err := l.Unlock(); err != nil {
		t.Fatalf("Unlock error: %v", err)
	}
}

func TestDoubleUnlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.lock")
	l := New(path)

	if err := l.Lock(); err != nil {
		t.Fatalf("Lock error: %v", err)
	}
	if err := l.Unlock(); err != nil {
		t.Fatalf("first Unlock error: %v", err)
	}
	// Second Unlock should be safe (file is nil).
	if err := l.Unlock(); err != nil {
		t.Fatalf("second Unlock error: %v", err)
	}
}

func TestTryLock_Unlocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.lock")
	l := New(path)

	got, err := l.TryLock()
	if err != nil {
		t.Fatalf("TryLock error: %v", err)
	}
	if !got {
		t.Error("TryLock on unlocked file should return true")
	}
	if err := l.Unlock(); err != nil {
		t.Fatalf("Unlock error: %v", err)
	}
}

func TestTryLock_Contention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.lock")

	// Acquire with the first lock.
	l1 := New(path)
	if err := l1.Lock(); err != nil {
		t.Fatalf("Lock error: %v", err)
	}

	// TryLock with a second lock on the same path should fail.
	l2 := New(path)
	got, err := l2.TryLock()
	if err != nil {
		t.Fatalf("TryLock error: %v", err)
	}
	if got {
		t.Error("TryLock should return false when lock is held by another")
		_ = l2.Unlock()
	}

	if err := l1.Unlock(); err != nil {
		t.Fatalf("Unlock error: %v", err)
	}

	// Now TryLock should succeed.
	got, err = l2.TryLock()
	if err != nil {
		t.Fatalf("TryLock error after unlock: %v", err)
	}
	if !got {
		t.Error("TryLock should succeed after first lock is released")
	}
	_ = l2.Unlock()
}

func TestConcurrentLocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.lock")

	// Both goroutines will try to Lock() the same file.
	// Both should eventually succeed (one waits for the other).
	var wg sync.WaitGroup
	results := make(chan int, 2)

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			l := New(path)
			if err := l.Lock(); err != nil {
				t.Errorf("goroutine %d Lock error: %v", id, err)
				return
			}
			results <- id
			// Hold the lock briefly.
			time.Sleep(10 * time.Millisecond)
			if err := l.Unlock(); err != nil {
				t.Errorf("goroutine %d Unlock error: %v", id, err)
			}
		}(i)
	}

	wg.Wait()
	close(results)

	var count int
	for range results {
		count++
	}
	if count != 2 {
		t.Errorf("expected 2 goroutines to acquire lock, got %d", count)
	}
}

func TestPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.lock")
	l := New(path)
	// The Locker interface includes Path().
	if l.Path() != path {
		t.Errorf("Path() = %q, want %q", l.Path(), path)
	}
}

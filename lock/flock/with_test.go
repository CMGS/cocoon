package flock

import (
	"fmt"
	"path/filepath"
	"testing"
)

func TestWithLock(t *testing.T) {
	t.Parallel()
	lockPath := filepath.Join(t.TempDir(), "test.lock")

	called := false
	err := WithLock(lockPath, func() error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("WithLock error: %v", err)
	}
	if !called {
		t.Error("callback was not called")
	}
}

func TestWithLock_PropagatesError(t *testing.T) {
	t.Parallel()
	lockPath := filepath.Join(t.TempDir(), "test.lock")

	want := fmt.Errorf("callback failed")
	err := WithLock(lockPath, func() error {
		return want
	})
	if err != want {
		t.Errorf("expected %v, got %v", want, err)
	}
}

func TestWithLock_MutualExclusion(t *testing.T) {
	t.Parallel()
	lockPath := filepath.Join(t.TempDir(), "test.lock")

	// Acquire the lock with a direct Lock to prove WithLock blocks.
	blocker := &Lock{path: lockPath}
	if err := blocker.Lock(); err != nil {
		t.Fatalf("blocker Lock error: %v", err)
	}

	done := make(chan struct{})
	go func() {
		_ = WithLock(lockPath, func() error {
			close(done)
			return nil
		})
	}()

	// WithLock goroutine should be blocked.
	select {
	case <-done:
		t.Fatal("WithLock should block while lock is held")
	default:
	}

	// Release blocker — WithLock should proceed.
	if err := blocker.Unlock(); err != nil {
		t.Fatalf("blocker Unlock error: %v", err)
	}
	<-done
}

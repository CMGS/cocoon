package flock

import (
	"fmt"
	"os"
	"syscall"

	"github.com/CMGS/cocoon/lock"
)

// Compile-time interface check.
var _ lock.Locker = (*Lock)(nil)

// Lock provides cross-process mutual exclusion using flock(2).
// Lock files are long-lived and never deleted after use.
//
// Lock hierarchy (must acquire in order to prevent deadlocks):
//
//	Level 1: GC Lock              (/var/lib/cocoon/gc.lock)
//	Level 2: References Lock      (/var/lib/cocoon/references.lock)
//	Level 2: Name Index Lock      (/var/lib/cocoon/name-index.lock) — never held with references.lock
//	Level 3: Image Conversion Lock (/var/lib/cocoon/cache/locks/{base_key}.lock)
//	Level 4: VM Metadata Lock     (/var/lib/cocoon/vms/{vm-id}/metadata.lock)
type Lock struct {
	path string
	file *os.File
}

// New creates a new Lock for the given path.
// The lock file is created if it does not exist.
// Returns the Locker interface to support dependency injection and testing.
func New(path string) lock.Locker {
	return &Lock{path: path}
}

// Lock acquires an exclusive flock. Blocks until the lock is available.
func (l *Lock) Lock() error {
	f, err := os.OpenFile(l.path, os.O_RDWR|os.O_CREATE, 0o644) //nolint:gosec // G302: lock files need to be readable by other cocoon processes
	if err != nil {
		return fmt.Errorf("open lock file %s: %w", l.path, err)
	}
	l.file = f

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		l.file = nil
		return fmt.Errorf("acquire flock %s: %w", l.path, err)
	}
	return nil
}

// TryLock attempts to acquire the lock without blocking.
// Returns false if the lock is held by another process.
func (l *Lock) TryLock() (bool, error) {
	f, err := os.OpenFile(l.path, os.O_RDWR|os.O_CREATE, 0o644) //nolint:gosec // G302: lock files need to be readable by other cocoon processes
	if err != nil {
		return false, fmt.Errorf("open lock file %s: %w", l.path, err)
	}
	l.file = f

	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		_ = f.Close()
		l.file = nil
		if err == syscall.EWOULDBLOCK {
			return false, nil
		}
		return false, fmt.Errorf("try flock %s: %w", l.path, err)
	}
	return true, nil
}

// Unlock releases the flock and closes the file.
// The lock file is NOT deleted (long-lived by design).
func (l *Lock) Unlock() error {
	if l.file == nil {
		return nil
	}
	defer func() { l.file = nil }()

	if err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN); err != nil {
		_ = l.file.Close()
		return fmt.Errorf("release flock %s: %w", l.path, err)
	}
	return l.file.Close()
}

// Path returns the lock file path.
func (l *Lock) Path() string {
	return l.path
}

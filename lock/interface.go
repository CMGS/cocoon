package lock

// Locker defines the interface for cross-process mutual exclusion.
// Implementations may use flock(2), advisory locks, or other mechanisms.
//
// This interface enables dependency injection and testing of components
// that require locking without requiring real filesystem flocks.
type Locker interface {
	// Lock acquires an exclusive lock. Blocks until the lock is available.
	Lock() error

	// TryLock attempts to acquire the lock without blocking.
	// Returns false if the lock is held by another process.
	TryLock() (bool, error)

	// Unlock releases the lock.
	Unlock() error

	// Path returns the lock file path.
	Path() string
}

// Compile-time interface check.
var _ Locker = (*FileLock)(nil)

package flock

// WithLock acquires an exclusive flock at lockPath, runs fn, then releases.
// This is a convenience wrapper for code that needs only the flock without
// higher-level JSON load/save semantics.
func WithLock(lockPath string, fn func() error) error {
	fl := &Lock{path: lockPath}
	if err := fl.Lock(); err != nil {
		return err
	}
	defer fl.Unlock() //nolint:errcheck
	return fn()
}

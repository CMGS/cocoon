//go:build darwin || linux

package pipeline

import (
	"os"
	"syscall"
)

// flockExclusive acquires an exclusive flock on the given file.
// Used by testFlock in manager_test.go.
func flockExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// flockRelease releases the flock on the given file.
func flockRelease(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

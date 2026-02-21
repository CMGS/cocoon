// Package jsonstore provides a generic flock-protected atomic JSON store.
//
// Store[T] eliminates the repeated pattern of flock + ReadJSON + mutate +
// AtomicWriteJSON found across the codebase by providing two operations:
//
//   - Read: load under flock, no auto-save
//   - Update: load under flock, auto-save on success
package jsonstore

import (
	"fmt"
	"os"

	"github.com/CMGS/cocoon/lock/flock"
	"github.com/CMGS/cocoon/utils"
)

// Store[T] provides flock-protected atomic JSON persistence for a value of
// type T. The lock file and data file are separate paths; the lock is held
// only for the duration of Read/Update callbacks.
type Store[T any] struct {
	lockPath  string
	dataPath  string
	ensureDir string
	newEmpty  func() *T
}

// New creates a Store that protects dataPath with a flock at lockPath.
// newEmpty returns a pointer to an initialized zero value of T (with any
// internal maps allocated).
func New[T any](lockPath, dataPath string, newEmpty func() *T) *Store[T] {
	return &Store[T]{
		lockPath: lockPath,
		dataPath: dataPath,
		newEmpty: newEmpty,
	}
}

// WithEnsureDir configures the Store to create dir (with 0o700) before
// acquiring the lock. Returns s for method chaining.
func (s *Store[T]) WithEnsureDir(dir string) *Store[T] {
	s.ensureDir = dir
	return s
}

// Read loads T under flock and calls fn. The data is NOT auto-saved after
// fn returns — use this for read-only access or when the caller needs
// explicit save control.
func (s *Store[T]) Read(fn func(*T) error) error {
	if err := s.ensureDirExists(); err != nil {
		return err
	}
	return flock.WithLock(s.lockPath, func() error {
		data, err := s.load()
		if err != nil {
			return err
		}
		return fn(data)
	})
}

// Update loads T under flock, calls fn, and atomically saves T if fn
// returns nil. If fn returns an error the data is NOT saved and the error
// is propagated.
func (s *Store[T]) Update(fn func(*T) error) error {
	if err := s.ensureDirExists(); err != nil {
		return err
	}
	return flock.WithLock(s.lockPath, func() error {
		data, err := s.load()
		if err != nil {
			return err
		}
		if err := fn(data); err != nil {
			return err
		}
		return utils.AtomicWriteJSON(s.dataPath, data)
	})
}

// load reads and unmarshals the data file. If the file does not exist,
// returns the result of newEmpty() (never nil).
func (s *Store[T]) load() (*T, error) {
	data := s.newEmpty()
	if err := utils.ReadJSON(s.dataPath, data); err != nil {
		if os.IsNotExist(err) {
			return data, nil
		}
		return nil, fmt.Errorf("read %s: %w", s.dataPath, err)
	}
	return data, nil
}

func (s *Store[T]) ensureDirExists() error {
	if s.ensureDir == "" {
		return nil
	}
	if err := os.MkdirAll(s.ensureDir, 0o700); err != nil {
		return fmt.Errorf("create dir %s: %w", s.ensureDir, err)
	}
	return nil
}

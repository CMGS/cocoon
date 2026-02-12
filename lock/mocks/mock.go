// Package mocks provides test doubles for the lock.Locker interface.
package mocks

import "github.com/CMGS/cocoon/lock"

// MockLocker is a test double for lock.Locker.
// Each method can be overridden by setting the corresponding Func field.
// If a Func field is nil, the method returns zero values.
type MockLocker struct {
	LockFunc    func() error
	TryLockFunc func() (bool, error)
	UnlockFunc  func() error
	PathFunc    func() string

	// LockPath stores the path for the Path() default return.
	LockPath string
}

// Compile-time check that MockLocker implements lock.Locker.
var _ lock.Locker = (*MockLocker)(nil)

func (m *MockLocker) Lock() error {
	if m.LockFunc != nil {
		return m.LockFunc()
	}
	return nil
}

func (m *MockLocker) TryLock() (bool, error) {
	if m.TryLockFunc != nil {
		return m.TryLockFunc()
	}
	return true, nil
}

func (m *MockLocker) Unlock() error {
	if m.UnlockFunc != nil {
		return m.UnlockFunc()
	}
	return nil
}

func (m *MockLocker) Path() string {
	if m.PathFunc != nil {
		return m.PathFunc()
	}
	return m.LockPath
}

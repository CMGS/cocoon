// Package mocks provides mock implementations of the image.Manager interface
// for use in unit tests. These mocks allow tests to exercise image-dependent
// code paths without requiring actual OCI registries or libguestfs.
//
// This is a placeholder. A full mock implementation will be added when the
// testing infrastructure is built out in Phase 1.
package mocks

import (
	"context"

	"github.com/CMGS/cocoon/image"
)

// MockManager is a test double for image.Manager. Each method field can be
// set to a custom function to control behavior in tests. Unset methods
// return zero values.
type MockManager struct {
	PullFunc              func(ctx context.Context, ref string) (*image.ImageIdentity, error)
	ConvertFunc           func(ctx context.Context, identity *image.ImageIdentity) (string, error)
	PrepareFunc           func(ctx context.Context, ref string) (*image.ImageIdentity, string, error)
	VerifyBootabilityFunc func(ctx context.Context, imagePath string) (*image.BootCheckResult, error)
	ListCachedFunc        func(ctx context.Context) ([]*image.CachedImage, error)
	RemoveCachedFunc      func(ctx context.Context, baseKey string) error
}

// Pull delegates to PullFunc if set, otherwise returns nil.
func (m *MockManager) Pull(ctx context.Context, ref string) (*image.ImageIdentity, error) {
	if m.PullFunc != nil {
		return m.PullFunc(ctx, ref)
	}
	return nil, nil
}

// Convert delegates to ConvertFunc if set, otherwise returns empty string.
func (m *MockManager) Convert(ctx context.Context, identity *image.ImageIdentity) (string, error) {
	if m.ConvertFunc != nil {
		return m.ConvertFunc(ctx, identity)
	}
	return "", nil
}

// Prepare delegates to PrepareFunc if set, otherwise returns nil values.
func (m *MockManager) Prepare(ctx context.Context, ref string) (*image.ImageIdentity, string, error) {
	if m.PrepareFunc != nil {
		return m.PrepareFunc(ctx, ref)
	}
	return nil, "", nil
}

// VerifyBootability delegates to VerifyBootabilityFunc if set, otherwise
// returns a default result with Bootable=true.
func (m *MockManager) VerifyBootability(ctx context.Context, imagePath string) (*image.BootCheckResult, error) {
	if m.VerifyBootabilityFunc != nil {
		return m.VerifyBootabilityFunc(ctx, imagePath)
	}
	return &image.BootCheckResult{Bootable: true}, nil
}

// ListCached delegates to ListCachedFunc if set, otherwise returns nil.
func (m *MockManager) ListCached(ctx context.Context) ([]*image.CachedImage, error) {
	if m.ListCachedFunc != nil {
		return m.ListCachedFunc(ctx)
	}
	return nil, nil
}

// RemoveCached delegates to RemoveCachedFunc if set, otherwise returns nil.
func (m *MockManager) RemoveCached(ctx context.Context, baseKey string) error {
	if m.RemoveCachedFunc != nil {
		return m.RemoveCachedFunc(ctx, baseKey)
	}
	return nil
}

// Compile-time interface compliance check.
var _ image.Manager = (*MockManager)(nil)

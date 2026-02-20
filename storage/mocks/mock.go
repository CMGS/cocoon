// Package mocks provides test doubles for the storage interfaces
// (ReferenceCounter, COWManager, GarbageCollector) for use in unit tests.
package mocks

import (
	"context"
	"time"

	"github.com/CMGS/cocoon/storage"
)

// ---------------------------------------------------------------------------
// MockReferenceCounter
// ---------------------------------------------------------------------------

// MockReferenceCounter is a test double for storage.ReferenceCounter.
// Each method can be overridden by setting the corresponding Func field.
// If a Func field is nil, the method returns zero values.
type MockReferenceCounter struct {
	AddReferenceFunc          func(baseKey, vmID, digestFull, sourceRef string) error
	RemoveReferenceFunc       func(baseKey, vmID string) error
	GetReferencesFunc         func(baseKey string) ([]string, error)
	IsReferencedFunc          func(baseKey string) (bool, error)
	GetUnreferencedImagesFunc func() ([]string, error)
}

// Compile-time check that MockReferenceCounter implements storage.ReferenceCounter.
var _ storage.ReferenceCounter = (*MockReferenceCounter)(nil)

func (m *MockReferenceCounter) AddReference(baseKey, vmID, digestFull, sourceRef string) error {
	if m.AddReferenceFunc != nil {
		return m.AddReferenceFunc(baseKey, vmID, digestFull, sourceRef)
	}
	return nil
}

func (m *MockReferenceCounter) RemoveReference(baseKey, vmID string) error {
	if m.RemoveReferenceFunc != nil {
		return m.RemoveReferenceFunc(baseKey, vmID)
	}
	return nil
}

func (m *MockReferenceCounter) GetReferences(baseKey string) ([]string, error) {
	if m.GetReferencesFunc != nil {
		return m.GetReferencesFunc(baseKey)
	}
	return []string{}, nil
}

func (m *MockReferenceCounter) IsReferenced(baseKey string) (bool, error) {
	if m.IsReferencedFunc != nil {
		return m.IsReferencedFunc(baseKey)
	}
	return false, nil
}

func (m *MockReferenceCounter) GetUnreferencedImages() ([]string, error) {
	if m.GetUnreferencedImagesFunc != nil {
		return m.GetUnreferencedImagesFunc()
	}
	return []string{}, nil
}

// ---------------------------------------------------------------------------
// MockCOWManager
// ---------------------------------------------------------------------------

// MockCOWManager is a test double for storage.COWManager.
// Each method can be overridden by setting the corresponding Func field.
// If a Func field is nil, the method returns zero values.
type MockCOWManager struct {
	CreateBaseImageFunc func(srcPath, baseKey string) error
	CreateOverlayFunc   func(ctx context.Context, baseKey, vmID, diskSize string) (string, error)
	RemoveOverlayFunc   func(vmID string) error
	GetOverlayInfoFunc  func(ctx context.Context, vmID string) (*storage.OverlayInfo, error)
	RepairOverlayFunc   func(ctx context.Context, vmID string) error
}

// Compile-time check that MockCOWManager implements storage.COWManager.
var _ storage.COWManager = (*MockCOWManager)(nil)

func (m *MockCOWManager) CreateBaseImage(srcPath, baseKey string) error {
	if m.CreateBaseImageFunc != nil {
		return m.CreateBaseImageFunc(srcPath, baseKey)
	}
	return nil
}

func (m *MockCOWManager) CreateOverlay(ctx context.Context, baseKey, vmID, diskSize string) (string, error) {
	if m.CreateOverlayFunc != nil {
		return m.CreateOverlayFunc(ctx, baseKey, vmID, diskSize)
	}
	return "", nil
}

func (m *MockCOWManager) RemoveOverlay(vmID string) error {
	if m.RemoveOverlayFunc != nil {
		return m.RemoveOverlayFunc(vmID)
	}
	return nil
}

func (m *MockCOWManager) GetOverlayInfo(ctx context.Context, vmID string) (*storage.OverlayInfo, error) {
	if m.GetOverlayInfoFunc != nil {
		return m.GetOverlayInfoFunc(ctx, vmID)
	}
	return nil, nil
}

func (m *MockCOWManager) RepairOverlay(ctx context.Context, vmID string) error {
	if m.RepairOverlayFunc != nil {
		return m.RepairOverlayFunc(ctx, vmID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// MockGarbageCollector
// ---------------------------------------------------------------------------

// MockGarbageCollector is a test double for storage.GarbageCollector.
// Each method can be overridden by setting the corresponding Func field.
// If a Func field is nil, the method returns zero values.
type MockGarbageCollector struct {
	CollectUnreferencedImagesFunc           func() ([]string, error)
	CollectOrphanedOverlaysFunc             func() ([]string, error)
	CollectOrphanedOCILayoutsFunc           func() ([]string, error)
	CollectStaleOCITagsFunc                 func() ([]string, error)
	CollectOrphanedOCIManifestRefsFunc      func() ([]string, error)
	CollectUnreferencedOCIBlobsFunc         func() ([]string, error)
	CollectUnreferencedOCIRuntimeCachesFunc func() ([]string, error)
	CollectStaleConversionLocksFunc         func(maxAge time.Duration) ([]string, error)
	CollectTempFilesFunc                    func(maxAge time.Duration) ([]string, error)
	FullGCFunc                              func() error
}

// Compile-time check that MockGarbageCollector implements storage.GarbageCollector.
var _ storage.GarbageCollector = (*MockGarbageCollector)(nil)

func (m *MockGarbageCollector) CollectUnreferencedImages() ([]string, error) {
	if m.CollectUnreferencedImagesFunc != nil {
		return m.CollectUnreferencedImagesFunc()
	}
	return []string{}, nil
}

func (m *MockGarbageCollector) CollectOrphanedOverlays() ([]string, error) {
	if m.CollectOrphanedOverlaysFunc != nil {
		return m.CollectOrphanedOverlaysFunc()
	}
	return []string{}, nil
}

func (m *MockGarbageCollector) CollectOrphanedOCILayouts() ([]string, error) {
	if m.CollectOrphanedOCILayoutsFunc != nil {
		return m.CollectOrphanedOCILayoutsFunc()
	}
	return []string{}, nil
}

func (m *MockGarbageCollector) CollectStaleOCITags() ([]string, error) {
	if m.CollectStaleOCITagsFunc != nil {
		return m.CollectStaleOCITagsFunc()
	}
	return []string{}, nil
}

func (m *MockGarbageCollector) CollectOrphanedOCIManifestRefs() ([]string, error) {
	if m.CollectOrphanedOCIManifestRefsFunc != nil {
		return m.CollectOrphanedOCIManifestRefsFunc()
	}
	return []string{}, nil
}

func (m *MockGarbageCollector) CollectUnreferencedOCIBlobs() ([]string, error) {
	if m.CollectUnreferencedOCIBlobsFunc != nil {
		return m.CollectUnreferencedOCIBlobsFunc()
	}
	return []string{}, nil
}

func (m *MockGarbageCollector) CollectUnreferencedOCIRuntimeCaches() ([]string, error) {
	if m.CollectUnreferencedOCIRuntimeCachesFunc != nil {
		return m.CollectUnreferencedOCIRuntimeCachesFunc()
	}
	return []string{}, nil
}

func (m *MockGarbageCollector) CollectStaleConversionLocks(maxAge time.Duration) ([]string, error) {
	if m.CollectStaleConversionLocksFunc != nil {
		return m.CollectStaleConversionLocksFunc(maxAge)
	}
	return []string{}, nil
}

func (m *MockGarbageCollector) CollectTempFiles(maxAge time.Duration) ([]string, error) {
	if m.CollectTempFilesFunc != nil {
		return m.CollectTempFilesFunc(maxAge)
	}
	return []string{}, nil
}

func (m *MockGarbageCollector) FullGC() error {
	if m.FullGCFunc != nil {
		return m.FullGCFunc()
	}
	return nil
}

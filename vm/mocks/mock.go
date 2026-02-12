// Package mocks provides test doubles for the vm.Manager interface.
//
// TODO: Generate mock implementation using mockgen or write manual mock.
// Example usage:
//
//	mgr := &mocks.MockManager{
//	    CreateFunc: func(ctx context.Context, opts *vm.CreateOptions) (*types.VMConfig, error) {
//	        return &types.VMConfig{VMID: "vm-test"}, nil
//	    },
//	}
package mocks

import (
	"context"
	"time"

	"github.com/projecteru2/cocoon/types"
	"github.com/projecteru2/cocoon/vm"
)

// MockManager is a test double for vm.Manager.
// Each method can be overridden by setting the corresponding Func field.
// If a Func field is nil, the method returns zero values.
type MockManager struct {
	CreateFunc          func(ctx context.Context, opts *vm.CreateOptions) (*types.VMConfig, error)
	StartFunc           func(ctx context.Context, vmID string) error
	StopFunc            func(ctx context.Context, vmID string, timeout time.Duration) error
	DeleteFunc          func(ctx context.Context, vmID string, force bool) error
	InspectFunc         func(ctx context.Context, vmID string) (*types.VMInspect, error)
	ListFunc            func(ctx context.Context) ([]*types.VMInspect, error)
	ResolveVMRefFunc    func(ref string) (string, error)
	TransitionStateFunc func(vmID string, to types.VMState, reason string) error
	LoadConfigFunc      func(vmID string) (*types.VMConfig, error)
	LoadMetadataFunc    func(vmID string) (*types.VMMetadataFile, error)
	SaveMetadataFunc    func(meta *types.VMMetadataFile) error
	ReconcileFunc       func(ctx context.Context, fix bool, force bool) ([]vm.Inconsistency, error)
}

// Compile-time check that MockManager implements vm.Manager.
var _ vm.Manager = (*MockManager)(nil)

func (m *MockManager) Create(ctx context.Context, opts *vm.CreateOptions) (*types.VMConfig, error) {
	if m.CreateFunc != nil {
		return m.CreateFunc(ctx, opts)
	}
	return nil, nil
}

func (m *MockManager) Start(ctx context.Context, vmID string) error {
	if m.StartFunc != nil {
		return m.StartFunc(ctx, vmID)
	}
	return nil
}

func (m *MockManager) Stop(ctx context.Context, vmID string, timeout time.Duration) error {
	if m.StopFunc != nil {
		return m.StopFunc(ctx, vmID, timeout)
	}
	return nil
}

func (m *MockManager) Delete(ctx context.Context, vmID string, force bool) error {
	if m.DeleteFunc != nil {
		return m.DeleteFunc(ctx, vmID, force)
	}
	return nil
}

func (m *MockManager) Inspect(ctx context.Context, vmID string) (*types.VMInspect, error) {
	if m.InspectFunc != nil {
		return m.InspectFunc(ctx, vmID)
	}
	return nil, nil
}

func (m *MockManager) List(ctx context.Context) ([]*types.VMInspect, error) {
	if m.ListFunc != nil {
		return m.ListFunc(ctx)
	}
	return nil, nil
}

func (m *MockManager) ResolveVMRef(ref string) (string, error) {
	if m.ResolveVMRefFunc != nil {
		return m.ResolveVMRefFunc(ref)
	}
	return "", nil
}

func (m *MockManager) TransitionState(vmID string, to types.VMState, reason string) error {
	if m.TransitionStateFunc != nil {
		return m.TransitionStateFunc(vmID, to, reason)
	}
	return nil
}

func (m *MockManager) LoadConfig(vmID string) (*types.VMConfig, error) {
	if m.LoadConfigFunc != nil {
		return m.LoadConfigFunc(vmID)
	}
	return nil, nil
}

func (m *MockManager) LoadMetadata(vmID string) (*types.VMMetadataFile, error) {
	if m.LoadMetadataFunc != nil {
		return m.LoadMetadataFunc(vmID)
	}
	return nil, nil
}

func (m *MockManager) SaveMetadata(meta *types.VMMetadataFile) error {
	if m.SaveMetadataFunc != nil {
		return m.SaveMetadataFunc(meta)
	}
	return nil
}

func (m *MockManager) Reconcile(ctx context.Context, fix bool, force bool) ([]vm.Inconsistency, error) {
	if m.ReconcileFunc != nil {
		return m.ReconcileFunc(ctx, fix, force)
	}
	return nil, nil
}

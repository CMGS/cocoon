// Package mocks provides test doubles for the hypervisor.Client interface.
//
// MockClient implements hypervisor.Client and allows per-method function
// override via exported Func fields. Methods with nil Func fields return
// zero values. Use it in unit tests that depend on the hypervisor package
// without requiring a real Cloud Hypervisor binary.
package mocks

import (
	"context"
	"time"

	"github.com/CMGS/cocoon/hypervisor"
	"github.com/CMGS/cocoon/types"
)

// MockClient is a test double for hypervisor.Client.
// Each method can be overridden by setting the corresponding Func field.
// If a Func field is nil, the method returns zero values.
type MockClient struct {
	LaunchFunc              func(ctx context.Context, vmID string, cfg *types.VMConfig) (int, error)
	ShutdownFunc            func(ctx context.Context, vmID string, timeout time.Duration) error
	ForceKillFunc           func(vmID string) error
	IsAliveFunc             func(vmID string) bool
	CreateVMFunc            func(ctx context.Context, socketPath string, vmCfg *hypervisor.CHVMConfig) error
	BootVMFunc              func(ctx context.Context, socketPath string) error
	ShutdownVMFunc          func(ctx context.Context, socketPath string) error
	PowerButtonFunc         func(ctx context.Context, socketPath string) error
	DeleteVMFunc            func(ctx context.Context, socketPath string) error
	GetVMInfoFunc           func(ctx context.Context, socketPath string) (*hypervisor.CHVMInfo, error)
	WaitForSocketFunc       func(ctx context.Context, socketPath string, timeout time.Duration) error
	CheckSocketConnFunc     func(socketPath string) error
}

// Compile-time check that MockClient implements hypervisor.Client.
var _ hypervisor.Client = (*MockClient)(nil)

func (m *MockClient) Launch(ctx context.Context, vmID string, cfg *types.VMConfig) (int, error) {
	if m.LaunchFunc != nil {
		return m.LaunchFunc(ctx, vmID, cfg)
	}
	return 0, nil
}

func (m *MockClient) Shutdown(ctx context.Context, vmID string, timeout time.Duration) error {
	if m.ShutdownFunc != nil {
		return m.ShutdownFunc(ctx, vmID, timeout)
	}
	return nil
}

func (m *MockClient) ForceKill(vmID string) error {
	if m.ForceKillFunc != nil {
		return m.ForceKillFunc(vmID)
	}
	return nil
}

func (m *MockClient) IsAlive(vmID string) bool {
	if m.IsAliveFunc != nil {
		return m.IsAliveFunc(vmID)
	}
	return false
}

func (m *MockClient) CreateVM(ctx context.Context, socketPath string, vmCfg *hypervisor.CHVMConfig) error {
	if m.CreateVMFunc != nil {
		return m.CreateVMFunc(ctx, socketPath, vmCfg)
	}
	return nil
}

func (m *MockClient) BootVM(ctx context.Context, socketPath string) error {
	if m.BootVMFunc != nil {
		return m.BootVMFunc(ctx, socketPath)
	}
	return nil
}

func (m *MockClient) ShutdownVM(ctx context.Context, socketPath string) error {
	if m.ShutdownVMFunc != nil {
		return m.ShutdownVMFunc(ctx, socketPath)
	}
	return nil
}

func (m *MockClient) PowerButton(ctx context.Context, socketPath string) error {
	if m.PowerButtonFunc != nil {
		return m.PowerButtonFunc(ctx, socketPath)
	}
	return nil
}

func (m *MockClient) DeleteVM(ctx context.Context, socketPath string) error {
	if m.DeleteVMFunc != nil {
		return m.DeleteVMFunc(ctx, socketPath)
	}
	return nil
}

func (m *MockClient) GetVMInfo(ctx context.Context, socketPath string) (*hypervisor.CHVMInfo, error) {
	if m.GetVMInfoFunc != nil {
		return m.GetVMInfoFunc(ctx, socketPath)
	}
	return nil, nil
}

func (m *MockClient) WaitForSocket(ctx context.Context, socketPath string, timeout time.Duration) error {
	if m.WaitForSocketFunc != nil {
		return m.WaitForSocketFunc(ctx, socketPath, timeout)
	}
	return nil
}

func (m *MockClient) CheckSocketConnectivity(socketPath string) error {
	if m.CheckSocketConnFunc != nil {
		return m.CheckSocketConnFunc(socketPath)
	}
	return nil
}

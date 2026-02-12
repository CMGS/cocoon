// Package vm provides VM lifecycle management for Cocoon.
//
// The Manager interface defines all operations for creating, starting,
// stopping, deleting, and inspecting VMs. It also handles state transitions,
// name resolution, and reconciliation.
//
// Key design principles:
//   - config.json is NEVER modified after creation (immutable).
//   - metadata.json is updated atomically (temp + fsync + rename) under flock.
//   - The name index is a derived cache rebuilt from config.json during reconcile.
//   - State transitions are validated before persisting.
//   - Idempotency: start RUNNING -> no-op, stop STOPPED -> no-op, delete non-existent -> no-op.
package vm

import (
	"context"
	"time"

	"github.com/projecteru2/cocoon/types"
)

// Manager is the interface for VM lifecycle operations.
type Manager interface {
	// CRUD operations.
	Create(ctx context.Context, opts *CreateOptions) (*types.VMConfig, error)
	Start(ctx context.Context, vmID string) error
	Stop(ctx context.Context, vmID string, timeout time.Duration) error
	Delete(ctx context.Context, vmID string, force bool) error
	Inspect(ctx context.Context, vmID string) (*types.VMInspect, error)
	List(ctx context.Context) ([]*types.VMInspect, error)

	// Name resolution.
	// If ref starts with "vm-", treat as a vm_id; otherwise look up name-index.json.
	ResolveVMRef(ref string) (string, error)

	// State management.
	TransitionState(vmID string, to types.VMState, reason string) error
	LoadConfig(vmID string) (*types.VMConfig, error)
	LoadMetadata(vmID string) (*types.VMMetadataFile, error)
	SaveMetadata(meta *types.VMMetadataFile) error

	// Reconciliation.
	Reconcile(ctx context.Context, fix bool, force bool) ([]Inconsistency, error)
}

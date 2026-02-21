// Package hypervisor provides an interface and implementation for managing
// Cloud Hypervisor (CH) processes and communicating with their REST API
// over Unix domain sockets.
//
// Architecture: Cocoon uses one CH process per VM for strong isolation.
// Each CH process exposes its own API socket at /run/cocoon/vms/{vm-id}/api.sock.
// The Client interface abstracts both process management (launch, kill, monitor)
// and the CH HTTP API (vm.create, vm.boot, vm.shutdown, etc.).
package hypervisor

import (
	"context"
	"time"

	"github.com/CMGS/cocoon/types"
)

// Client is the interface for managing a Cloud Hypervisor instance.
// It covers two concerns:
//
//  1. Process management: launching the CH binary, monitoring liveness,
//     graceful/forceful shutdown.
//  2. CH REST API: communicating with a running CH process over its
//     Unix socket (PUT /api/v1/vm.create, vm.boot, etc.).
//
// All blocking operations accept a context.Context for cancellation and
// timeout propagation.
type Client interface {
	// --- Process management ---

	// Launch starts a new Cloud Hypervisor process for the given VM.
	// It creates the required runtime directories, starts the CH binary with
	// the appropriate flags, waits for the API socket to appear, and writes
	// the PID file. On success it returns the CH process PID.
	Launch(ctx context.Context, vmID string, cfg *types.VMConfig) (pid int, err error)

	// Shutdown performs a graceful shutdown sequence for UEFI-boot VMs:
	//   1. Send ACPI power-button event via the CH API.
	//   2. Poll until the CH process exits or the timeout expires.
	//   3. Fallback: vm.shutdown API + SIGTERM + SIGKILL.
	Shutdown(ctx context.Context, vmID string, timeout time.Duration) error

	// ShutdownDirect stops a VM without ACPI (for direct kernel boot VMs
	// whose guest kernel may lack ACPI support). Sequence:
	//   1. vm.shutdown API (stop vCPUs, flush backends).
	//   2. SIGTERM the CH process, wait up to gracePeriod for exit.
	//   3. SIGKILL as last resort.
	ShutdownDirect(ctx context.Context, vmID string, gracePeriod time.Duration) error

	// ForceKill sends SIGKILL to the CH process for the given VM.
	// It is a best-effort operation; if the process has already exited
	// the error is silently ignored.
	ForceKill(vmID string) error

	// IsAlive reports whether the CH process for the given VM is still running.
	// It reads the PID from the PID file and probes it with signal 0.
	IsAlive(vmID string) bool

	// --- CH REST API over Unix socket ---

	// CreateVM sends PUT /api/v1/vm.create with the provided configuration.
	// The socketPath identifies which CH instance to talk to.
	CreateVM(ctx context.Context, socketPath string, vmCfg *CHVMConfig) error

	// BootVM sends PUT /api/v1/vm.boot (no body) to start the guest.
	BootVM(ctx context.Context, socketPath string) error

	// ShutdownVM sends PUT /api/v1/vm.shutdown to request a clean guest shutdown.
	ShutdownVM(ctx context.Context, socketPath string) error

	// PowerButton sends PUT /api/v1/vm.power-button (ACPI power-button event).
	PowerButton(ctx context.Context, socketPath string) error

	// DeleteVM sends PUT /api/v1/vm.delete to release all VM resources inside CH.
	DeleteVM(ctx context.Context, socketPath string) error

	// GetVMInfo sends GET /api/v1/vm.info and returns the parsed response.
	GetVMInfo(ctx context.Context, socketPath string) (*CHVMInfo, error)

	// GetConsolePTYPath retrieves the virtio-console PTY path for a running VM.
	// Returns an empty string (no error) if the console is not in Pty mode.
	GetConsolePTYPath(ctx context.Context, socketPath string) (string, error)

	// --- Utilities ---

	// WaitForSocket blocks until the Unix socket at socketPath exists and is
	// connectable, or the timeout expires.
	WaitForSocket(ctx context.Context, socketPath string, timeout time.Duration) error

	// CheckSocketConnectivity performs a single connectivity check against the
	// given Unix socket (dial + close).
	CheckSocketConnectivity(socketPath string) error
}

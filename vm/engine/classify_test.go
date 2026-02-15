package engine

import (
	"fmt"
	"testing"

	"github.com/CMGS/cocoon/types"
)

func TestClassifyError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		reason string
		want   types.ErrorType
	}{
		// Boot timeout variations
		{
			name:   "boot timeout from waitForBoot",
			reason: "boot failed: boot failure detected: boot timeout: no boot completion detected within 30s",
			want:   types.ErrorBootTimeout,
		},
		{
			name:   "boot timeout serial log missing",
			reason: "boot failed: boot failure detected: boot timeout: serial log file /tmp/serial.log did not appear",
			want:   types.ErrorBootTimeout,
		},

		// Kernel panic
		{
			name:   "kernel panic in serial log",
			reason: `boot failed: boot failure detected: "Kernel panic - not syncing: VFS" matched pattern "Kernel panic"`,
			want:   types.ErrorKernelPanic,
		},

		// Generic boot failure (failure pattern match that is not panic/timeout)
		{
			name:   "boot failure detected generic",
			reason: `boot failed: boot failure detected: "some failure" matched pattern "fail"`,
			want:   types.ErrorKernelPanic,
		},

		// CH launch failure
		{
			name:   "launch CH failure",
			reason: "boot failed: launch CH for vm-ABC123 (uefi): exec: cloud-hypervisor: not found",
			want:   types.ErrorCHCrash,
		},

		// CH create VM config failure
		{
			name:   "create VM config failure",
			reason: "boot failed: create VM config for vm-ABC123 (uefi): connection refused",
			want:   types.ErrorCHCrash,
		},

		// CH boot VM failure
		{
			name:   "boot VM failure",
			reason: "boot failed: boot VM vm-ABC123 (uefi): 500 Internal Server Error",
			want:   types.ErrorCHCrash,
		},

		// Force kill during start
		{
			name:   "force killed during start",
			reason: "force killed during start",
			want:   types.ErrorCHCrash,
		},

		// Graceful stop failure
		{
			name:   "graceful stop failed",
			reason: "graceful stop failed: timeout waiting for process exit after 30s",
			want:   types.ErrorStopTimeout,
		},

		// Stop transition failure
		{
			name:   "stop transition failed",
			reason: "stop transition failed: acquire metadata lock for vm-ABC123: resource busy",
			want:   types.ErrorStopTimeout,
		},

		// Force stop failure during delete
		{
			name:   "force stop failed during delete",
			reason: "force stop failed: shutdown VM vm-ABC123: timeout",
			want:   types.ErrorStopTimeout,
		},

		// Start transition failure
		{
			name:   "start transition failed",
			reason: "start transition failed: acquire metadata lock for vm-ABC123: resource busy",
			want:   types.ErrorCHCrash,
		},

		// Reconciliation crash
		{
			name:   "reconciliation detected crash",
			reason: "reconciliation: was RUNNING but actual state is ERROR (process dead or PID reused)",
			want:   types.ErrorCHCrash,
		},

		// OCI conversion
		{
			name:   "OCI conversion failure",
			reason: "prepare image: oci conversion failed: buildah push: exit code 1",
			want:   types.ErrorOCIConversion,
		},

		// Disk creation
		{
			name:   "disk creation failure",
			reason: "disk creation failed: qemu-img create: exit code 1",
			want:   types.ErrorDiskCreation,
		},

		// Image not bootable
		{
			name:   "image not bootable",
			reason: "image is not bootable: no kernel found",
			want:   types.ErrorImageNotBootable,
		},

		// Missing bootloader
		{
			name:   "missing bootloader",
			reason: "boot failed: missing bootloader in image",
			want:   types.ErrorMissingBootloader,
		},

		// Missing kernel
		{
			name:   "missing kernel",
			reason: "boot failed: missing kernel in image",
			want:   types.ErrorMissingKernel,
		},

		// Insufficient disk space
		{
			name:   "insufficient disk space",
			reason: "insufficient disk space: need 10GB, have 2GB",
			want:   types.ErrorInsufficientDisk,
		},

		// Resource exhaustion
		{
			name:   "resource exhaustion",
			reason: "resource exhaustion: cannot allocate memory",
			want:   types.ErrorResourceExhaustion,
		},
		{
			name:   "out of memory",
			reason: "VM crashed: out of memory",
			want:   types.ErrorResourceExhaustion,
		},

		// Unknown / no match
		{
			name:   "unknown error returns empty",
			reason: "something completely unexpected happened",
			want:   "",
		},
		{
			name:   "empty reason returns empty",
			reason: "",
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := classifyError(tt.reason)
			if got != tt.want {
				t.Errorf("classifyError(%q) = %q, want %q", tt.reason, got, tt.want)
			}
		})
	}
}

// TestClassifyError_RealTransitionReasons verifies classification of the exact
// reason strings produced by manager.go when transitioning to ERROR state.
func TestClassifyError_RealTransitionReasons(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		reason string
		want   types.ErrorType
	}{
		{
			name:   "Start: boot failed with boot error",
			reason: fmt.Sprintf("boot failed: %v", fmt.Errorf("launch CH for vm-X (uefi): exec error")),
			want:   types.ErrorCHCrash,
		},
		{
			name:   "Start: start transition failed",
			reason: fmt.Sprintf("start transition failed: %v", fmt.Errorf("lock busy")),
			want:   types.ErrorCHCrash,
		},
		{
			name:   "Stop: graceful stop failed",
			reason: fmt.Sprintf("graceful stop failed: %v", fmt.Errorf("timeout")),
			want:   types.ErrorStopTimeout,
		},
		{
			name:   "Stop: stop transition failed",
			reason: fmt.Sprintf("stop transition failed: %v", fmt.Errorf("lock busy")),
			want:   types.ErrorStopTimeout,
		},
		{
			name:   "Kill: force killed during start",
			reason: "force killed during start",
			want:   types.ErrorCHCrash,
		},
		{
			name:   "Delete: force stop failed",
			reason: fmt.Sprintf("force stop failed: %v", fmt.Errorf("shutdown timeout")),
			want:   types.ErrorStopTimeout,
		},
		{
			name:   "boot failed with timeout",
			reason: fmt.Sprintf("boot failed: boot failure detected: %v", fmt.Errorf("boot timeout: no boot completion detected within 30s")),
			want:   types.ErrorBootTimeout,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := classifyError(tt.reason)
			if got != tt.want {
				t.Errorf("classifyError(%q) = %q, want %q", tt.reason, got, tt.want)
			}
		})
	}
}

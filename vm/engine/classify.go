package engine

import (
	"strings"

	"github.com/CMGS/cocoon/types"
)

// classifyError examines an error reason string (as passed to TransitionState)
// and returns the most specific ErrorType constant. It uses substring matching
// on the reason text, which is the only information available at the
// transitionStateWithUpdate call site.
//
// Returns an empty string when the reason does not match any known pattern.
func classifyError(reason string) types.ErrorType {
	lower := strings.ToLower(reason)

	// Boot timeout: produced by waitForBoot when no success pattern matches
	// within the configured timeout, or when the serial log file never appears.
	if strings.Contains(lower, "boot timeout") {
		return types.ErrorBootTimeout
	}

	// Kernel panic: failure pattern matched in serial log during boot monitoring.
	if strings.Contains(lower, "kernel panic") {
		return types.ErrorKernelPanic
	}

	// Boot failure detected: waitForBoot matched a failure pattern (e.g.,
	// missing init, failed to execute /init). If it was a kernel panic, the
	// check above already caught it, so this is a generic boot failure.
	if strings.Contains(lower, "boot failure detected") {
		return types.ErrorBootFailure
	}

	// Cloud Hypervisor launch failures.
	if strings.Contains(lower, "launch ch") {
		return types.ErrorCHCrash
	}

	// CH REST API configuration or boot command failures.
	if strings.Contains(lower, "create vm config") || strings.Contains(lower, "boot vm") {
		return types.ErrorCHCrash
	}

	// Force kill during start: CH process killed while in STARTING state.
	if strings.Contains(lower, "force kill") {
		return types.ErrorCHCrash
	}

	// Graceful stop failures (shutdown timeout or ACPI shutdown error).
	if strings.Contains(lower, "graceful stop failed") {
		return types.ErrorStopTimeout
	}

	// Stop transition failure: the VM was running but the metadata transition
	// from STOPPING -> STOPPED failed, so the VM was force-killed.
	if strings.Contains(lower, "stop transition failed") {
		return types.ErrorStopTimeout
	}

	// Force stop failure during delete (ensureDeletePreconditions).
	if strings.Contains(lower, "force stop failed") {
		return types.ErrorStopTimeout
	}

	// Start transition failure: boot succeeded but STARTING -> RUNNING transition failed.
	if strings.Contains(lower, "start transition failed") {
		return types.ErrorCHCrash
	}

	// Reconciliation-detected crashes: metadata said RUNNING/STARTING/STOPPING
	// but process was dead or stuck.
	if strings.Contains(lower, "reconciliation") {
		return types.ErrorCHCrash
	}

	// OCI conversion errors (from image preparation).
	if strings.Contains(lower, "oci") && strings.Contains(lower, "conver") {
		return types.ErrorOCIConversion
	}

	// Disk creation errors.
	if strings.Contains(lower, "disk") && strings.Contains(lower, "creat") {
		return types.ErrorDiskCreation
	}

	// Image not bootable.
	if strings.Contains(lower, "not bootable") || strings.Contains(lower, "image_not_bootable") {
		return types.ErrorImageNotBootable
	}

	// Missing bootloader.
	if strings.Contains(lower, "missing bootloader") {
		return types.ErrorMissingBootloader
	}

	// Missing kernel.
	if strings.Contains(lower, "missing kernel") {
		return types.ErrorMissingKernel
	}

	// Insufficient disk space.
	if strings.Contains(lower, "insufficient") && strings.Contains(lower, "disk") {
		return types.ErrorInsufficientDisk
	}

	// Resource exhaustion.
	if strings.Contains(lower, "resource exhaustion") || strings.Contains(lower, "out of memory") || strings.Contains(lower, "oom") {
		return types.ErrorResourceExhaustion
	}

	// No match: return empty string (unknown/unclassified error).
	return ""
}

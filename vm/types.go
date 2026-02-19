package vm

import "github.com/CMGS/cocoon/types"

// CreateOptions holds parameters for creating a new VM.
type CreateOptions struct {
	// Image is the OCI image reference (required).
	Image string `json:"image"`

	// Name is the optional user-facing alias.
	// If empty, auto-generated as "cocoon-{random-8-chars}".
	Name string `json:"name,omitempty"`

	// CPUs is the number of vCPUs. Defaults to config default if zero.
	CPUs int `json:"cpus,omitempty"`

	// MemoryMB is the memory in megabytes. Defaults to config default if zero.
	MemoryMB int64 `json:"memory_mb,omitempty"`

	// DiskSize is the overlay disk size (e.g., "10G"). Defaults to config default if empty.
	DiskSize string `json:"disk_size,omitempty"`

	// BootStrategy is one of "uefi" (default) or "direct" (OCI direct kernel boot).
	BootStrategy types.BootStrategy `json:"boot_strategy,omitempty"`

	// SkipVerify skips bootability verification during create.
	// Useful for known-good images or when guestfish is unavailable.
	SkipVerify bool `json:"skip_verify,omitempty"`

	// EnableTPM enables TPM 2.0 emulation via swtpm.
	EnableTPM bool `json:"enable_tpm,omitempty"`
}

// InconsistencyType classifies the kind of reconciliation inconsistency.
type InconsistencyType string

const (
	InconsistencyStateMismatch         InconsistencyType = "state_mismatch"
	InconsistencyMetadataCorrupt       InconsistencyType = "metadata_corrupted"
	InconsistencyStalePIDFile          InconsistencyType = "stale_pid_file"
	InconsistencyZombieSocket          InconsistencyType = "zombie_socket"
	InconsistencyZombieProcess         InconsistencyType = "zombie_process"
	InconsistencyMissingOverlay        InconsistencyType = "missing_overlay"
	InconsistencyOrphanedOverlay       InconsistencyType = "orphaned_overlay"
	InconsistencyOCIRuntimeCache       InconsistencyType = "oci_runtime_cache_missing"
	InconsistencyOCIRuntimeOverlay     InconsistencyType = "oci_runtime_overlay_mismatch"
	InconsistencyOCIRuntimeVirtio      InconsistencyType = "oci_runtime_virtiofsd_mismatch"
	InconsistencyMissingOCIRuntimePin  InconsistencyType = "missing_oci_runtime_pin"
	InconsistencyDanglingOCIRuntimePin InconsistencyType = "dangling_oci_runtime_pin"
	InconsistencyOrphanedOCIRuntime    InconsistencyType = "orphaned_oci_runtime_cache"
	InconsistencyMissingReference      InconsistencyType = "missing_reference"
	InconsistencyDanglingReference     InconsistencyType = "dangling_reference"
	InconsistencyNameIndexStale        InconsistencyType = "name_index_stale"
	InconsistencyDuplicateVMName       InconsistencyType = "duplicate_vm_name"
	InconsistencyDeletedVMDir          InconsistencyType = "deleted_vm_directory"
)

// InconsistencySeverity indicates how serious an inconsistency is.
type InconsistencySeverity string

const (
	SeverityCritical InconsistencySeverity = "critical"
	SeverityWarning  InconsistencySeverity = "warning"
	SeverityInfo     InconsistencySeverity = "info"
)

// Inconsistency represents a detected discrepancy between expected and actual VM state.
type Inconsistency struct {
	// VMID is the VM that has the inconsistency.
	VMID string `json:"vm_id"`

	// Type classifies the inconsistency.
	Type InconsistencyType `json:"type"`

	// Severity indicates how serious the issue is.
	Severity InconsistencySeverity `json:"severity"`

	// Details is a human-readable description of the problem.
	Details string `json:"details"`

	// BaseKey is used for reference reconciliation issues.
	BaseKey string `json:"base_key,omitempty"`

	// DigestFull is the full image digest associated with a VM config.
	DigestFull string `json:"digest_full,omitempty"`

	// ImageRef is the original image reference associated with a VM config.
	ImageRef string `json:"image_ref,omitempty"`

	// ExpectedState is the state recorded in metadata.json.
	ExpectedState string `json:"expected_state,omitempty"`

	// ActualState is the state determined by probing the system.
	ActualState string `json:"actual_state,omitempty"`
}

// NameIndex is an alias for types.NameIndex.
// Maps VM names to their vm_id values.
// Stored at /var/lib/cocoon/db/name-index.json.
type NameIndex = types.NameIndex

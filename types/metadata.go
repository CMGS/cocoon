package types

// VMMetadataFile represents the mutable runtime state (metadata.json on disk).
// Updated on every state transition.
type VMMetadataFile struct {
	VMID          string `json:"vm_id"`
	State         string `json:"state"`
	PreviousState string `json:"previous_state"`

	// Runtime (changes with each start/stop cycle)
	ProcessPID       int    `json:"process_pid,omitempty"`
	HypervisorBinary string `json:"hypervisor_binary,omitempty"`
	BootTime         string `json:"boot_time,omitempty"`
	LastBootMode     string `json:"last_boot_mode,omitempty"`
	LastFirmwarePath string `json:"last_firmware_path,omitempty"`

	// Error tracking
	LastError     string `json:"last_error,omitempty"`
	LastErrorType string `json:"last_error_type,omitempty"`
	LastErrorAt   string `json:"last_error_at,omitempty"`
	ErrorCount    int    `json:"error_count"`

	// Lifecycle flags
	AutoRemove bool `json:"auto_remove,omitempty"`

	// Timestamps (RFC 3339)
	UpdatedAt string `json:"updated_at"`
	StartedAt string `json:"started_at,omitempty"`
	StoppedAt string `json:"stopped_at,omitempty"`

	// Schema version
	SchemaVersion int `json:"schema_version"`
}

// CurrentMetadataSchemaVersion is the current metadata.json schema version.
const CurrentMetadataSchemaVersion = 1

// DefaultHypervisorProcess is the fallback process name used when
// VMMetadataFile.HypervisorBinary is empty (backward compatibility with
// VMs created before the field was introduced).
const DefaultHypervisorProcess = "cloud-hypervisor"

// HypervisorProcessName returns the expected process name for this VM.
// It uses HypervisorBinary from metadata if set, otherwise falls back to
// the configured binary name. If both are empty, returns the default.
func (m *VMMetadataFile) HypervisorProcessName(configBinary string) string {
	if m != nil && m.HypervisorBinary != "" {
		return m.HypervisorBinary
	}
	if configBinary != "" {
		return configBinary
	}
	return DefaultHypervisorProcess
}

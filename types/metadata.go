package types

// VMMetadataFile represents the mutable runtime state (metadata.json on disk).
// Updated on every state transition.
type VMMetadataFile struct {
	VMID          string `json:"vm_id"`
	State         string `json:"state"`
	PreviousState string `json:"previous_state"`

	// Runtime (changes with each start/stop cycle)
	ProcessPID       int    `json:"process_pid,omitempty"`
	BootTime         string `json:"boot_time,omitempty"`
	LastBootMode     string `json:"last_boot_mode,omitempty"`
	LastFirmwarePath string `json:"last_firmware_path,omitempty"`

	// Error tracking
	LastError  string `json:"last_error,omitempty"`
	ErrorCount int    `json:"error_count"`

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

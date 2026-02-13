package types

// VMInspect is the merged view returned by "cocoon inspect".
// Combines data from config.json (immutable) and metadata.json (mutable).
// This struct is never persisted as a single file.
type VMInspect struct {
	VMID  string  `json:"vm_id"`
	Name  string  `json:"name"`
	State VMState `json:"state"`

	Image      InspectImageInfo      `json:"image"`
	Storage    InspectStorageInfo    `json:"storage"`
	Hypervisor InspectHypervisorInfo `json:"hypervisor"`
	BootConfig InspectBootConfig     `json:"boot_config"`
	Timestamps InspectTimestamps     `json:"timestamps"`
	Runtime    InspectRuntimeStatus  `json:"runtime"`
	Error      *InspectErrorInfo     `json:"error,omitempty"`
}

// InspectImageInfo contains OCI image details.
type InspectImageInfo struct {
	Ref     string `json:"ref"`
	Digest  string `json:"digest"`
	BaseKey string `json:"base_key"`
}

// InspectStorageInfo contains disk information.
type InspectStorageInfo struct {
	OverlayPath string `json:"overlay_path"`
	BasePath    string `json:"base_path"`
	Size        string `json:"size"`
}

// InspectHypervisorInfo contains Cloud Hypervisor details.
type InspectHypervisorInfo struct {
	CHSocket  string `json:"ch_socket"`
	CHPID     int    `json:"ch_pid"`
	SerialLog string `json:"serial_log"`
}

// InspectBootConfig contains boot configuration.
type InspectBootConfig struct {
	CPUs         int          `json:"cpus"`
	MemoryMB     int64        `json:"memory_mb"`
	BootStrategy BootStrategy `json:"boot_strategy"`
	FirmwarePath string       `json:"firmware_path"`
}

// InspectTimestamps tracks VM lifecycle events.
type InspectTimestamps struct {
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	StartedAt string `json:"started_at,omitempty"`
	StoppedAt string `json:"stopped_at,omitempty"`
}

// InspectRuntimeStatus contains runtime execution information.
type InspectRuntimeStatus struct {
	BootTime     string `json:"boot_time,omitempty"`
	LastBootMode string `json:"last_boot_mode,omitempty"`
	ErrorCount   int    `json:"error_count"`
}

// InspectErrorInfo contains error details.
type InspectErrorInfo struct {
	Message   string `json:"message"`
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
}

// BuildInspect constructs a VMInspect from config.json and metadata.json data.
func BuildInspect(cfg *VMConfig, meta *VMMetadataFile) *VMInspect {
	inspect := &VMInspect{
		VMID:  cfg.VMID,
		Name:  cfg.Name,
		State: VMState(meta.State),
		Image: InspectImageInfo{
			Ref:     cfg.ImageRef,
			Digest:  cfg.BaseDigestFull,
			BaseKey: cfg.BaseKey,
		},
		Storage: InspectStorageInfo{
			OverlayPath: cfg.OverlayPath,
			BasePath:    cfg.BaseImagePath,
			Size:        cfg.DiskSize,
		},
		Hypervisor: InspectHypervisorInfo{
			CHSocket:  cfg.SocketPath,
			CHPID:     meta.ProcessPID,
			SerialLog: cfg.SerialLog,
		},
		BootConfig: InspectBootConfig{
			CPUs:         cfg.CPUs,
			MemoryMB:     cfg.MemoryMB,
			BootStrategy: cfg.BootStrategy,
			FirmwarePath: cfg.FirmwarePath,
		},
		Timestamps: InspectTimestamps{
			CreatedAt: cfg.CreatedAt,
			UpdatedAt: meta.UpdatedAt,
			StartedAt: meta.StartedAt,
			StoppedAt: meta.StoppedAt,
		},
		Runtime: InspectRuntimeStatus{
			BootTime:     meta.BootTime,
			LastBootMode: meta.LastBootMode,
			ErrorCount:   meta.ErrorCount,
		},
	}
	if meta.LastError != "" {
		inspect.Error = &InspectErrorInfo{
			Message:   meta.LastError,
			Type:      meta.LastErrorType,
			Timestamp: meta.LastErrorAt,
		}
	}
	return inspect
}

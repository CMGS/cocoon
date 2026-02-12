package hypervisor

// CHVMConfig is the request body for PUT /api/v1/vm.create.
// Field names and JSON tags follow the Cloud Hypervisor REST API schema.
type CHVMConfig struct {
	CPUs    CHCPUConfig     `json:"cpus"`
	Memory  CHMemoryConfig  `json:"memory"`
	Disks   []CHDiskConfig  `json:"disks,omitempty"`
	Serial  CHSerialConfig  `json:"serial"`
	Console CHConsoleConfig `json:"console"`
}

// CHCPUConfig specifies the number of virtual CPUs.
type CHCPUConfig struct {
	BootVCPUs int `json:"boot_vcpus"`
}

// CHMemoryConfig specifies the guest memory size.
type CHMemoryConfig struct {
	// Size is the memory allocation in bytes.
	Size int64 `json:"size"`
}

// CHDiskConfig describes a single block device attached to the VM.
type CHDiskConfig struct {
	Path     string `json:"path"`
	ReadOnly bool   `json:"readonly,omitempty"`
}

// CHSerialConfig controls the serial console output.
//
// Supported modes (per CH documentation):
//   - "Null"  : discard all output
//   - "Tty"   : connect to a TTY
//   - "File"  : write output to File
type CHSerialConfig struct {
	Mode string `json:"mode"`
	File string `json:"file,omitempty"`
}

// CHConsoleConfig controls the virtio console.
//
// Supported modes:
//   - "Off"  : no console
//   - "Tty"  : connect to a TTY
//   - "File" : write output to a file
type CHConsoleConfig struct {
	Mode string `json:"mode"`
}

// CHVMInfo is the response body from GET /api/v1/vm.info.
type CHVMInfo struct {
	Config           CHVMConfig `json:"config"`
	State            string     `json:"state"` // "Created", "Running", "Shutdown", "Paused"
	MemoryActualSize int64      `json:"memory_actual_size"`
}

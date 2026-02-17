package hypervisor

// CHVMConfig is the request body for PUT /api/v1/vm.create.
// Field names and JSON tags follow the Cloud Hypervisor REST API schema.
type CHVMConfig struct {
	Payload *CHPayloadConfig `json:"payload,omitempty"`
	CPUs    CHCPUConfig      `json:"cpus"`
	Memory  CHMemoryConfig   `json:"memory"`
	Disks   []CHDiskConfig   `json:"disks,omitempty"`
	Serial  CHSerialConfig   `json:"serial"`
	Console CHConsoleConfig  `json:"console"`
	TPM     *CHTPMConfig     `json:"tpm,omitempty"`
}

// CHTPMConfig specifies the TPM 2.0 emulator socket path.
type CHTPMConfig struct {
	Socket string `json:"socket"`
}

// CHPayloadConfig specifies the boot payload for the VM.
// For UEFI boot, set the Firmware field (CLOUDHV.fd).
// For direct kernel boot (OCI images), set Kernel, Initramfs, and Cmdline.
type CHPayloadConfig struct {
	Firmware  string `json:"firmware,omitempty"`
	Kernel    string `json:"kernel,omitempty"`
	Initramfs string `json:"initramfs,omitempty"`
	Cmdline   string `json:"cmdline,omitempty"`
}

// CHCPUConfig specifies the number of virtual CPUs.
type CHCPUConfig struct {
	BootVCPUs int `json:"boot_vcpus"`
	// MaxVCPUs is required by newer Cloud Hypervisor API versions.
	// For now we pin it to boot_vcpus (no CPU hotplug in Phase 1).
	MaxVCPUs int `json:"max_vcpus"`
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
//   - "Pty"  : allocate a PTY (path reported in vm.info)
//   - "Tty"  : connect to a TTY
//   - "File" : write output to a file
type CHConsoleConfig struct {
	Mode string `json:"mode"`
	File string `json:"file,omitempty"` // Populated by CH when mode is "Pty" or "File"
}

// CHVMInfo is the response body from GET /api/v1/vm.info.
type CHVMInfo struct {
	Config           CHVMConfig `json:"config"`
	State            string     `json:"state"` // "Created", "Running", "Shutdown", "Paused"
	MemoryActualSize int64      `json:"memory_actual_size"`
}

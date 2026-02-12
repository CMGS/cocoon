package cloudhypervisor

import (
	"os/exec"
	"syscall"
)

// configureCHProcess sets process attributes for the Cloud Hypervisor process.
// Setpgid creates a new process group so CH survives if cocoon exits.
func configureCHProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
}

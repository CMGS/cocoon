//go:build linux

package hypervisor

import (
	"os/exec"
	"syscall"
)

// configureCHProcess sets Linux-specific process attributes for the CH process.
// Setpgid creates a new process group so CH survives if cocoon exits.
func configureCHProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
}

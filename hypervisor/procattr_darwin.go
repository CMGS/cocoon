//go:build darwin

package hypervisor

import (
	"os/exec"
	"syscall"
)

// configureCHProcess sets macOS-specific process attributes.
// Setpgid is also available on macOS.
func configureCHProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
}

//go:build darwin

package utils

import (
	"bytes"
	"strings"

	"golang.org/x/sys/unix"
)

// validateProcessImpl uses sysctl to verify the process name on macOS.
func validateProcessImpl(pid int, expectedName string) bool {
	kinfo, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		// If sysctl fails, we cannot verify the process identity. Return false
		// (fail-safe) to prevent sending signals to a wrong process due to
		// PID reuse.
		return false
	}

	raw := kinfo.Proc.P_comm[:]
	if end := bytes.IndexByte(raw, 0); end >= 0 {
		raw = raw[:end]
	}
	actual := strings.TrimSpace(string(raw))
	return strings.Contains(actual, expectedName) || strings.HasPrefix(expectedName, actual)
}

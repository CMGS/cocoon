//go:build darwin

package utils

import (
	"fmt"
	"os/exec"
	"strings"
)

// validateProcessImpl uses `ps` to verify the process name on macOS.
func validateProcessImpl(pid int, expectedName string) bool {
	out, err := exec.Command("ps", "-p", fmt.Sprintf("%d", pid), "-o", "comm=").Output() //nolint:gosec // G204: pid is an integer, not arbitrary user input
	if err != nil {
		// If ps fails, we cannot verify the process identity. Return false
		// (fail-safe) to prevent sending signals to a wrong process due to
		// PID reuse.
		return false
	}
	actual := strings.TrimSpace(string(out))
	return strings.Contains(actual, expectedName)
}

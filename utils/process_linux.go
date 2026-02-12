//go:build linux

package utils

import (
	"fmt"
	"os"
	"strings"
)

// validateProcessImpl checks /proc/pid/comm to verify the process name.
func validateProcessImpl(pid int, expectedName string) bool {
	commPath := fmt.Sprintf("/proc/%d/comm", pid)
	data, err := os.ReadFile(commPath) //nolint:gosec // commPath is constructed from a numeric PID, not user-controlled input
	if err != nil {
		return false
	}
	actual := strings.TrimSpace(string(data))
	return strings.Contains(actual, expectedName)
}

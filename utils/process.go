package utils

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// IsProcessAlive checks if a process with the given PID exists.
func IsProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}

// ValidateProcess checks if a process is alive AND matches the expected name.
// This guards against PID reuse after a crash.
// Implementation is platform-specific (Linux reads /proc, macOS uses ps).
func ValidateProcess(pid int, expectedName string) bool {
	if !IsProcessAlive(pid) {
		return false
	}
	return validateProcessImpl(pid, expectedName)
}

// ForceKillProcess sends SIGKILL to a process and polls until it exits.
// After Release(), the process is no longer a child, so Wait() would fail
// with ECHILD. Instead we poll IsProcessAlive with a short timeout.
func ForceKillProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process %d: %w", pid, err)
	}
	if err := process.Signal(syscall.SIGKILL); err != nil {
		return fmt.Errorf("kill process %d: %w", pid, err)
	}
	// Poll for exit — SIGKILL is unconditional but the kernel needs a
	// brief window to tear down the process.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !IsProcessAlive(pid) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("process %d still alive after SIGKILL timeout", pid)
}

// WritePIDFile writes a PID to a file.
func WritePIDFile(path string, pid int) error {
	return os.WriteFile(path, []byte(strconv.Itoa(pid)), 0o644) //nolint:gosec // G306: PID file needs to be readable by other cocoon processes
}

// ReadPIDFile reads a PID from a file.
func ReadPIDFile(path string) (int, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: PID file path is an internal runtime path
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

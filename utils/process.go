package utils

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
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

// ForceKillProcess sends SIGKILL to a process and waits for exit.
func ForceKillProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process %d: %w", pid, err)
	}
	if err := process.Signal(syscall.SIGKILL); err != nil {
		return fmt.Errorf("kill process %d: %w", pid, err)
	}
	_, _ = process.Wait()
	return nil
}

// WritePIDFile writes a PID to a file.
func WritePIDFile(path string, pid int) error {
	return os.WriteFile(path, []byte(strconv.Itoa(pid)), 0644)
}

// ReadPIDFile reads a PID from a file.
func ReadPIDFile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

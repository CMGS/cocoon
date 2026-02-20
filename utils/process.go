package utils

import (
	"context"
	"fmt"
	"os"
	"os/exec"
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

// CommandContextWithGroup creates a command that runs in its own process group.
// When the context is canceled, the entire process group is killed with SIGKILL.
// This prevents orphaned grandchild processes (e.g. qemu spawned by guestfish).
func CommandContextWithGroup(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// We can't use cmd.Cancel because it only signals the process, not the group.
	// Instead, we rely on a goroutine to wait for context done and kill the group.
	// Note: exec.CommandContext already sets up a killer, but it's for the process only.
	// We override the Cancel function to handle the group.
	cmd.Cancel = func() error {
		// Send SIGKILL to the process group (negative PID).
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	return cmd
}

// processNameMatches checks whether actual process name matches expected.
// On Linux/macOS, kernel-reported comm names can be truncated (e.g., 15 chars
// on Linux), so we accept exact match or a truncated-prefix match.
func processNameMatches(actual, expected string) bool {
	actual = strings.TrimSpace(actual)
	expected = strings.TrimSpace(expected)
	if actual == "" || expected == "" {
		return false
	}
	if actual == expected {
		return true
	}
	// Fallback for kernel-truncated comm names. Guard with minimum length to
	// avoid broad prefix matching on short process names.
	return len(actual) >= 15 && strings.HasPrefix(expected, actual)
}

package main

import (
	"context"
	"fmt"
	"os"
	"testing"
)

func setupDoctorOverlayFSTestHooks(t *testing.T) {
	t.Helper()

	origRunCommand := doctorRunCommand
	origReadFile := doctorReadFile
	origGOOS := doctorGOOS

	// Force Linux path so tests exercise the full overlay check logic.
	doctorGOOS = "linux"

	t.Cleanup(func() {
		doctorRunCommand = origRunCommand
		doctorReadFile = origReadFile
		doctorGOOS = origGOOS
	})
}

func TestCheckOverlayFSSupport_SkippedOnNonLinux(t *testing.T) {
	origGOOS := doctorGOOS
	doctorGOOS = "darwin"
	t.Cleanup(func() { doctorGOOS = origGOOS })

	got := checkOverlayFSSupport()
	if got.Status != checkStatusWarn {
		t.Fatalf("status = %q, want warn (detail=%q)", got.Status, got.Detail)
	}
}

func TestCheckOverlayFSSupport_FoundInProcFilesystems(t *testing.T) {
	setupDoctorOverlayFSTestHooks(t)

	doctorReadFile = func(name string) ([]byte, error) {
		if name == "/proc/filesystems" {
			return []byte("nodev\tsysfs\nnodev\ttmpfs\n\toverlay\n"), nil
		}
		return os.ReadFile(name) //nolint:gosec // test helper
	}

	got := checkOverlayFSSupport()
	if got.Status != checkStatusPass {
		t.Fatalf("status = %q, want pass (detail=%q)", got.Status, got.Detail)
	}
	if got.Detail != "/proc/filesystems lists overlay" {
		t.Fatalf("detail = %q, want '/proc/filesystems lists overlay'", got.Detail)
	}
}

func TestCheckOverlayFSSupport_NotInProcButModuleLoaded(t *testing.T) {
	setupDoctorOverlayFSTestHooks(t)

	doctorReadFile = func(name string) ([]byte, error) {
		if name == "/proc/filesystems" {
			return []byte("nodev\tsysfs\nnodev\ttmpfs\n"), nil
		}
		return os.ReadFile(name) //nolint:gosec // test helper
	}

	doctorRunCommand = func(_ context.Context, binary string, _ ...string) ([]byte, error) {
		if binary == "lsmod" {
			return []byte("Module                  Size  Used by\noverlay               155648  0\n"), nil
		}
		return nil, fmt.Errorf("unexpected command: %s", binary)
	}

	got := checkOverlayFSSupport()
	if got.Status != checkStatusPass {
		t.Fatalf("status = %q, want pass (detail=%q)", got.Status, got.Detail)
	}
	if got.Detail != "overlay kernel module loaded (lsmod)" {
		t.Fatalf("detail = %q, want 'overlay kernel module loaded (lsmod)'", got.Detail)
	}
}

func TestCheckOverlayFSSupport_NotAvailable(t *testing.T) {
	setupDoctorOverlayFSTestHooks(t)

	doctorReadFile = func(name string) ([]byte, error) {
		if name == "/proc/filesystems" {
			return []byte("nodev\tsysfs\nnodev\ttmpfs\n"), nil
		}
		return os.ReadFile(name) //nolint:gosec // test helper
	}

	doctorRunCommand = func(_ context.Context, binary string, _ ...string) ([]byte, error) {
		if binary == "lsmod" {
			return []byte("Module                  Size  Used by\n"), nil
		}
		return nil, fmt.Errorf("unexpected command: %s", binary)
	}

	got := checkOverlayFSSupport()
	if got.Status != checkStatusFail {
		t.Fatalf("status = %q, want fail (detail=%q)", got.Status, got.Detail)
	}
}

func TestCheckOverlayFSSupport_ProcFilesystemsReadError(t *testing.T) {
	setupDoctorOverlayFSTestHooks(t)

	doctorReadFile = func(name string) ([]byte, error) {
		if name == "/proc/filesystems" {
			return nil, fmt.Errorf("permission denied")
		}
		return os.ReadFile(name) //nolint:gosec // test helper
	}

	got := checkOverlayFSSupport()
	if got.Status != checkStatusFail {
		t.Fatalf("status = %q, want fail (detail=%q)", got.Status, got.Detail)
	}
}

func TestTryFixOverlayFS_ModprobeSucceeds(t *testing.T) {
	setupDoctorOverlayFSTestHooks(t)

	modprobeRan := false
	doctorRunCommand = func(_ context.Context, binary string, args ...string) ([]byte, error) {
		switch binary {
		case "modprobe":
			modprobeRan = true
			return nil, nil
		case "lsmod":
			// After modprobe, lsmod should show overlay.
			if modprobeRan {
				return []byte("Module                  Size  Used by\noverlay               155648  0\n"), nil
			}
			return []byte("Module                  Size  Used by\n"), nil
		default:
			return nil, fmt.Errorf("unexpected command: %s", binary)
		}
	}

	// After modprobe, /proc/filesystems may still not show overlay,
	// but lsmod will — so the re-check should pass.
	doctorReadFile = func(name string) ([]byte, error) {
		if name == "/proc/filesystems" {
			return []byte("nodev\tsysfs\nnodev\ttmpfs\n"), nil
		}
		return os.ReadFile(name) //nolint:gosec // test helper
	}

	checks := []checkResult{
		{Name: "overlayfs", Status: checkStatusFail, Detail: "overlay not available"},
	}
	updated := tryFixOverlayFS(t.Context(), checks)
	if len(updated) != 1 {
		t.Fatalf("len(updated) = %d, want 1", len(updated))
	}
	if updated[0].Status != checkStatusPass {
		t.Fatalf("status = %q, want pass (detail=%q)", updated[0].Status, updated[0].Detail)
	}
	if !modprobeRan {
		t.Fatal("modprobe was not called")
	}
}

func TestTryFixOverlayFS_ModprobeFails(t *testing.T) {
	setupDoctorOverlayFSTestHooks(t)

	doctorRunCommand = func(_ context.Context, binary string, _ ...string) ([]byte, error) {
		if binary == "modprobe" {
			return []byte("modprobe: FATAL: Module overlay not found"), fmt.Errorf("exit status 1")
		}
		return nil, fmt.Errorf("unexpected command: %s", binary)
	}

	checks := []checkResult{
		{Name: "overlayfs", Status: checkStatusFail, Detail: "overlay not available"},
	}
	updated := tryFixOverlayFS(t.Context(), checks)
	if len(updated) != 1 {
		t.Fatalf("len(updated) = %d, want 1", len(updated))
	}
	if updated[0].Status != checkStatusFail {
		t.Fatalf("status = %q, want fail (detail=%q)", updated[0].Status, updated[0].Detail)
	}
}

func TestTryFixOverlayFS_NoopWhenAlreadyPassing(t *testing.T) {
	setupDoctorOverlayFSTestHooks(t)

	checks := []checkResult{
		{Name: "overlayfs", Status: checkStatusPass, Detail: "/proc/filesystems lists overlay"},
	}
	updated := tryFixOverlayFS(t.Context(), checks)
	if len(updated) != 1 {
		t.Fatalf("len(updated) = %d, want 1", len(updated))
	}
	if updated[0].Status != checkStatusPass {
		t.Fatalf("status = %q, want pass", updated[0].Status)
	}
}

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupDoctorVirtiofsdTestHooks(t *testing.T) {
	t.Helper()

	origLookPath := doctorLookPath
	origRunCommand := doctorRunCommand
	origStat := doctorStat
	origLstat := doctorLstat
	origReadlink := doctorReadlink
	origRemove := doctorRemove
	origSymlink := doctorSymlink
	origMkdirAll := doctorMkdirAll
	origLinkDir := doctorVirtiofsdLinkDir
	origFallbackPaths := append([]string(nil), doctorVirtiofsdFallbackPaths...)

	t.Cleanup(func() {
		doctorLookPath = origLookPath
		doctorRunCommand = origRunCommand
		doctorStat = origStat
		doctorLstat = origLstat
		doctorReadlink = origReadlink
		doctorRemove = origRemove
		doctorSymlink = origSymlink
		doctorMkdirAll = origMkdirAll
		doctorVirtiofsdLinkDir = origLinkDir
		doctorVirtiofsdFallbackPaths = origFallbackPaths
	})
}

func writeExecutableForTest(t *testing.T, dir, name string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write executable: %v", err)
	}
	return path
}

func TestResolveVirtiofsdBinary_UsesConfiguredPath(t *testing.T) {
	setupDoctorVirtiofsdTestHooks(t)

	doctorLookPath = func(file string) (string, error) {
		if file == "virtiofsd" {
			return "/usr/bin/virtiofsd", nil
		}
		return "", fmt.Errorf("not found: %s", file)
	}

	path, fallbackUsed, err := resolveVirtiofsdBinary("virtiofsd")
	if err != nil {
		t.Fatalf("resolveVirtiofsdBinary: %v", err)
	}
	if fallbackUsed {
		t.Fatal("fallbackUsed = true, want false")
	}
	if path != "/usr/bin/virtiofsd" {
		t.Fatalf("path = %q, want %q", path, "/usr/bin/virtiofsd")
	}
}

func TestResolveVirtiofsdBinary_UsesFallbackWhenConfiguredMissing(t *testing.T) {
	setupDoctorVirtiofsdTestHooks(t)

	tmpDir := t.TempDir()
	fallback := writeExecutableForTest(t, tmpDir, "virtiofsd-fallback")
	doctorVirtiofsdFallbackPaths = []string{fallback}

	doctorLookPath = func(string) (string, error) {
		return "", fmt.Errorf("not found")
	}

	path, fallbackUsed, err := resolveVirtiofsdBinary("virtiofsd")
	if err != nil {
		t.Fatalf("resolveVirtiofsdBinary: %v", err)
	}
	if !fallbackUsed {
		t.Fatal("fallbackUsed = false, want true")
	}
	if path != fallback {
		t.Fatalf("path = %q, want %q", path, fallback)
	}
}

func TestCheckVirtiofsdBinary_FallbackOnlyRequiresFix(t *testing.T) {
	setupDoctorVirtiofsdTestHooks(t)

	tmpDir := t.TempDir()
	fallback := writeExecutableForTest(t, tmpDir, "virtiofsd-fallback")
	doctorVirtiofsdFallbackPaths = []string{fallback}

	doctorLookPath = func(string) (string, error) {
		return "", fmt.Errorf("not found")
	}

	got := checkVirtiofsdBinary("virtiofsd")
	if got.Status != "fail" {
		t.Fatalf("status = %q, want fail", got.Status)
	}
	if !strings.Contains(got.Detail, "cocoon doctor --fix") {
		t.Fatalf("detail = %q, want mention of cocoon doctor --fix", got.Detail)
	}
}

func TestTryFixVirtiofsdDependency_LinksFallback(t *testing.T) {
	setupDoctorVirtiofsdTestHooks(t)

	tmpDir := t.TempDir()
	fallback := writeExecutableForTest(t, tmpDir, "virtiofsd-fallback")
	doctorVirtiofsdLinkDir = tmpDir
	doctorVirtiofsdFallbackPaths = []string{fallback}

	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	doctorRunCommand = func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return []byte("virtiofsd 1.9.0"), nil
	}

	doctorLookPath = func(file string) (string, error) {
		switch file {
		case "virtiofsd":
			linkPath := filepath.Join(doctorVirtiofsdLinkDir, "virtiofsd")
			if _, err := os.Stat(linkPath); err == nil {
				return linkPath, nil
			}
			return "", fmt.Errorf("not found")
		case "apt-get", "dnf":
			return "", fmt.Errorf("not found")
		default:
			return "", fmt.Errorf("not found")
		}
	}

	checks := []checkResult{
		{Name: "virtiofsd", Status: "fail", Detail: "missing"},
	}
	updated := tryFixVirtiofsdDependency(t.Context(), "virtiofsd", checks)
	if len(updated) != 1 {
		t.Fatalf("len(updated) = %d, want 1", len(updated))
	}
	if updated[0].Status != "pass" {
		t.Fatalf("status = %q, want pass (detail=%q)", updated[0].Status, updated[0].Detail)
	}
	if !strings.Contains(updated[0].Detail, "auto-fix applied") {
		t.Fatalf("detail = %q, want auto-fix applied", updated[0].Detail)
	}

	linkPath := filepath.Join(doctorVirtiofsdLinkDir, "virtiofsd")
	target, err := os.Readlink(linkPath)
	if err != nil {
		t.Fatalf("readlink %s: %v", linkPath, err)
	}
	if target != fallback {
		t.Fatalf("symlink target = %q, want %q", target, fallback)
	}
}

func TestEnsureVirtiofsdCommandLink_RefusesNonSymlinkCollision(t *testing.T) {
	setupDoctorVirtiofsdTestHooks(t)

	tmpDir := t.TempDir()
	doctorVirtiofsdLinkDir = tmpDir

	linkPath := filepath.Join(tmpDir, "virtiofsd")
	if err := os.WriteFile(linkPath, []byte("existing-user-managed-binary"), 0o755); err != nil {
		t.Fatalf("write pre-existing file: %v", err)
	}

	err := ensureVirtiofsdCommandLink("virtiofsd", "/usr/libexec/virtiofsd")
	if err == nil {
		t.Fatal("ensureVirtiofsdCommandLink error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "refusing to replace existing non-symlink") {
		t.Fatalf("error = %q, want non-symlink refusal", err)
	}
}

package utils

import (
	"archive/tar"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackDirectoryToTarAndExtractTarToDir(t *testing.T) {
	t.Parallel()

	srcDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(srcDir, "etc"), 0o755); err != nil {
		t.Fatalf("mkdir source tree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "etc", "hosts"), []byte("127.0.0.1 localhost\n"), 0o644); err != nil { //nolint:gosec // test data
		t.Fatalf("write source file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, ".env"), []byte("A=B\n"), 0o600); err != nil { //nolint:gosec // test data
		t.Fatalf("write dotfile: %v", err)
	}
	if err := os.Symlink("etc/hosts", filepath.Join(srcDir, "hosts-link")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	tarPath := filepath.Join(t.TempDir(), "rootfs.tar")
	if err := PackDirectoryToTar(context.Background(), srcDir, tarPath); err != nil {
		t.Fatalf("PackDirectoryToTar: %v", err)
	}

	dstDir := t.TempDir()
	if err := ExtractTarToDir(context.Background(), tarPath, dstDir); err != nil {
		t.Fatalf("ExtractTarToDir: %v", err)
	}

	hostsData, err := os.ReadFile(filepath.Join(dstDir, "etc", "hosts")) //nolint:gosec // test temp path
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	if got, want := string(hostsData), "127.0.0.1 localhost\n"; got != want {
		t.Fatalf("unexpected hosts content: got %q, want %q", got, want)
	}

	envData, err := os.ReadFile(filepath.Join(dstDir, ".env")) //nolint:gosec // test temp path
	if err != nil {
		t.Fatalf("read extracted dotfile: %v", err)
	}
	if got, want := string(envData), "A=B\n"; got != want {
		t.Fatalf("unexpected dotfile content: got %q, want %q", got, want)
	}

	linkTarget, err := os.Readlink(filepath.Join(dstDir, "hosts-link"))
	if err != nil {
		t.Fatalf("read extracted symlink: %v", err)
	}
	if got, want := linkTarget, "etc/hosts"; got != want {
		t.Fatalf("unexpected symlink target: got %q, want %q", got, want)
	}
}

func TestExtractTarToDirRejectsPathTraversal(t *testing.T) {
	t.Parallel()

	tarPath := filepath.Join(t.TempDir(), "bad.tar")
	f, err := os.Create(tarPath) //nolint:gosec // test temp file path
	if err != nil {
		t.Fatalf("create tar: %v", err)
	}
	tw := tar.NewWriter(f)

	payload := []byte("x")
	if err := tw.WriteHeader(&tar.Header{Name: "../escape", Mode: 0o644, Size: int64(len(payload)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatalf("write bad header: %v", err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatalf("write bad payload: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close tar file: %v", err)
	}

	err = ExtractTarToDir(context.Background(), tarPath, t.TempDir())
	if err == nil {
		t.Fatalf("expected traversal rejection, got nil")
	}
	if !strings.Contains(err.Error(), "invalid tar entry") {
		t.Fatalf("expected traversal error, got %v", err)
	}
}

func TestTarHelpersRespectCanceledContext(t *testing.T) {
	t.Parallel()

	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "file"), []byte("data"), 0o644); err != nil { //nolint:gosec // test data
		t.Fatalf("write source file: %v", err)
	}
	tarPath := filepath.Join(t.TempDir(), "archive.tar")

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := PackDirectoryToTar(canceledCtx, srcDir, tarPath); !errors.Is(err, context.Canceled) {
		t.Fatalf("PackDirectoryToTar with canceled context: got %v, want %v", err, context.Canceled)
	}

	// Create a minimal valid tar so ExtractTarToDir can run and observe context first.
	validTar, err := os.Create(tarPath) //nolint:gosec // test temp file path
	if err != nil {
		t.Fatalf("create tar: %v", err)
	}
	tw := tar.NewWriter(validTar)
	if err := tw.WriteHeader(&tar.Header{Name: "file", Mode: 0o644, Size: 4, Typeflag: tar.TypeReg}); err != nil {
		t.Fatalf("write header: %v", err)
	}
	if _, err := tw.Write([]byte("data")); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := validTar.Close(); err != nil {
		t.Fatalf("close tar file: %v", err)
	}

	if err := ExtractTarToDir(canceledCtx, tarPath, t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("ExtractTarToDir with canceled context: got %v, want %v", err, context.Canceled)
	}
}

func TestExtractTarToDir_PreservesSpecialModeBits(t *testing.T) {
	t.Parallel()

	tarPath := filepath.Join(t.TempDir(), "special-mode.tar")
	f, err := os.Create(tarPath) //nolint:gosec // test temp file
	if err != nil {
		t.Fatalf("create tar: %v", err)
	}
	tw := tar.NewWriter(f)
	payload := []byte("#!/bin/sh\necho ok\n")
	if err := tw.WriteHeader(&tar.Header{
		Name:     "bin/tool",
		Mode:     0o4755,
		Size:     int64(len(payload)),
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatalf("write header: %v", err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close file: %v", err)
	}

	dstDir := t.TempDir()
	if err := ExtractTarToDir(context.Background(), tarPath, dstDir); err != nil {
		t.Fatalf("extract: %v", err)
	}
	info, err := os.Stat(filepath.Join(dstDir, "bin", "tool"))
	if err != nil {
		t.Fatalf("stat extracted file: %v", err)
	}
	if info.Mode()&os.ModeSetuid == 0 {
		t.Skip("filesystem does not preserve setuid bit")
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("perm=%#o, want %#o", got, 0o755)
	}
}

func TestExtractTarToDir_SkipsSpecialTarEntries(t *testing.T) {
	t.Parallel()

	tarPath := filepath.Join(t.TempDir(), "special-entry.tar")
	f, err := os.Create(tarPath) //nolint:gosec // test temp file
	if err != nil {
		t.Fatalf("create tar: %v", err)
	}
	tw := tar.NewWriter(f)

	// Char device entry: should be skipped, not fail extraction.
	if err := tw.WriteHeader(&tar.Header{
		Name:     "dev/null",
		Mode:     0o666,
		Typeflag: tar.TypeChar,
		Devmajor: 1,
		Devminor: 3,
	}); err != nil {
		t.Fatalf("write char header: %v", err)
	}

	payload := []byte("ok\n")
	if err := tw.WriteHeader(&tar.Header{
		Name:     "etc/ok.txt",
		Mode:     0o644,
		Size:     int64(len(payload)),
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatalf("write regular header: %v", err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatalf("write regular payload: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close tar file: %v", err)
	}

	dstDir := t.TempDir()
	if err := ExtractTarToDir(context.Background(), tarPath, dstDir); err != nil {
		t.Fatalf("extract tar with special entries: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dstDir, "etc", "ok.txt")) //nolint:gosec // test path
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	if got, want := string(data), "ok\n"; got != want {
		t.Fatalf("content mismatch: got %q, want %q", got, want)
	}

	if _, err := os.Stat(filepath.Join(dstDir, "dev", "null")); !os.IsNotExist(err) {
		t.Fatalf("special node should be skipped, stat err=%v", err)
	}
}

func TestTarEntryModeOrDefault_PreservesModeBits(t *testing.T) {
	t.Parallel()

	hdr := &tar.Header{Name: "bin/tool", Mode: 0o6755, Typeflag: tar.TypeReg}
	mode := tarEntryModeOrDefault(hdr, 0o644)
	if mode.Perm() != 0o755 {
		t.Fatalf("perm=%#o, want %#o", mode.Perm(), 0o755)
	}
	if mode&os.ModeSetuid == 0 {
		t.Fatalf("setuid bit missing in mode=%v", mode)
	}
	if mode&os.ModeSetgid == 0 {
		t.Fatalf("setgid bit missing in mode=%v", mode)
	}
}

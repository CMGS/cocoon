package utils

import (
	"archive/tar"
	"context"
	"errors"
	"io"
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
	if !strings.Contains(err.Error(), "path escapes target directory") {
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
	// Secure default: setuid/gid bits are stripped during extraction to prevent privilege escalation.
	if info.Mode()&os.ModeSetuid != 0 {
		t.Errorf("setuid bit should be stripped")
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

func TestExtractOCILayerTarToDir_ResolvesForwardHardlink(t *testing.T) {
	t.Parallel()

	tarPath := filepath.Join(t.TempDir(), "forward-hardlink.tar")
	f, err := os.Create(tarPath) //nolint:gosec // test temp file
	if err != nil {
		t.Fatalf("create tar: %v", err)
	}
	tw := tar.NewWriter(f)

	// Emit hardlink before its target to reproduce forward-link ordering.
	if err := tw.WriteHeader(&tar.Header{
		Name:     "usr/bin/perl",
		Mode:     0o755,
		Typeflag: tar.TypeLink,
		Linkname: "usr/bin/perl5.34.0",
	}); err != nil {
		t.Fatalf("write hardlink header: %v", err)
	}

	payload := []byte("perl-binary")
	if err := tw.WriteHeader(&tar.Header{
		Name:     "usr/bin/perl5.34.0",
		Mode:     0o755,
		Size:     int64(len(payload)),
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatalf("write target header: %v", err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatalf("write target payload: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close tar file: %v", err)
	}

	dstDir := t.TempDir()
	if err := ExtractOCILayerTarToDir(t.Context(), tarPath, dstDir); err != nil {
		t.Fatalf("extract layer: %v", err)
	}

	targetPath := filepath.Join(dstDir, "usr", "bin", "perl5.34.0")
	linkPath := filepath.Join(dstDir, "usr", "bin", "perl")

	targetInfo, err := os.Stat(targetPath)
	if err != nil {
		t.Fatalf("stat hardlink target: %v", err)
	}
	linkInfo, err := os.Stat(linkPath)
	if err != nil {
		t.Fatalf("stat hardlink path: %v", err)
	}
	if !os.SameFile(targetInfo, linkInfo) {
		t.Fatalf("%s and %s should reference the same inode", linkPath, targetPath)
	}
}

func TestTarEntryModeOrDefault_PreservesModeBits(t *testing.T) {
	t.Parallel()

	hdr := &tar.Header{Name: "bin/tool", Mode: 0o6755, Typeflag: tar.TypeReg}
	mode := tarEntryModeOrDefault(hdr, 0o644)
	if mode.Perm() != 0o755 {
		t.Fatalf("perm=%#o, want %#o", mode.Perm(), 0o755)
	}
	if mode&os.ModeSetuid != 0 {
		t.Fatalf("setuid bit should be stripped, got mode=%v", mode)
	}
	if mode&os.ModeSetgid != 0 {
		t.Fatalf("setgid bit should be stripped, got mode=%v", mode)
	}
}

func TestExtractOCILayerTarToDir_AppliesWhiteoutFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	layer1 := filepath.Join(root, "layer1.tar")
	layer2 := filepath.Join(root, "layer2.tar")
	outDir := filepath.Join(root, "out")

	createLayerTar(t, layer1, []tarEntrySpec{
		{name: "foo.txt", body: []byte("foo"), mode: 0o644, typ: tar.TypeReg},
		{name: "keep.txt", body: []byte("keep"), mode: 0o644, typ: tar.TypeReg},
	})
	createLayerTar(t, layer2, []tarEntrySpec{
		{name: ".wh.foo.txt", body: nil, mode: 0o000, typ: tar.TypeReg},
		{name: "new.txt", body: []byte("new"), mode: 0o644, typ: tar.TypeReg},
	})

	if err := ExtractOCILayerTarToDir(context.Background(), layer1, outDir); err != nil {
		t.Fatalf("extract layer1: %v", err)
	}
	if err := ExtractOCILayerTarToDir(context.Background(), layer2, outDir); err != nil {
		t.Fatalf("extract layer2: %v", err)
	}

	if _, err := os.Stat(filepath.Join(outDir, "foo.txt")); !os.IsNotExist(err) {
		t.Fatalf("foo.txt should be removed by whiteout, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, ".wh.foo.txt")); !os.IsNotExist(err) {
		t.Fatalf("whiteout marker should not be materialized, stat err=%v", err)
	}

	keep, err := os.ReadFile(filepath.Join(outDir, "keep.txt")) //nolint:gosec // test path
	if err != nil {
		t.Fatalf("read keep.txt: %v", err)
	}
	if string(keep) != "keep" {
		t.Fatalf("keep.txt content=%q, want keep", string(keep))
	}

	newData, err := os.ReadFile(filepath.Join(outDir, "new.txt")) //nolint:gosec // test path
	if err != nil {
		t.Fatalf("read new.txt: %v", err)
	}
	if string(newData) != "new" {
		t.Fatalf("new.txt content=%q, want new", string(newData))
	}
}

func TestExtractOCILayerTarToDir_AppliesOpaqueWhiteout(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	layer1 := filepath.Join(root, "layer1.tar")
	layer2 := filepath.Join(root, "layer2.tar")
	outDir := filepath.Join(root, "out")

	createLayerTar(t, layer1, []tarEntrySpec{
		{name: "etc/a.conf", body: []byte("a"), mode: 0o644, typ: tar.TypeReg},
		{name: "etc/b.conf", body: []byte("b"), mode: 0o644, typ: tar.TypeReg},
	})
	createLayerTar(t, layer2, []tarEntrySpec{
		{name: "etc/.wh..wh..opq", body: nil, mode: 0o000, typ: tar.TypeReg},
		{name: "etc/c.conf", body: []byte("c"), mode: 0o644, typ: tar.TypeReg},
	})

	if err := ExtractOCILayerTarToDir(context.Background(), layer1, outDir); err != nil {
		t.Fatalf("extract layer1: %v", err)
	}
	if err := ExtractOCILayerTarToDir(context.Background(), layer2, outDir); err != nil {
		t.Fatalf("extract layer2: %v", err)
	}

	if _, err := os.Stat(filepath.Join(outDir, "etc", "a.conf")); !os.IsNotExist(err) {
		t.Fatalf("etc/a.conf should be removed by opaque whiteout, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "etc", "b.conf")); !os.IsNotExist(err) {
		t.Fatalf("etc/b.conf should be removed by opaque whiteout, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "etc", ".wh..wh..opq")); !os.IsNotExist(err) {
		t.Fatalf("opaque whiteout marker should not be materialized, stat err=%v", err)
	}

	cData, err := os.ReadFile(filepath.Join(outDir, "etc", "c.conf")) //nolint:gosec // test path
	if err != nil {
		t.Fatalf("read etc/c.conf: %v", err)
	}
	if string(cData) != "c" {
		t.Fatalf("etc/c.conf content=%q, want c", string(cData))
	}
}

type tarEntrySpec struct {
	name string
	body []byte
	mode int64
	typ  byte
}

func createLayerTar(t *testing.T, tarPath string, entries []tarEntrySpec) {
	t.Helper()

	f, err := os.Create(tarPath) //nolint:gosec // test temp file
	if err != nil {
		t.Fatalf("create tar %s: %v", tarPath, err)
	}
	tw := tar.NewWriter(f)

	for _, entry := range entries {
		size := int64(len(entry.body))
		hdr := &tar.Header{
			Name:     entry.name,
			Mode:     entry.mode,
			Size:     size,
			Typeflag: entry.typ,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			_ = tw.Close()
			_ = f.Close()
			t.Fatalf("write header %s: %v", entry.name, err)
		}
		if size > 0 {
			if _, err := tw.Write(entry.body); err != nil {
				_ = tw.Close()
				_ = f.Close()
				t.Fatalf("write body %s: %v", entry.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		_ = f.Close()
		t.Fatalf("close tar writer: %v", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		t.Fatalf("seek tar file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close tar file: %v", err)
	}
}

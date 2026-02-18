//go:build linux

package oci

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestGenerateDeltaLayerTar_ChangesAndWhiteouts(t *testing.T) {
	t.Parallel()

	baseDir := filepath.Join(t.TempDir(), "base")
	modifiedDir := filepath.Join(t.TempDir(), "modified")
	if err := os.MkdirAll(baseDir, 0o755); err != nil { //nolint:gosec // test setup
		t.Fatalf("mkdir base: %v", err)
	}
	if err := os.MkdirAll(modifiedDir, 0o755); err != nil { //nolint:gosec // test setup
		t.Fatalf("mkdir modified: %v", err)
	}

	mustWriteFile := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // test setup
			t.Fatalf("mkdir parent for %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil { //nolint:gosec // test setup
			t.Fatalf("write %s: %v", path, err)
		}
	}

	// Base tree.
	mustWriteFile(filepath.Join(baseDir, "etc/app.conf"), "value=old\n")
	mustWriteFile(filepath.Join(baseDir, "remove.txt"), "remove-me\n")
	mustWriteFile(filepath.Join(baseDir, "dirremove/nested.txt"), "gone\n")
	mustWriteFile(filepath.Join(baseDir, "dir/keep.txt"), "same\n")

	// Modified tree.
	mustWriteFile(filepath.Join(modifiedDir, "etc/app.conf"), "value=new\n")
	mustWriteFile(filepath.Join(modifiedDir, "add.txt"), "new-file\n")
	mustWriteFile(filepath.Join(modifiedDir, "dir/keep.txt"), "same\n")

	outTar := filepath.Join(t.TempDir(), "delta.tar")
	digest, size, changeCount, err := generateDeltaLayerTar(baseDir, modifiedDir, outTar)
	if err != nil {
		t.Fatalf("generateDeltaLayerTar: %v", err)
	}
	if digest == "" {
		t.Fatal("delta digest is empty")
	}
	if size <= 0 {
		t.Fatalf("delta size=%d, want >0", size)
	}
	if changeCount <= 0 {
		t.Fatalf("changeCount=%d, want >0", changeCount)
	}

	names := readTarNames(t, outTar)
	for _, expected := range []string{
		"add.txt",
		"etc/app.conf",
		".wh.remove.txt",
		".wh.dirremove",
	} {
		if !slices.Contains(names, expected) {
			t.Fatalf("delta tar entries=%v, missing %q", names, expected)
		}
	}
	if slices.Contains(names, "dir/keep.txt") {
		t.Fatalf("delta tar should not include unchanged file, got entries=%v", names)
	}
}

func TestGenerateDeltaLayerTar_NoChanges(t *testing.T) {
	t.Parallel()

	baseDir := filepath.Join(t.TempDir(), "base")
	modifiedDir := filepath.Join(t.TempDir(), "modified")
	if err := os.MkdirAll(baseDir, 0o755); err != nil { //nolint:gosec // test setup
		t.Fatalf("mkdir base: %v", err)
	}
	if err := os.MkdirAll(modifiedDir, 0o755); err != nil { //nolint:gosec // test setup
		t.Fatalf("mkdir modified: %v", err)
	}
	path := filepath.Join(baseDir, "same.txt")
	if err := os.WriteFile(path, []byte("same\n"), 0o644); err != nil { //nolint:gosec // test setup
		t.Fatalf("write base file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(modifiedDir, "same.txt"), []byte("same\n"), 0o644); err != nil { //nolint:gosec // test setup
		t.Fatalf("write modified file: %v", err)
	}

	outTar := filepath.Join(t.TempDir(), "delta-empty.tar")
	digest, size, changeCount, err := generateDeltaLayerTar(baseDir, modifiedDir, outTar)
	if err != nil {
		t.Fatalf("generateDeltaLayerTar: %v", err)
	}
	if digest != "" {
		t.Fatalf("digest=%q, want empty for no-op delta", digest)
	}
	if size != 0 {
		t.Fatalf("size=%d, want 0 for no-op delta", size)
	}
	if changeCount != 0 {
		t.Fatalf("changeCount=%d, want 0", changeCount)
	}
	if _, statErr := os.Stat(outTar); !os.IsNotExist(statErr) {
		t.Fatalf("delta tar %s should not be created for no-op delta", outTar)
	}
}

func TestGenerateDeltaLayerTar_PermissionOnlyChangeSetuid(t *testing.T) {
	t.Parallel()

	baseDir := filepath.Join(t.TempDir(), "base")
	modifiedDir := filepath.Join(t.TempDir(), "modified")
	if err := os.MkdirAll(baseDir, 0o755); err != nil { //nolint:gosec // test setup
		t.Fatalf("mkdir base: %v", err)
	}
	if err := os.MkdirAll(modifiedDir, 0o755); err != nil { //nolint:gosec // test setup
		t.Fatalf("mkdir modified: %v", err)
	}

	basePath := filepath.Join(baseDir, "usr/bin/tool")
	modifiedPath := filepath.Join(modifiedDir, "usr/bin/tool")
	if err := os.MkdirAll(filepath.Dir(basePath), 0o755); err != nil { //nolint:gosec // test setup
		t.Fatalf("mkdir base parent: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(modifiedPath), 0o755); err != nil { //nolint:gosec // test setup
		t.Fatalf("mkdir modified parent: %v", err)
	}
	if err := os.WriteFile(basePath, []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil { //nolint:gosec // test setup
		t.Fatalf("write base file: %v", err)
	}
	if err := os.WriteFile(modifiedPath, []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil { //nolint:gosec // test setup
		t.Fatalf("write modified file: %v", err)
	}
	if err := os.Chmod(modifiedPath, 0o4755); err != nil {
		t.Fatalf("chmod modified setuid: %v", err)
	}
	modifiedInfo, err := os.Stat(modifiedPath)
	if err != nil {
		t.Fatalf("stat modified file: %v", err)
	}
	if modifiedInfo.Mode()&os.ModeSetuid == 0 {
		t.Skip("filesystem does not preserve setuid bit for test file")
	}

	outTar := filepath.Join(t.TempDir(), "delta-setuid.tar")
	_, size, changeCount, err := generateDeltaLayerTar(baseDir, modifiedDir, outTar)
	if err != nil {
		t.Fatalf("generateDeltaLayerTar: %v", err)
	}
	if size <= 0 || changeCount <= 0 {
		t.Fatalf("expected non-empty delta, size=%d changeCount=%d", size, changeCount)
	}

	headers := readTarHeaders(t, outTar)
	hdr, ok := headers["usr/bin/tool"]
	if !ok {
		t.Fatalf("expected usr/bin/tool in delta tar, headers=%v", mapKeys(headers))
	}
	if hdr.Mode != 0o4755 {
		t.Fatalf("header mode=%#o, want %#o", hdr.Mode, 0o4755)
	}
}

func TestGenerateDeltaLayerTar_SymlinkChange(t *testing.T) {
	t.Parallel()

	baseDir := filepath.Join(t.TempDir(), "base")
	modifiedDir := filepath.Join(t.TempDir(), "modified")
	if err := os.MkdirAll(filepath.Join(baseDir, "etc"), 0o755); err != nil { //nolint:gosec // test setup
		t.Fatalf("mkdir base: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(modifiedDir, "etc"), 0o755); err != nil { //nolint:gosec // test setup
		t.Fatalf("mkdir modified: %v", err)
	}

	baseLink := filepath.Join(baseDir, "etc/current")
	modifiedLink := filepath.Join(modifiedDir, "etc/current")
	if err := os.Symlink("/opt/app-v1", baseLink); err != nil {
		t.Fatalf("symlink base: %v", err)
	}
	if err := os.Symlink("/opt/app-v2", modifiedLink); err != nil {
		t.Fatalf("symlink modified: %v", err)
	}

	outTar := filepath.Join(t.TempDir(), "delta-symlink.tar")
	_, size, changeCount, err := generateDeltaLayerTar(baseDir, modifiedDir, outTar)
	if err != nil {
		t.Fatalf("generateDeltaLayerTar: %v", err)
	}
	if size <= 0 || changeCount <= 0 {
		t.Fatalf("expected non-empty delta, size=%d changeCount=%d", size, changeCount)
	}

	headers := readTarHeaders(t, outTar)
	hdr, ok := headers["etc/current"]
	if !ok {
		t.Fatalf("expected etc/current symlink in delta tar, headers=%v", mapKeys(headers))
	}
	if hdr.Typeflag != tar.TypeSymlink {
		t.Fatalf("type=%d, want symlink (%d)", hdr.Typeflag, tar.TypeSymlink)
	}
	if hdr.Linkname != "/opt/app-v2" {
		t.Fatalf("linkname=%q, want %q", hdr.Linkname, "/opt/app-v2")
	}
}

func readTarNames(t *testing.T, tarPath string) []string {
	t.Helper()

	f, err := os.Open(tarPath) //nolint:gosec // test temp file
	if err != nil {
		t.Fatalf("open %s: %v", tarPath, err)
	}
	defer f.Close() //nolint:errcheck

	tr := tar.NewReader(f)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar %s: %v", tarPath, err)
		}
		names = append(names, hdr.Name)
	}
	return names
}

func readTarHeaders(t *testing.T, tarPath string) map[string]*tar.Header {
	t.Helper()

	f, err := os.Open(tarPath) //nolint:gosec // test temp file
	if err != nil {
		t.Fatalf("open %s: %v", tarPath, err)
	}
	defer f.Close() //nolint:errcheck

	tr := tar.NewReader(f)
	headers := make(map[string]*tar.Header)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar %s: %v", tarPath, err)
		}
		copyHdr := *hdr
		headers[hdr.Name] = &copyHdr
	}
	return headers
}

func mapKeys(m map[string]*tar.Header) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

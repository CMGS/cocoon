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
	_, size, changeCount, err := generateDeltaLayerTar(baseDir, modifiedDir, outTar)
	if err != nil {
		t.Fatalf("generateDeltaLayerTar: %v", err)
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

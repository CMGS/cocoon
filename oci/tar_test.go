package oci

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNormalizeHeader(t *testing.T) {
	t.Parallel()

	hdr := &tar.Header{
		Name:       "test.txt",
		Size:       100,
		Mode:       0o644,
		Uid:        1000,
		Gid:        1000,
		ModTime:    time.Now(),
		AccessTime: time.Now(),
		ChangeTime: time.Now(),
		PAXRecords: map[string]string{
			"SCHILY.acl.access":  "some-acl",
			"SCHILY.acl.default": "some-default-acl",
			"SCHILY.xattr.user.test": "keep-this",
			"atime": "1234567890",
			"ctime": "1234567890",
			"mtime": "1234567890",
		},
	}

	normalizeHeader(hdr)

	epoch := time.Unix(0, 0)
	if !hdr.ModTime.Equal(epoch) {
		t.Errorf("ModTime = %v, want epoch", hdr.ModTime)
	}
	if !hdr.AccessTime.Equal(epoch) {
		t.Errorf("AccessTime = %v, want epoch", hdr.AccessTime)
	}
	if !hdr.ChangeTime.Equal(epoch) {
		t.Errorf("ChangeTime = %v, want epoch", hdr.ChangeTime)
	}
	if hdr.Format != tar.FormatPAX {
		t.Errorf("Format = %v, want PAX", hdr.Format)
	}
	if _, ok := hdr.PAXRecords["SCHILY.acl.access"]; ok {
		t.Error("SCHILY.acl.access should be stripped")
	}
	if _, ok := hdr.PAXRecords["SCHILY.acl.default"]; ok {
		t.Error("SCHILY.acl.default should be stripped")
	}
	if v, ok := hdr.PAXRecords["SCHILY.xattr.user.test"]; !ok || v != "keep-this" {
		t.Error("SCHILY.xattr.user.test should be preserved")
	}
	if _, ok := hdr.PAXRecords["atime"]; ok {
		t.Error("atime PAX record should be stripped")
	}
	// UID/GID/mode preserved.
	if hdr.Uid != 1000 {
		t.Errorf("Uid = %d, want 1000", hdr.Uid)
	}
	if hdr.Mode != 0o644 {
		t.Errorf("Mode = %o, want 644", hdr.Mode)
	}
}

func TestRewriteDeterministicTar(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Create a source tar with unsorted entries and non-epoch timestamps.
	srcPath := filepath.Join(tmpDir, "source.tar")
	sf, err := os.Create(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(sf)

	entries := []struct {
		name string
		body string
	}{
		{"c.txt", "content-c"},
		{"a.txt", "content-a"},
		{"b.txt", "content-b"},
		{"excluded.txt", "should-be-excluded"},
	}
	for _, e := range entries {
		hdr := &tar.Header{
			Name:    e.name,
			Size:    int64(len(e.body)),
			Mode:    0o644,
			ModTime: time.Now(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	sf.Close()

	// Rewrite with exclude.
	dstPath := filepath.Join(tmpDir, "dest.tar")
	digest, size, err := rewriteDeterministicTar(srcPath, dstPath, []string{"excluded.txt"})
	if err != nil {
		t.Fatalf("rewriteDeterministicTar: %v", err)
	}
	if digest == "" {
		t.Error("digest is empty")
	}
	if size <= 0 {
		t.Errorf("size = %d, want > 0", size)
	}

	// Verify the output tar entries are sorted and excluded entry is absent.
	df, err := os.Open(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	defer df.Close()

	tr := tar.NewReader(df)
	var names []string
	for {
		hdr, rerr := tr.Next()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			t.Fatal(rerr)
		}
		names = append(names, hdr.Name)
		// Verify timestamps are epoch.
		if !hdr.ModTime.Equal(time.Unix(0, 0)) {
			t.Errorf("entry %q ModTime = %v, want epoch", hdr.Name, hdr.ModTime)
		}
	}

	wantNames := []string{"a.txt", "b.txt", "c.txt"}
	if len(names) != len(wantNames) {
		t.Fatalf("entries = %v, want %v", names, wantNames)
	}
	for i, name := range names {
		if name != wantNames[i] {
			t.Errorf("entry[%d] = %q, want %q", i, name, wantNames[i])
		}
	}

	// Verify digest matches actual file content.
	df.Seek(0, io.SeekStart)
	h := sha256.New()
	io.Copy(h, df)
	wantDigest := hex.EncodeToString(h.Sum(nil))
	if digest != wantDigest {
		t.Errorf("digest = %s, want %s", digest, wantDigest)
	}

	// Run again to verify determinism — same digest.
	dstPath2 := filepath.Join(tmpDir, "dest2.tar")
	digest2, _, err := rewriteDeterministicTar(srcPath, dstPath2, []string{"excluded.txt"})
	if err != nil {
		t.Fatalf("second rewrite: %v", err)
	}
	if digest != digest2 {
		t.Errorf("non-deterministic: digest1=%s, digest2=%s", digest, digest2)
	}
}

func TestBuildKernelLayerTar(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Create fake kernel and initrd files.
	kernelContent := []byte("fake-kernel-binary-content")
	initrdContent := []byte("fake-initrd-binary-content")

	kernelPath := filepath.Join(tmpDir, "vmlinuz-5.15.0-100")
	initrdPath := filepath.Join(tmpDir, "initrd.img-5.15.0-100")
	os.WriteFile(kernelPath, kernelContent, 0o644)
	os.WriteFile(initrdPath, initrdContent, 0o644)

	outPath := filepath.Join(tmpDir, "kernel-layer.tar")
	digest, size, err := buildKernelLayerTar(kernelPath, initrdPath, outPath)
	if err != nil {
		t.Fatalf("buildKernelLayerTar: %v", err)
	}
	if digest == "" {
		t.Error("digest is empty")
	}
	if size <= 0 {
		t.Errorf("size = %d, want > 0", size)
	}

	// Verify tar contents: should have exactly 2 entries in sorted order.
	f, err := os.Open(outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	tr := tar.NewReader(f)
	var names []string
	bodies := make(map[string][]byte)
	for {
		hdr, rerr := tr.Next()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			t.Fatal(rerr)
		}
		names = append(names, hdr.Name)
		body, _ := io.ReadAll(tr)
		bodies[hdr.Name] = body

		// Timestamps should be epoch.
		if !hdr.ModTime.Equal(time.Unix(0, 0)) {
			t.Errorf("entry %q ModTime = %v, want epoch", hdr.Name, hdr.ModTime)
		}
	}

	wantNames := []string{"initrd.img", "vmlinuz"}
	if len(names) != len(wantNames) {
		t.Fatalf("entries = %v, want %v", names, wantNames)
	}
	for i, name := range names {
		if name != wantNames[i] {
			t.Errorf("entry[%d] = %q, want %q", i, name, wantNames[i])
		}
	}

	if !bytes.Equal(bodies["vmlinuz"], kernelContent) {
		t.Error("vmlinuz content mismatch")
	}
	if !bytes.Equal(bodies["initrd.img"], initrdContent) {
		t.Error("initrd.img content mismatch")
	}

	// Verify determinism.
	outPath2 := filepath.Join(tmpDir, "kernel-layer2.tar")
	digest2, _, err := buildKernelLayerTar(kernelPath, initrdPath, outPath2)
	if err != nil {
		t.Fatal(err)
	}
	if digest != digest2 {
		t.Errorf("non-deterministic: digest1=%s, digest2=%s", digest, digest2)
	}
}

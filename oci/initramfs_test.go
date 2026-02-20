package oci

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// buildCpioNewc constructs a minimal cpio newc archive containing the given
// entry names (all with zero-length file data). The archive is terminated with
// a TRAILER!!! entry.
func buildCpioNewc(names []string) []byte {
	var buf bytes.Buffer
	for _, name := range names {
		writeCpioEntry(&buf, name, nil)
	}
	writeCpioEntry(&buf, "TRAILER!!!", nil)
	return buf.Bytes()
}

// writeCpioEntry writes one cpio newc entry (header + name + data) to w.
func writeCpioEntry(buf *bytes.Buffer, name string, data []byte) {
	nameBytes := append([]byte(name), 0) // include trailing NUL
	namesize := len(nameBytes)
	filesize := len(data)

	// Fixed 110-byte header: all fields as 8-char hex, except magic (6 chars).
	hdr := fmt.Sprintf(
		"070701"+
			"%08x"+ // inode
			"%08x"+ // mode
			"%08x"+ // uid
			"%08x"+ // gid
			"%08x"+ // nlink
			"%08x"+ // mtime
			"%08x"+ // filesize
			"%08x"+ // devmajor
			"%08x"+ // devminor
			"%08x"+ // rdevmajor
			"%08x"+ // rdevminor
			"%08x"+ // namesize
			"%08x", // checksum
		0,        // inode
		0o100644, // mode
		0,        // uid
		0,        // gid
		1,        // nlink
		0,        // mtime
		filesize, // filesize
		0,        // devmajor
		0,        // devminor
		0,        // rdevmajor
		0,        // rdevminor
		namesize, // namesize
		0,        // checksum
	)

	buf.WriteString(hdr)
	buf.Write(nameBytes)

	// Pad header + name to 4-byte boundary.
	headerAndName := 110 + namesize
	if pad := alignUp(headerAndName, 4) - headerAndName; pad > 0 {
		buf.Write(make([]byte, pad))
	}

	// Write file data.
	if len(data) > 0 {
		buf.Write(data)
	}

	// Pad file data to 4-byte boundary.
	if pad := alignUp(filesize, 4) - filesize; pad > 0 {
		buf.Write(make([]byte, pad))
	}
}

// gzipData compresses data using gzip.
func gzipData(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// buildKernelTarWithInitramfs creates a kernel layer tar containing an
// initramfs entry with the given name and raw data.
func buildKernelTarWithInitramfs(t *testing.T, dir, initramfsName string, initramfsData []byte) string {
	t.Helper()
	tarPath := filepath.Join(dir, "kernel-layer.tar")
	f, err := os.Create(tarPath)
	if err != nil {
		t.Fatalf("create tar: %v", err)
	}
	defer f.Close() //nolint:errcheck

	tw := tar.NewWriter(f)

	// Write a vmlinuz entry (dummy).
	if err := tw.WriteHeader(&tar.Header{
		Name: "vmlinuz",
		Size: 4,
		Mode: 0o644,
	}); err != nil {
		t.Fatalf("write vmlinuz header: %v", err)
	}
	if _, err := tw.Write([]byte("kern")); err != nil {
		t.Fatalf("write vmlinuz body: %v", err)
	}

	// Write the initramfs entry.
	if initramfsName != "" {
		if err := tw.WriteHeader(&tar.Header{
			Name: initramfsName,
			Size: int64(len(initramfsData)),
			Mode: 0o644,
		}); err != nil {
			t.Fatalf("write initramfs header: %v", err)
		}
		if _, err := tw.Write(initramfsData); err != nil {
			t.Fatalf("write initramfs body: %v", err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	return tarPath
}

func TestCheckInitramfsVirtiofs_GzipWithVirtiofs(t *testing.T) {
	t.Parallel()

	cpioData := buildCpioNewc([]string{
		"kernel/fs/fuse/fuse.ko",
		"kernel/fs/fuse/virtiofs.ko",
		"kernel/drivers/virtio/virtio.ko",
	})
	compressed := gzipData(t, cpioData)

	dir := t.TempDir()
	tarPath := buildKernelTarWithInitramfs(t, dir, "initrd.img", compressed)

	found, err := CheckInitramfsVirtiofs(tarPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Errorf("expected virtiofs found=true, got false")
	}
}

func TestCheckInitramfsVirtiofs_GzipWithoutVirtiofs(t *testing.T) {
	t.Parallel()

	cpioData := buildCpioNewc([]string{
		"kernel/fs/fuse/fuse.ko",
		"kernel/drivers/virtio/virtio.ko",
		"kernel/net/core/net.ko",
	})
	compressed := gzipData(t, cpioData)

	dir := t.TempDir()
	tarPath := buildKernelTarWithInitramfs(t, dir, "initrd.img", compressed)

	found, err := CheckInitramfsVirtiofs(tarPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Errorf("expected virtiofs found=false, got true")
	}
}

func TestCheckInitramfsVirtiofs_UncompressedWithVirtiofsXZ(t *testing.T) {
	t.Parallel()

	cpioData := buildCpioNewc([]string{
		"kernel/fs/fuse/fuse.ko",
		"kernel/fs/fuse/virtiofs.ko.xz",
	})

	dir := t.TempDir()
	tarPath := buildKernelTarWithInitramfs(t, dir, "initramfs.img", cpioData)

	found, err := CheckInitramfsVirtiofs(tarPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Errorf("expected virtiofs found=true for .ko.xz, got false")
	}
}

func TestCheckInitramfsVirtiofs_ZstdReturnsError(t *testing.T) {
	t.Parallel()

	// Zstd magic: 0x28 0xb5 0x2f 0xfd followed by dummy bytes.
	zstdMagic, _ := hex.DecodeString("28b52ffd") //nolint:errcheck
	zstdData := append(zstdMagic, make([]byte, 100)...)

	dir := t.TempDir()
	tarPath := buildKernelTarWithInitramfs(t, dir, "initrd.img", zstdData)

	found, err := CheckInitramfsVirtiofs(tarPath)
	if err == nil {
		t.Fatalf("expected error for zstd initramfs, got nil (found=%v)", found)
	}
	if found {
		t.Errorf("expected found=false on error, got true")
	}
	if got := err.Error(); !contains(got, "zstd") {
		t.Errorf("error should mention zstd, got: %s", got)
	}
}

func TestCheckInitramfsVirtiofs_NoInitrdEntry(t *testing.T) {
	t.Parallel()

	// Build a kernel tar with only vmlinuz, no initramfs entry.
	dir := t.TempDir()
	tarPath := buildKernelTarWithInitramfs(t, dir, "", nil)

	found, err := CheckInitramfsVirtiofs(tarPath)
	if err != nil {
		t.Fatalf("expected no error for missing initrd, got: %v", err)
	}
	if found {
		t.Errorf("expected found=false when no initrd entry, got true")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && bytes.Contains([]byte(s), []byte(substr))
}

package oci

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildNewcCPIO constructs a minimal cpio newc archive from a list of
// (name, content) pairs. This is used to simulate an initramfs for testing.
func buildNewcCPIO(entries []cpioTestEntry) []byte {
	var buf bytes.Buffer
	ino := uint32(1)
	for _, e := range entries {
		writeCPIONewcEntry(&buf, ino, e.Name, e.Data)
		ino++
	}
	// Write trailer.
	writeCPIONewcEntry(&buf, 0, "TRAILER!!!", nil)
	return buf.Bytes()
}

type cpioTestEntry struct {
	Name string
	Data []byte
}

// writeCPIONewcEntry writes a single cpio newc-format entry to w.
func writeCPIONewcEntry(w *bytes.Buffer, ino uint32, name string, data []byte) {
	nameWithNul := name + "\x00"
	namesize := len(nameWithNul)
	filesize := len(data)

	// Header: magic + 13 hex fields of 8 chars each.
	hdr := fmt.Sprintf("070701"+
		"%08X"+ // ino
		"%08X"+ // mode
		"%08X"+ // uid
		"%08X"+ // gid
		"%08X"+ // nlink
		"%08X"+ // mtime
		"%08X"+ // filesize
		"%08X"+ // devmajor
		"%08X"+ // devminor
		"%08X"+ // rdevmajor
		"%08X"+ // rdevminor
		"%08X"+ // namesize
		"%08X", // check
		ino,
		0o100644,  // mode (regular file)
		uint32(0), // uid
		uint32(0), // gid
		uint32(1), // nlink
		uint32(0), // mtime
		uint32(filesize),
		uint32(0), // devmajor
		uint32(0), // devminor
		uint32(0), // rdevmajor
		uint32(0), // rdevminor
		uint32(namesize),
		uint32(0), // check
	)

	w.WriteString(hdr)
	w.WriteString(nameWithNul)

	// Pad header + name to 4-byte boundary.
	headerPlusName := 110 + namesize
	if pad := align4(headerPlusName) - headerPlusName; pad > 0 {
		w.Write(make([]byte, pad))
	}

	// Write data.
	w.Write(data)

	// Pad data to 4-byte boundary.
	if pad := align4(filesize) - filesize; pad > 0 {
		w.Write(make([]byte, pad))
	}
}

func TestCheckInitramfsVirtiofs_Found(t *testing.T) {
	t.Parallel()

	// Build a cpio archive with a virtiofs.ko entry.
	cpioData := buildNewcCPIO([]cpioTestEntry{
		{Name: "lib/modules/6.8.0-40-generic/kernel/fs/fuse/virtiofs.ko", Data: []byte("ELF-fake")},
		{Name: "bin/busybox", Data: []byte("busybox-binary")},
	})

	// Gzip-compress the cpio (most common initramfs format).
	var gzBuf bytes.Buffer
	gzw := gzip.NewWriter(&gzBuf)
	if _, err := gzw.Write(cpioData); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	// Build a kernel layer tar containing the initramfs.
	layerTar := buildTestKernelTar(t, "initrd.img", gzBuf.Bytes())

	// Write tar to a temp file.
	tmpDir := t.TempDir()
	layerPath := filepath.Join(tmpDir, "kernel-layer.tar")
	if err := os.WriteFile(layerPath, layerTar, 0o644); err != nil {
		t.Fatalf("write kernel layer tar: %v", err)
	}

	found, err := CheckInitramfsVirtiofs(layerPath)
	if err != nil {
		t.Fatalf("CheckInitramfsVirtiofs: %v", err)
	}
	if !found {
		t.Fatal("expected virtiofs module to be found")
	}
}

func TestCheckInitramfsVirtiofs_NotFound(t *testing.T) {
	t.Parallel()

	// Build a cpio archive without virtiofs.
	cpioData := buildNewcCPIO([]cpioTestEntry{
		{Name: "lib/modules/6.8.0-40-generic/kernel/fs/ext4/ext4.ko", Data: []byte("ELF-fake")},
		{Name: "bin/busybox", Data: []byte("busybox-binary")},
	})

	var gzBuf bytes.Buffer
	gzw := gzip.NewWriter(&gzBuf)
	if _, err := gzw.Write(cpioData); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	layerTar := buildTestKernelTar(t, "initrd.img", gzBuf.Bytes())
	tmpDir := t.TempDir()
	layerPath := filepath.Join(tmpDir, "kernel-layer.tar")
	if err := os.WriteFile(layerPath, layerTar, 0o644); err != nil {
		t.Fatalf("write kernel layer tar: %v", err)
	}

	found, err := CheckInitramfsVirtiofs(layerPath)
	if err != nil {
		t.Fatalf("CheckInitramfsVirtiofs: %v", err)
	}
	if found {
		t.Fatal("expected virtiofs module NOT to be found")
	}
}

func TestCheckInitramfsVirtiofs_UncompressedCPIO(t *testing.T) {
	t.Parallel()

	// Build an uncompressed cpio archive with virtiofs.ko.xz.
	cpioData := buildNewcCPIO([]cpioTestEntry{
		{Name: "lib/modules/6.8.0/kernel/fs/fuse/virtiofs.ko.xz", Data: []byte("XZ-compressed")},
	})

	layerTar := buildTestKernelTar(t, "initrd.img", cpioData)
	tmpDir := t.TempDir()
	layerPath := filepath.Join(tmpDir, "kernel-layer.tar")
	if err := os.WriteFile(layerPath, layerTar, 0o644); err != nil {
		t.Fatalf("write kernel layer tar: %v", err)
	}

	found, err := CheckInitramfsVirtiofs(layerPath)
	if err != nil {
		t.Fatalf("CheckInitramfsVirtiofs: %v", err)
	}
	if !found {
		t.Fatal("expected virtiofs.ko.xz to be found in uncompressed cpio")
	}
}

func TestCheckInitramfsVirtiofs_UncompressedCPIONewcCRC(t *testing.T) {
	t.Parallel()

	// Build a cpio archive with virtiofs.ko but mutate magic to "070702"
	// (newc-crc variant), which should also be accepted.
	cpioData := buildNewcCPIO([]cpioTestEntry{
		{Name: "lib/modules/6.8.0/kernel/fs/fuse/virtiofs.ko.gz", Data: []byte("GZ-compressed")},
	})
	copy(cpioData[:6], []byte("070702"))

	layerTar := buildTestKernelTar(t, "initrd.img", cpioData)
	tmpDir := t.TempDir()
	layerPath := filepath.Join(tmpDir, "kernel-layer.tar")
	if err := os.WriteFile(layerPath, layerTar, 0o644); err != nil {
		t.Fatalf("write kernel layer tar: %v", err)
	}

	found, err := CheckInitramfsVirtiofs(layerPath)
	if err != nil {
		t.Fatalf("CheckInitramfsVirtiofs: %v", err)
	}
	if !found {
		t.Fatal("expected virtiofs.ko.gz to be found in uncompressed newc-crc cpio")
	}
}

func TestCheckInitramfsVirtiofs_ZstdReturnsError(t *testing.T) {
	t.Parallel()

	// Simulate a zstd-compressed initramfs (magic bytes 0x28B52FFD + dummy data).
	zstdMagic := []byte{0x28, 0xB5, 0x2F, 0xFD}
	initrdData := append(zstdMagic, make([]byte, 100)...)

	layerTar := buildTestKernelTar(t, "initrd.img", initrdData)
	tmpDir := t.TempDir()
	layerPath := filepath.Join(tmpDir, "kernel-layer.tar")
	if err := os.WriteFile(layerPath, layerTar, 0o644); err != nil {
		t.Fatalf("write kernel layer tar: %v", err)
	}

	found, err := CheckInitramfsVirtiofs(layerPath)
	if err == nil {
		t.Fatalf("expected error for zstd initramfs, got found=%v", found)
	}
	if !strings.Contains(err.Error(), "zstd") {
		t.Fatalf("expected zstd-related error, got: %v", err)
	}
}

func TestCheckInitramfsVirtiofs_UnknownInitramfsFormatReturnsError(t *testing.T) {
	t.Parallel()

	unknownData := []byte{0xde, 0xad, 0xbe, 0xef, 0x00, 0x01}
	layerTar := buildTestKernelTar(t, "initrd.img", unknownData)
	tmpDir := t.TempDir()
	layerPath := filepath.Join(tmpDir, "kernel-layer.tar")
	if err := os.WriteFile(layerPath, layerTar, 0o644); err != nil {
		t.Fatalf("write kernel layer tar: %v", err)
	}

	found, err := CheckInitramfsVirtiofs(layerPath)
	if err == nil {
		t.Fatalf("expected error for unknown initramfs format, got found=%v", found)
	}
	if !strings.Contains(err.Error(), "unrecognized initramfs format") {
		t.Fatalf("expected unrecognized format error, got: %v", err)
	}
}

func TestCheckInitramfsVirtiofs_NoInitrdEntry(t *testing.T) {
	t.Parallel()

	// Build a kernel layer tar with only vmlinuz (no initrd).
	layerTar := buildTestKernelTar(t, "vmlinuz", []byte("kernel-binary"))
	tmpDir := t.TempDir()
	layerPath := filepath.Join(tmpDir, "kernel-layer.tar")
	if err := os.WriteFile(layerPath, layerTar, 0o644); err != nil {
		t.Fatalf("write kernel layer tar: %v", err)
	}

	found, err := CheckInitramfsVirtiofs(layerPath)
	if err == nil {
		t.Fatalf("expected error for missing initrd, got found=%v", found)
	}
	if !strings.Contains(err.Error(), "initramfs entry not found") {
		t.Fatalf("expected 'initramfs entry not found' error, got: %v", err)
	}
	if found {
		t.Fatal("found should be false when initrd is missing")
	}
}

func TestContainsVirtiofsModule(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		want bool
	}{
		{name: "bare-ko", path: "lib/modules/6.8.0/kernel/fs/fuse/virtiofs.ko", want: true},
		{name: "xz-compressed", path: "lib/modules/6.8.0/kernel/fs/fuse/virtiofs.ko.xz", want: true},
		{name: "zst-compressed", path: "lib/modules/6.8.0/kernel/fs/fuse/virtiofs.ko.zst", want: true},
		{name: "gz-compressed", path: "lib/modules/6.8.0/kernel/fs/fuse/virtiofs.ko.gz", want: true},
		{name: "different-module", path: "lib/modules/6.8.0/kernel/fs/ext4/ext4.ko", want: false},
		{name: "partial-match", path: "lib/modules/6.8.0/kernel/fs/fuse/virtiofs.ko.bak", want: false},
		{name: "empty", path: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := containsVirtiofsModule(tt.path); got != tt.want {
				t.Fatalf("containsVirtiofsModule(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestIsInitrdEntry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		want bool
	}{
		{name: "bare", path: "initrd.img", want: true},
		{name: "dot-slash", path: "./initrd.img", want: true},
		{name: "slash", path: "/initrd.img", want: true},
		{name: "initramfs.img", path: "initramfs.img", want: true},
		{name: "initrd-bare", path: "initrd", want: true},
		{name: "initramfs-bare", path: "initramfs", want: true},
		{name: "vmlinuz", path: "vmlinuz", want: false},
		{name: "subdir-initrd", path: "boot/initrd.img", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isInitrdEntry(tt.path); got != tt.want {
				t.Fatalf("isInitrdEntry(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestParseHex8(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want uint32
		ok   bool
	}{
		{name: "zero", in: "00000000", want: 0, ok: true},
		{name: "one", in: "00000001", want: 1, ok: true},
		{name: "max", in: "FFFFFFFF", want: 0xFFFFFFFF, ok: true},
		{name: "mixed-case", in: "0000000a", want: 10, ok: true},
		{name: "realistic-filesize", in: "0000A000", want: 0xA000, ok: true},
		{name: "too-short", in: "0000", want: 0, ok: false},
		{name: "invalid-char", in: "0000000G", want: 0, ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := parseHex8([]byte(tt.in))
			if ok != tt.ok {
				t.Fatalf("parseHex8(%q) ok=%v, want %v", tt.in, ok, tt.ok)
			}
			if ok && got != tt.want {
				t.Fatalf("parseHex8(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestAlign4(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   int
		want int
	}{
		{0, 0},
		{1, 4},
		{2, 4},
		{3, 4},
		{4, 4},
		{5, 8},
		{110, 112},
	}

	for _, tt := range tests {
		if got := align4(tt.in); got != tt.want {
			t.Fatalf("align4(%d) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

// buildTestKernelTar creates a tar archive with a single entry.
func buildTestKernelTar(t *testing.T, name string, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	if err := tw.WriteHeader(&tar.Header{
		Name: name,
		Mode: 0o644,
		Size: int64(len(data)),
	}); err != nil {
		t.Fatalf("write tar header for %s: %v", name, err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatalf("write tar data for %s: %v", name, err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	return buf.Bytes()
}

package oci

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
)

// initrdEntryNames lists the tar entry names used for the initramfs inside
// the kernel layer tar. The build pipeline uses "initrd.img"; other names
// are checked for forward compatibility.
var initrdEntryNames = []string{
	"initrd.img",
	"initrd",
	"initramfs.img",
	"initramfs",
}

// CheckInitramfsVirtiofs opens the kernel layer tar at layerBlobPath, locates
// the initramfs entry, decompresses it (gzip or uncompressed cpio), and scans
// the cpio archive for any entry whose path contains "virtiofs.ko".
//
// It returns (found, error). When the initramfs cannot be located or read,
// it returns (false, err) so the caller can decide whether to treat the
// failure as a warning or an error.
func CheckInitramfsVirtiofs(layerBlobPath string) (bool, error) {
	f, err := os.Open(layerBlobPath) //nolint:gosec // G304: path is from local OCI layout
	if err != nil {
		return false, fmt.Errorf("open kernel layer blob: %w", err)
	}
	defer f.Close() //nolint:errcheck

	tr := tar.NewReader(f)
	for {
		hdr, readErr := tr.Next()
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return false, fmt.Errorf("read kernel layer tar: %w", readErr)
		}
		if !isInitrdEntry(hdr.Name) {
			continue
		}

		// Found the initramfs entry. Read it fully so we can inspect it.
		initrdData, readAllErr := io.ReadAll(tr)
		if readAllErr != nil {
			return false, fmt.Errorf("read initramfs entry %q: %w", hdr.Name, readAllErr)
		}
		return scanCPIOForVirtiofs(initrdData)
	}

	return false, fmt.Errorf("initramfs entry not found in kernel layer tar (tried: %s)",
		strings.Join(initrdEntryNames, ", "))
}

// isInitrdEntry checks whether a tar header name matches one of the known
// initramfs filenames (with or without a leading "./" or "/" prefix).
func isInitrdEntry(name string) bool {
	// Normalize: strip leading "./" or "/" to compare bare names.
	clean := strings.TrimPrefix(strings.TrimPrefix(name, "./"), "/")
	return slices.Contains(initrdEntryNames, clean)
}

// scanCPIOForVirtiofs decompresses the initramfs data (detecting gzip by magic
// bytes) and scans the resulting cpio archive for any entry path containing
// "virtiofs.ko".
//
// The cpio format is intentionally parsed with a minimal hand-rolled reader
// rather than importing a full cpio library, keeping dependencies lean.
func scanCPIOForVirtiofs(data []byte) (bool, error) {
	raw, err := decompressInitramfs(data)
	if err != nil {
		return false, fmt.Errorf("decompress initramfs: %w", err)
	}
	return cpioContainsVirtiofs(raw), nil
}

// decompressInitramfs detects the compression format by magic bytes and
// returns the decompressed payload. Supported: gzip, zstd (magic 0x28B52FFD),
// and uncompressed cpio newc/newc-crc ("070701"/"070702").
func decompressInitramfs(data []byte) ([]byte, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("initramfs data too small (%d bytes)", len(data))
	}

	// Gzip: magic 0x1F 0x8B
	if data[0] == 0x1F && data[1] == 0x8B {
		return decompressGzip(data)
	}

	// Zstd: magic 0x28 0xB5 0x2F 0xFD
	if data[0] == 0x28 && data[1] == 0xB5 && data[2] == 0x2F && data[3] == 0xFD {
		// Zstd decompression requires cgo or a large pure-Go dependency.
		// Rather than adding a dependency, return an informative error so
		// the caller can report it as a warning.
		return nil, fmt.Errorf("zstd-compressed initramfs detected; virtiofs check not supported for zstd")
	}

	// Uncompressed cpio newc/newc-crc.
	if hasCPIONewcMagic(data) {
		return data, nil
	}

	return nil, fmt.Errorf("unrecognized initramfs format (magic: %s)", hex.EncodeToString(data[:4]))
}

func hasCPIONewcMagic(data []byte) bool {
	if len(data) < 6 {
		return false
	}
	magic := string(data[:6])
	return magic == "070701" || magic == "070702"
}

// decompressGzip decompresses gzip data.
func decompressGzip(data []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("gzip reader: %w", err)
	}
	defer r.Close() //nolint:errcheck
	out, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("gzip decompress: %w", err)
	}
	return out, nil
}

// cpioContainsVirtiofs scans raw cpio bytes (newc format) for any entry whose
// filename path contains "virtiofs.ko". It uses a minimal cpio newc parser.
//
// newc format (per man 5 cpio):
//
//	"070701" magic (6 bytes ASCII)
//	13 x 8-byte hex fields (104 bytes)
//	filename (null-terminated, padded to 4-byte boundary)
//	file data (padded to 4-byte boundary)
//
// The trailer entry has name "TRAILER!!!".
func cpioContainsVirtiofs(data []byte) bool {
	const (
		magicNewc = "070701"
		magicCrc  = "070702"
		headerLen = 110 // 6 (magic) + 13*8 (fields)
	)

	offset := 0
	for offset+headerLen <= len(data) {
		magic := string(data[offset : offset+6])
		if magic != magicNewc && magic != magicCrc {
			// Not a recognized cpio header; stop scanning.
			return false
		}

		// Parse namesize (hex, 8 chars, offset 94 from header start).
		namesize, ok := parseHex8(data[offset+94 : offset+102])
		if !ok {
			return false
		}

		// Parse filesize (hex, 8 chars, offset 54 from header start).
		filesize, ok := parseHex8(data[offset+54 : offset+62])
		if !ok {
			return false
		}

		// Extract filename.
		nameStart := offset + headerLen
		nameEnd := nameStart + int(namesize)
		if nameEnd > len(data) {
			return false
		}

		// Filename includes trailing NUL; trim it for comparison.
		name := strings.TrimRight(string(data[nameStart:nameEnd]), "\x00")

		// Check for trailer.
		if name == "TRAILER!!!" {
			return false
		}

		// Check if this entry path contains virtiofs.ko (with or without
		// compression extension).
		if containsVirtiofsModule(name) {
			return true
		}

		// Advance past header + name (padded to 4) + data (padded to 4).
		nameTotal := align4(headerLen + int(namesize))
		dataTotal := align4(int(filesize))
		offset += nameTotal + dataTotal
	}

	return false
}

// containsVirtiofsModule returns true if the path contains a virtiofs kernel
// module filename (virtiofs.ko, virtiofs.ko.xz, virtiofs.ko.zst, virtiofs.ko.gz).
func containsVirtiofsModule(path string) bool {
	// Match by checking if any path segment ends with a virtiofs module name.
	suffixes := []string{
		"virtiofs.ko",
		"virtiofs.ko.xz",
		"virtiofs.ko.zst",
		"virtiofs.ko.gz",
	}
	for _, suffix := range suffixes {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}

// parseHex8 parses an 8-byte ASCII hex string into a uint32.
func parseHex8(b []byte) (uint32, bool) {
	if len(b) != 8 {
		return 0, false
	}
	var val uint32
	for _, c := range b {
		val <<= 4
		switch {
		case c >= '0' && c <= '9':
			val |= uint32(c - '0')
		case c >= 'a' && c <= 'f':
			val |= uint32(c - 'a' + 10)
		case c >= 'A' && c <= 'F':
			val |= uint32(c - 'A' + 10)
		default:
			return 0, false
		}
	}
	return val, true
}

// align4 rounds n up to the next multiple of 4.
func align4(n int) int {
	return (n + 3) &^ 3
}

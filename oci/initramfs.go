package oci

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
)

// initramfs entry names to search for inside the kernel layer tar.
var initramfsNames = []string{
	"initrd.img",
	"initrd",
	"initramfs.img",
	"initramfs",
}

// CheckInitramfsVirtiofs opens a kernel layer tar blob, locates the initramfs
// entry, decompresses it (gzip or uncompressed cpio), and scans cpio newc
// entry names for a virtiofs kernel module (virtiofs.ko, .ko.xz, .ko.zst,
// .ko.gz).
//
// Returns (true, nil) if virtiofs module found, (false, nil) if not found or
// no initramfs entry exists, and (false, err) on read/parse errors.
func CheckInitramfsVirtiofs(kernelLayerBlobPath string) (bool, error) {
	initramfsData, err := extractInitramfsFromTar(kernelLayerBlobPath)
	if err != nil {
		return false, err
	}
	if initramfsData == nil {
		// No initramfs entry in the kernel layer tar — not an error,
		// but we cannot check for virtiofs.
		return false, nil
	}

	cpioData, err := decompressInitramfs(initramfsData)
	if err != nil {
		return false, fmt.Errorf("decompress initramfs: %w", err)
	}

	return scanCpioForVirtiofs(cpioData)
}

// extractInitramfsFromTar opens the kernel layer tar and returns the raw bytes
// of the first initramfs entry found. Returns (nil, nil) if no initramfs
// entry is present.
func extractInitramfsFromTar(tarPath string) ([]byte, error) {
	f, err := os.Open(tarPath) //nolint:gosec // G304: tarPath is from local OCI layout blob store
	if err != nil {
		return nil, fmt.Errorf("open kernel layer tar: %w", err)
	}
	defer f.Close() //nolint:errcheck

	nameSet := make(map[string]struct{}, len(initramfsNames))
	for _, n := range initramfsNames {
		nameSet[n] = struct{}{}
	}

	tr := tar.NewReader(f)
	for {
		hdr, rerr := tr.Next()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, fmt.Errorf("read kernel layer tar: %w", rerr)
		}

		// Normalize: strip leading "./" or "/".
		name := strings.TrimPrefix(hdr.Name, "./")
		name = strings.TrimPrefix(name, "/")

		if _, ok := nameSet[name]; ok {
			data, readErr := io.ReadAll(tr)
			if readErr != nil {
				return nil, fmt.Errorf("read initramfs entry %q: %w", hdr.Name, readErr)
			}
			return data, nil
		}
	}

	return nil, nil
}

// decompressInitramfs detects the compression format and returns raw cpio data.
// Supported: gzip (\x1f\x8b), uncompressed cpio (070701 magic).
// Returns an error for zstd (\x28\xb5\x2f\xfd) since stdlib doesn't support it.
func decompressInitramfs(data []byte) ([]byte, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("initramfs data too short (%d bytes)", len(data))
	}

	// Gzip magic: 0x1f 0x8b
	if data[0] == 0x1f && data[1] == 0x8b {
		gz, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("open gzip reader: %w", err)
		}
		defer gz.Close() //nolint:errcheck
		decompressed, err := io.ReadAll(gz)
		if err != nil {
			return nil, fmt.Errorf("decompress gzip: %w", err)
		}
		return decompressed, nil
	}

	// Zstd magic: 0x28 0xb5 0x2f 0xfd
	if data[0] == 0x28 && data[1] == 0xb5 && data[2] == 0x2f && data[3] == 0xfd {
		return nil, fmt.Errorf("zstd-compressed initramfs is not supported: stdlib does not include a zstd decoder; re-build the initramfs with gzip or install a zstd-capable kernel")
	}

	// Uncompressed cpio newc magic: "070701" (ASCII).
	if len(data) >= 6 && string(data[:6]) == "070701" {
		return data, nil
	}

	return nil, fmt.Errorf("unrecognized initramfs format (magic: %s)", hex.EncodeToString(data[:4]))
}

// scanCpioForVirtiofs parses a cpio newc archive and returns true if any entry
// name contains "virtiofs.ko" (covers .ko, .ko.xz, .ko.zst, .ko.gz).
//
// The cpio newc format (see "man 5 cpio") uses fixed-size ASCII hex headers:
//
//	Offset  Length  Field
//	0       6       magic "070701"
//	6       8       inode
//	14      8       mode
//	22      8       uid
//	30      8       gid
//	38      8       nlink
//	46      8       mtime
//	54      8       filesize
//	62      8       devmajor
//	70      8       devminor
//	78      8       rdevmajor
//	86      8       rdevminor
//	94      8       namesize
//	102     8       checksum
//	110     (namesize bytes, padded to 4-byte boundary)
//	        (filesize bytes, padded to 4-byte boundary)
func scanCpioForVirtiofs(data []byte) (bool, error) {
	const headerLen = 110 // fixed cpio newc header size

	offset := 0
	for offset+headerLen <= len(data) {
		// Verify magic.
		magic := string(data[offset : offset+6])
		if magic != "070701" {
			// End of archive or corrupted — stop scanning.
			break
		}

		// Parse namesize (8 hex chars at offset 94).
		namesize, err := parseHex8(data[offset+94 : offset+102])
		if err != nil {
			return false, fmt.Errorf("parse cpio namesize at offset %d: %w", offset, err)
		}

		// Parse filesize (8 hex chars at offset 54).
		filesize, err := parseHex8(data[offset+54 : offset+62])
		if err != nil {
			return false, fmt.Errorf("parse cpio filesize at offset %d: %w", offset, err)
		}

		// Read entry name.
		nameStart := offset + headerLen
		nameEnd := nameStart + int(namesize)
		if nameEnd > len(data) {
			break
		}

		// Name includes trailing NUL byte — strip it.
		name := string(data[nameStart:nameEnd])
		name = strings.TrimRight(name, "\x00")

		// TRAILER!!! marks end of archive.
		if name == "TRAILER!!!" {
			break
		}

		// Check for virtiofs module.
		if strings.Contains(name, "virtiofs.ko") {
			return true, nil
		}

		// Advance past header + name (padded to 4 bytes) + file data (padded to 4 bytes).
		nameTotal := alignUp(headerLen+int(namesize), 4)
		dataTotal := alignUp(int(filesize), 4)
		offset += nameTotal + dataTotal
	}

	return false, nil
}

// parseHex8 parses 8 bytes of ASCII hexadecimal into a uint32.
func parseHex8(b []byte) (uint32, error) {
	if len(b) != 8 {
		return 0, fmt.Errorf("expected 8 hex bytes, got %d", len(b))
	}
	decoded, err := hex.DecodeString(string(b))
	if err != nil {
		return 0, fmt.Errorf("decode hex %q: %w", string(b), err)
	}
	return binary.BigEndian.Uint32(decoded), nil
}

// alignUp rounds n up to the next multiple of align.
func alignUp(n, align int) int {
	return (n + align - 1) &^ (align - 1)
}

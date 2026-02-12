package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"runtime"
)

const (
	// checksumHexLen is the number of hex characters used for the truncated
	// checksum (16 hex chars = 64 bits).
	checksumHexLen = 16
)

// computeFileChecksum computes a SHA-256 checksum of a file's content.
// Returns the full 64-char hex digest and the truncated 16-char checksum.
// This is used for cloud images (qcow2/img files) and URL-downloaded images
// where the content itself serves as the identity.
func computeFileChecksum(path string) (fullDigest string, checksum string, err error) {
	f, err := os.Open(path) //nolint:gosec // G304: path is an internal image file path for checksum computation
	if err != nil {
		return "", "", fmt.Errorf("open file for checksum: %w", err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", "", fmt.Errorf("read file for checksum: %w", err)
	}

	fullDigest = hex.EncodeToString(h.Sum(nil))
	checksum = fullDigest[:checksumHexLen]
	return fullDigest, checksum, nil
}

// goarchToOCI maps Go's runtime.GOARCH values to OCI platform architecture
// strings. This is used when computing checksums for the local platform.
func goarchToOCI(goarch string) string {
	switch goarch {
	case "amd64":
		return "amd64"
	case "arm64":
		return "arm64"
	default:
		return goarch
	}
}

// defaultArch returns the OCI architecture string for the current platform.
func defaultArch() string {
	return goarchToOCI(runtime.GOARCH)
}

package oci

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ParseSHA256Digest validates a "sha256:<hex>" digest string and returns the
// lowercase 64-character hex portion. It rejects digests that use a different
// algorithm, have the wrong length, or contain non-hex characters.
func ParseSHA256Digest(digest string) (string, error) {
	const prefix = "sha256:"
	if !strings.HasPrefix(digest, prefix) {
		return "", fmt.Errorf("unsupported digest format %q", digest)
	}
	hex := strings.TrimPrefix(digest, prefix)
	if len(hex) != 64 {
		return "", fmt.Errorf("invalid digest length for %q", digest)
	}
	for _, c := range hex {
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return "", fmt.Errorf("invalid digest characters for %q", digest)
		}
	}
	return strings.ToLower(hex), nil
}

// LayoutBlobPath returns the filesystem path for a blob within an OCI image
// layout directory. digest must be in "sha256:<hex>" format.
func LayoutBlobPath(layoutPath, digest string) (string, error) {
	hex, err := ParseSHA256Digest(digest)
	if err != nil {
		return "", err
	}
	return filepath.Join(layoutPath, "blobs", "sha256", hex), nil
}

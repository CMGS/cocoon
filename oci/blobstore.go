package oci

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/CMGS/cocoon/config"
)

// BlobStore manages a content-addressed blob store at cache/oci/blobs/sha256/.
// Blobs are shared across OCI layouts via hardlinks.
type BlobStore struct {
	cfg *config.CocoonConfig
}

// NewBlobStore creates a BlobStore for the given config.
func NewBlobStore(cfg *config.CocoonConfig) *BlobStore {
	return &BlobStore{cfg: cfg}
}

// validateHexDigest checks that digest contains only hexadecimal characters.
// This prevents path traversal via digest parameters.
func validateHexDigest(digest string) error {
	if digest == "" {
		return fmt.Errorf("empty digest")
	}
	for _, c := range digest {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return fmt.Errorf("invalid character %q in digest %q", string(c), digest)
		}
	}
	return nil
}

// BlobPath returns the shared blob path for a hex digest (without "sha256:" prefix).
func (bs *BlobStore) BlobPath(digest string) string {
	return filepath.Join(bs.cfg.OCIBlobDir(), digest)
}

// BlobExists checks if a blob exists in the shared store.
func (bs *BlobStore) BlobExists(digest string) bool {
	_, err := os.Stat(bs.BlobPath(digest))
	return err == nil
}

// StoreBlob copies a file into the shared blob store if not already present.
// digest is the hex sha256 (without "sha256:" prefix).
// Returns the blob path in the shared store.
func (bs *BlobStore) StoreBlob(srcPath, digest string) (string, error) {
	if err := validateHexDigest(digest); err != nil {
		return "", err
	}
	blobPath := bs.BlobPath(digest)
	if bs.BlobExists(digest) {
		return blobPath, nil
	}

	if err := os.MkdirAll(filepath.Dir(blobPath), 0o750); err != nil {
		return "", fmt.Errorf("create blob dir: %w", err)
	}

	// Write to temp file then rename for atomicity.
	tmp, err := os.CreateTemp(filepath.Dir(blobPath), ".blob-*")
	if err != nil {
		return "", fmt.Errorf("create temp blob: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		// Clean up temp file on any failure.
		if _, statErr := os.Stat(tmpPath); statErr == nil {
			_ = os.Remove(tmpPath)
		}
	}()

	sf, err := os.Open(srcPath) //nolint:gosec // G304: srcPath is a build pipeline temp file validated by caller
	if err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("open source %s: %w", srcPath, err)
	}
	defer sf.Close() //nolint:errcheck

	// Compute digest during copy to verify content matches the expected digest.
	h := sha256.New()
	if _, err = io.Copy(io.MultiWriter(tmp, h), sf); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("copy blob %s: %w", digest, err)
	}

	actualDigest := hex.EncodeToString(h.Sum(nil))
	if actualDigest != digest {
		_ = tmp.Close()
		return "", fmt.Errorf("blob digest mismatch: expected %s, got %s", digest, actualDigest)
	}

	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("sync blob %s: %w", digest, err)
	}
	if err = tmp.Close(); err != nil {
		return "", fmt.Errorf("close blob %s: %w", digest, err)
	}

	// Atomic rename. If another process raced us, this may overwrite
	// their file -- but both are identical content-addressed data.
	if err = os.Rename(tmpPath, blobPath); err != nil {
		return "", fmt.Errorf("rename blob %s: %w", digest, err)
	}
	return blobPath, nil
}

// StoreBlobFromBytes writes data into the shared blob store if not already present.
// digest is the hex sha256 (without "sha256:" prefix).
func (bs *BlobStore) StoreBlobFromBytes(data []byte, digest string) (string, error) {
	if err := validateHexDigest(digest); err != nil {
		return "", err
	}
	blobPath := bs.BlobPath(digest)
	if bs.BlobExists(digest) {
		return blobPath, nil
	}

	if err := os.MkdirAll(filepath.Dir(blobPath), 0o750); err != nil {
		return "", fmt.Errorf("create blob dir: %w", err)
	}

	// Verify content matches the provided digest.
	h := sha256.Sum256(data)
	actualDigest := hex.EncodeToString(h[:])
	if actualDigest != digest {
		return "", fmt.Errorf("blob digest mismatch: expected %s, got %s", digest, actualDigest)
	}

	// Write to temp file then rename for atomicity.
	tmp, err := os.CreateTemp(filepath.Dir(blobPath), ".blob-*")
	if err != nil {
		return "", fmt.Errorf("create temp blob: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		if _, statErr := os.Stat(tmpPath); statErr == nil {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write blob %s: %w", digest, err)
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("sync blob %s: %w", digest, err)
	}
	if err = tmp.Close(); err != nil {
		return "", fmt.Errorf("close blob %s: %w", digest, err)
	}

	if err = os.Rename(tmpPath, blobPath); err != nil {
		return "", fmt.Errorf("rename blob %s: %w", digest, err)
	}
	return blobPath, nil
}

// LinkBlobToLayout creates a hardlink from the shared blob to the layout's blobs/sha256/ dir.
// Falls back to copy if hardlink fails (e.g., cross-device).
func (bs *BlobStore) LinkBlobToLayout(digest, layoutDir string) error {
	if err := validateHexDigest(digest); err != nil {
		return err
	}
	src := bs.BlobPath(digest)
	dstDir := filepath.Join(layoutDir, "blobs", "sha256")
	if err := os.MkdirAll(dstDir, 0o750); err != nil {
		return fmt.Errorf("create layout blobs dir: %w", err)
	}

	dst := filepath.Join(dstDir, digest)
	if err := os.Link(src, dst); err == nil {
		return nil
	} else if os.IsExist(err) {
		return nil // already linked
	}
	// Fallback: copy (e.g., cross-device link not supported).
	return copyFileSync(src, dst)
}

// RemoveBlob removes a blob from the shared store (called by GC only).
func (bs *BlobStore) RemoveBlob(digest string) error {
	if err := validateHexDigest(digest); err != nil {
		return err
	}
	return os.Remove(bs.BlobPath(digest))
}

// copyFileSync copies src to dst with fsync for durability.
func copyFileSync(src, dst string) error {
	sf, err := os.Open(src) //nolint:gosec // G304: src is a shared blob store path
	if err != nil {
		return err
	}
	defer sf.Close() //nolint:errcheck

	df, err := os.Create(dst) //nolint:gosec // G304: dst is a layout blob path
	if err != nil {
		return err
	}

	if _, err = io.Copy(df, sf); err != nil {
		_ = df.Close()
		_ = os.Remove(dst)
		return err
	}
	if err = df.Sync(); err != nil {
		_ = df.Close()
		_ = os.Remove(dst)
		return err
	}
	if err = df.Close(); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return nil
}

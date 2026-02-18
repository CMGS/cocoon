package oci

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestBlobStoreStoreAndRetrieve(t *testing.T) {
	cfg := testConfig(t)
	bs := NewBlobStore(cfg)

	// Create a test blob.
	data := []byte("hello world blob content")
	h := sha256.Sum256(data)
	digest := hex.EncodeToString(h[:])

	// Write data to a temp source file.
	srcPath := filepath.Join(t.TempDir(), "source.bin")
	if err := os.WriteFile(srcPath, data, 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	// Store the blob.
	blobPath, err := bs.StoreBlob(srcPath, digest)
	if err != nil {
		t.Fatalf("StoreBlob: %v", err)
	}

	// Verify blob exists.
	if !bs.BlobExists(digest) {
		t.Fatal("BlobExists returned false after StoreBlob")
	}

	// Verify content.
	got, err := os.ReadFile(blobPath)
	if err != nil {
		t.Fatalf("ReadFile blob: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("blob content mismatch: got %q, want %q", got, data)
	}

	// Store again (should be idempotent).
	blobPath2, err := bs.StoreBlob(srcPath, digest)
	if err != nil {
		t.Fatalf("StoreBlob (idempotent): %v", err)
	}
	if blobPath2 != blobPath {
		t.Fatalf("StoreBlob path changed: %s vs %s", blobPath, blobPath2)
	}
}

func TestBlobStoreStoreBlobFromBytes(t *testing.T) {
	cfg := testConfig(t)
	bs := NewBlobStore(cfg)

	data := []byte(`{"config": "test"}`)
	h := sha256.Sum256(data)
	digest := hex.EncodeToString(h[:])

	blobPath, err := bs.StoreBlobFromBytes(data, digest)
	if err != nil {
		t.Fatalf("StoreBlobFromBytes: %v", err)
	}

	got, err := os.ReadFile(blobPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("content mismatch: got %q, want %q", got, data)
	}
}

func TestBlobStoreLinkToLayout(t *testing.T) {
	cfg := testConfig(t)
	bs := NewBlobStore(cfg)

	data := []byte("link test data")
	h := sha256.Sum256(data)
	digest := hex.EncodeToString(h[:])

	if _, err := bs.StoreBlobFromBytes(data, digest); err != nil {
		t.Fatalf("StoreBlobFromBytes: %v", err)
	}

	layoutDir := filepath.Join(t.TempDir(), "test-layout")
	if err := bs.LinkBlobToLayout(digest, layoutDir); err != nil {
		t.Fatalf("LinkBlobToLayout: %v", err)
	}

	// Verify the linked file exists and has correct content.
	linkedPath := filepath.Join(layoutDir, "blobs", "sha256", digest)
	got, err := os.ReadFile(linkedPath)
	if err != nil {
		t.Fatalf("ReadFile linked blob: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("linked content mismatch: got %q, want %q", got, data)
	}
}

func TestBlobStoreRemove(t *testing.T) {
	cfg := testConfig(t)
	bs := NewBlobStore(cfg)

	data := []byte("remove test")
	h := sha256.Sum256(data)
	digest := hex.EncodeToString(h[:])

	if _, err := bs.StoreBlobFromBytes(data, digest); err != nil {
		t.Fatalf("StoreBlobFromBytes: %v", err)
	}

	if !bs.BlobExists(digest) {
		t.Fatal("blob should exist before removal")
	}

	if err := bs.RemoveBlob(digest); err != nil {
		t.Fatalf("RemoveBlob: %v", err)
	}

	if bs.BlobExists(digest) {
		t.Fatal("blob should not exist after removal")
	}
}

func TestBlobStoreInvalidDigest(t *testing.T) {
	cfg := testConfig(t)
	bs := NewBlobStore(cfg)

	// Path traversal attempt.
	_, err := bs.StoreBlobFromBytes([]byte("x"), "../../etc/passwd")
	if err == nil {
		t.Fatal("expected error for path traversal digest")
	}

	// Empty digest.
	_, err = bs.StoreBlobFromBytes([]byte("x"), "")
	if err == nil {
		t.Fatal("expected error for empty digest")
	}
}

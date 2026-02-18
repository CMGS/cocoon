package oci

import (
	"testing"
)

func TestAddAndRemoveBlobRefs(t *testing.T) {
	cfg := testConfig(t)

	manifest1 := "aaaa"
	blobs := []string{"blob1", "blob2", "blob3"}
	sizes := []int64{100, 200, 300}

	// Add blob refs for manifest1.
	if err := AddBlobRefs(cfg, manifest1, blobs, sizes); err != nil {
		t.Fatalf("AddBlobRefs: %v", err)
	}

	// Verify no unreferenced blobs.
	unreferenced, err := GetUnreferencedBlobs(cfg)
	if err != nil {
		t.Fatalf("GetUnreferencedBlobs: %v", err)
	}
	if len(unreferenced) != 0 {
		t.Fatalf("expected 0 unreferenced blobs, got %d", len(unreferenced))
	}

	// Add another manifest referencing some of the same blobs.
	manifest2 := "bbbb"
	if err := AddBlobRefs(cfg, manifest2, []string{"blob1", "blob4"}, []int64{100, 400}); err != nil {
		t.Fatalf("AddBlobRefs (manifest2): %v", err)
	}

	// Remove manifest1's refs. blob2 and blob3 should become zero-ref.
	zeroRef, err := RemoveBlobRefs(cfg, manifest1)
	if err != nil {
		t.Fatalf("RemoveBlobRefs: %v", err)
	}

	zeroRefSet := make(map[string]bool)
	for _, d := range zeroRef {
		zeroRefSet[d] = true
	}

	if !zeroRefSet["blob2"] {
		t.Error("blob2 should be zero-ref after removing manifest1")
	}
	if !zeroRefSet["blob3"] {
		t.Error("blob3 should be zero-ref after removing manifest1")
	}
	if zeroRefSet["blob1"] {
		t.Error("blob1 should NOT be zero-ref (still ref'd by manifest2)")
	}
}

func TestAddBlobRefsDuplicateManifest(t *testing.T) {
	cfg := testConfig(t)

	// Adding same manifest twice should not duplicate.
	if err := AddBlobRefs(cfg, "m1", []string{"b1"}, []int64{100}); err != nil {
		t.Fatalf("first AddBlobRefs: %v", err)
	}
	if err := AddBlobRefs(cfg, "m1", []string{"b1"}, []int64{100}); err != nil {
		t.Fatalf("second AddBlobRefs: %v", err)
	}

	// Remove once should make it zero-ref.
	zeroRef, err := RemoveBlobRefs(cfg, "m1")
	if err != nil {
		t.Fatalf("RemoveBlobRefs: %v", err)
	}
	if len(zeroRef) != 1 || zeroRef[0] != "b1" {
		t.Fatalf("expected [b1], got %v", zeroRef)
	}
}

func TestGetAllTrackedBlobs(t *testing.T) {
	cfg := testConfig(t)

	if err := AddBlobRefs(cfg, "m1", []string{"b1", "b2"}, []int64{100, 200}); err != nil {
		t.Fatalf("AddBlobRefs: %v", err)
	}

	tracked, err := GetAllTrackedBlobs(cfg)
	if err != nil {
		t.Fatalf("GetAllTrackedBlobs: %v", err)
	}
	if len(tracked) != 2 {
		t.Fatalf("expected 2 tracked blobs, got %d", len(tracked))
	}
}

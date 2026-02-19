package oci

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	typesreg "github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/google/go-containerregistry/pkg/registry"

	"github.com/CMGS/cocoon/types"
)

// setupTestRegistry starts an in-process OCI registry and returns
// the host:port string to use as registry address.
func setupTestRegistry(t *testing.T) string {
	t.Helper()
	handler := registry.New()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// staticLayer implements partial.CompressedLayer for testing.
type staticLayer struct {
	content   []byte
	mediaType typesreg.MediaType
}

func (l *staticLayer) Digest() (v1.Hash, error) {
	h := sha256.Sum256(l.content)
	return v1.Hash{Algorithm: "sha256", Hex: hex.EncodeToString(h[:])}, nil
}

func (l *staticLayer) Compressed() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(l.content)), nil
}

func (l *staticLayer) Size() (int64, error) {
	return int64(len(l.content)), nil
}

func (l *staticLayer) MediaType() (typesreg.MediaType, error) {
	return l.mediaType, nil
}

// pushTestCocoonVMImage creates and pushes a minimal Cocoon VM image to the
// test registry. Returns the full ref and manifest digest.
func pushTestCocoonVMImage(t *testing.T, registryAddr, repo, tag string) (string, v1.Hash) {
	t.Helper()

	var err error
	kernelLayer, err := partial.CompressedToLayer(&staticLayer{
		content:   []byte("fake-kernel-content"),
		mediaType: typesreg.MediaType(MediaTypeKernelLayer),
	})
	if err != nil {
		t.Fatalf("create kernel layer: %v", err)
	}

	rootfsLayer, err := partial.CompressedToLayer(&staticLayer{
		content:   []byte("fake-rootfs-content"),
		mediaType: typesreg.MediaType(MediaTypeRootfsLayer),
	})
	if err != nil {
		t.Fatalf("create rootfs layer: %v", err)
	}

	// Build image: set config media type to our VM config type.
	img := mutate.MediaType(empty.Image, typesreg.OCIManifestSchema1)
	img = mutate.ConfigMediaType(img, typesreg.MediaType(MediaTypeVMConfig))

	// Override raw config.
	img, err = mutate.AppendLayers(img, kernelLayer, rootfsLayer)
	if err != nil {
		t.Fatalf("append layers: %v", err)
	}

	// For a simpler approach, push the image as-is — the config blob will be
	// the default empty config, but the config.MediaType in the manifest will
	// be MediaTypeVMConfig which is what validateCocoonVMManifest checks.

	ref := fmt.Sprintf("%s/%s:%s", registryAddr, repo, tag)
	parsedRef, err := name.NewTag(ref, name.Insecure)
	if err != nil {
		t.Fatalf("parse ref %q: %v", ref, err)
	}

	if err := remote.Write(parsedRef, img); err != nil {
		t.Fatalf("push image to test registry: %v", err)
	}

	digest, err := img.Digest()
	if err != nil {
		t.Fatalf("get image digest: %v", err)
	}

	return ref, digest
}

// pushTestNonVMImage pushes a standard OCI image (not a Cocoon VM) to the test registry.
func pushTestNonVMImage(t *testing.T, registryAddr, repo, tag string) string {
	t.Helper()

	layer, err := partial.CompressedToLayer(&staticLayer{
		content:   []byte("standard-layer-content"),
		mediaType: typesreg.OCILayer,
	})
	if err != nil {
		t.Fatalf("create layer: %v", err)
	}

	img := empty.Image
	img, err = mutate.AppendLayers(img, layer)
	if err != nil {
		t.Fatalf("append layer: %v", err)
	}

	ref := fmt.Sprintf("%s/%s:%s", registryAddr, repo, tag)
	parsedRef, err := name.NewTag(ref, name.Insecure)
	if err != nil {
		t.Fatalf("parse ref %q: %v", ref, err)
	}

	if err := remote.Write(parsedRef, img); err != nil {
		t.Fatalf("push image to test registry: %v", err)
	}

	return ref
}

func TestPull_ValidCocoonVMImage(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	regAddr := setupTestRegistry(t)
	ref, expectedDigest := pushTestCocoonVMImage(t, regAddr, "test/myvm", "v1")

	result, err := Pull(t.Context(), cfg, ref)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	if result.Ref != ref {
		t.Errorf("Ref = %q, want %q", result.Ref, ref)
	}
	if result.ManifestDigest != expectedDigest.String() {
		t.Errorf("ManifestDigest = %q, want %q", result.ManifestDigest, expectedDigest.String())
	}
	if result.LayoutPath == "" {
		t.Fatal("LayoutPath is empty")
	}

	// Verify layout exists and is valid.
	info, err := InspectLayout(result.LayoutPath)
	if err != nil {
		t.Fatalf("InspectLayout: %v", err)
	}
	if info.ManifestDigest != expectedDigest.String() {
		t.Errorf("layout ManifestDigest = %q, want %q", info.ManifestDigest, expectedDigest.String())
	}

	// Verify tag is registered.
	store := NewStore(cfg)
	has, err := store.HasTag(ref)
	if err != nil {
		t.Fatalf("HasTag: %v", err)
	}
	if !has {
		t.Fatalf("tag %q not found in store", ref)
	}
}

func TestPull_IdempotentRepull(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	regAddr := setupTestRegistry(t)
	ref, _ := pushTestCocoonVMImage(t, regAddr, "test/repull", "v1")

	result1, err := Pull(t.Context(), cfg, ref)
	if err != nil {
		t.Fatalf("first Pull: %v", err)
	}

	result2, err := Pull(t.Context(), cfg, ref)
	if err != nil {
		t.Fatalf("second Pull: %v", err)
	}

	if result1.ManifestDigest != result2.ManifestDigest {
		t.Errorf("digest mismatch: %q vs %q", result1.ManifestDigest, result2.ManifestDigest)
	}
	if result1.LayoutPath != result2.LayoutPath {
		t.Errorf("layout path mismatch: %q vs %q", result1.LayoutPath, result2.LayoutPath)
	}
}

func TestPull_RejectsNonVMImage(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	regAddr := setupTestRegistry(t)
	ref := pushTestNonVMImage(t, regAddr, "test/notvm", "v1")

	_, err := Pull(t.Context(), cfg, ref)
	if err == nil {
		t.Fatal("expected error for non-VM image, got nil")
	}
	// Should be permanent (not transient).
	if types.IsTransient(err) {
		t.Errorf("expected permanent error, got transient: %v", err)
	}
}

func TestPull_InvalidRef(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)

	_, err := Pull(t.Context(), cfg, "!!invalid!!")
	if err == nil {
		t.Fatal("expected error for invalid ref, got nil")
	}
	if types.IsTransient(err) {
		t.Errorf("expected permanent error, got transient: %v", err)
	}
}

func TestPull_BlobDeduplication(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	regAddr := setupTestRegistry(t)

	ref1, _ := pushTestCocoonVMImage(t, regAddr, "test/dedup", "v1")
	ref2, _ := pushTestCocoonVMImage(t, regAddr, "test/dedup", "v2")

	_, err := Pull(t.Context(), cfg, ref1)
	if err != nil {
		t.Fatalf("Pull ref1: %v", err)
	}
	_, err = Pull(t.Context(), cfg, ref2)
	if err != nil {
		t.Fatalf("Pull ref2: %v", err)
	}

	blobDir := cfg.OCIBlobDir()
	entries, err := os.ReadDir(blobDir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", blobDir, err)
	}
	if len(entries) == 0 {
		t.Fatal("expected blobs in store, got none")
	}
}

func TestPull_LayerCountValidation(t *testing.T) {
	t.Parallel()

	layers := make([]ociDescriptor, 35)
	for i := range layers {
		layers[i] = ociDescriptor{
			MediaType: MediaTypeRootfsLayer,
			Digest:    fmt.Sprintf("sha256:%064d", i),
			Size:      100,
		}
	}
	manifest := ociManifest{
		SchemaVersion: 2,
		Config: ociDescriptor{
			MediaType: MediaTypeVMConfig,
			Digest:    fmt.Sprintf("sha256:%064d", 99),
			Size:      100,
		},
		Layers: layers,
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if err := validateCocoonVMManifest(data); err == nil {
		t.Fatal("expected error for 35 layers, got nil")
	}
}

func TestValidateCocoonVMManifest_ValidArtifactType(t *testing.T) {
	t.Parallel()
	manifest := ociManifest{
		SchemaVersion: 2,
		ArtifactType:  ArtifactTypeVMImage,
		Config: ociDescriptor{
			MediaType: MediaTypeVMConfig,
			Digest:    "sha256:0000000000000000000000000000000000000000000000000000000000000000",
			Size:      2,
		},
		Layers: []ociDescriptor{
			{MediaType: MediaTypeKernelLayer, Digest: "sha256:0000000000000000000000000000000000000000000000000000000000000001", Size: 100},
			{MediaType: MediaTypeRootfsLayer, Digest: "sha256:0000000000000000000000000000000000000000000000000000000000000002", Size: 200},
		},
	}
	data, _ := json.Marshal(manifest)
	if err := validateCocoonVMManifest(data); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateCocoonVMManifest_InvalidManifest(t *testing.T) {
	t.Parallel()
	manifest := ociManifest{
		SchemaVersion: 2,
		Config: ociDescriptor{
			MediaType: "application/vnd.oci.image.config.v1+json",
			Digest:    "sha256:0000000000000000000000000000000000000000000000000000000000000000",
			Size:      100,
		},
		Layers: []ociDescriptor{
			{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Digest: "sha256:0000000000000000000000000000000000000000000000000000000000000001", Size: 100},
		},
	}
	data, _ := json.Marshal(manifest)
	if err := validateCocoonVMManifest(data); err == nil {
		t.Fatal("expected error for non-VM manifest, got nil")
	}
}

func TestValidateCocoonVMManifest_RejectsMarkerOnlyManifest(t *testing.T) {
	t.Parallel()
	manifest := ociManifest{
		SchemaVersion: 2,
		ArtifactType:  ArtifactTypeVMImage,
		Config: ociDescriptor{
			MediaType: MediaTypeVMConfig,
			Digest:    "sha256:0000000000000000000000000000000000000000000000000000000000000000",
			Size:      100,
		},
		Layers: nil,
	}
	data, _ := json.Marshal(manifest)
	if err := validateCocoonVMManifest(data); err == nil {
		t.Fatal("expected error for marker-only manifest without runtime layers, got nil")
	}
}

func TestFormatPullLayerProgressLine(t *testing.T) {
	t.Parallel()

	line := formatPullLayerProgressLine(2, 3, "abcdef012345", 512, 1024)
	if !strings.Contains(line, "Layer 2/3") {
		t.Fatalf("expected layer position in line, got %q", line)
	}
	if !strings.Contains(line, "sha256:abcdef012345") {
		t.Fatalf("expected digest short in line, got %q", line)
	}
	if !strings.Contains(line, "50%") {
		t.Fatalf("expected percent in line, got %q", line)
	}
	if !strings.Contains(line, "512B / 1.0KB") {
		t.Fatalf("expected byte progress in line, got %q", line)
	}
}

func TestFormatPullLayerProgressLine_NoTotal(t *testing.T) {
	t.Parallel()

	line := formatPullLayerProgressLine(1, 2, "abcdef012345", 2048, 0)
	if !strings.Contains(line, "Layer 1/2") {
		t.Fatalf("expected layer position in line, got %q", line)
	}
	if strings.Contains(line, "%") {
		t.Fatalf("expected no percent when total is unknown, got %q", line)
	}
	if !strings.Contains(line, "2.0KB") {
		t.Fatalf("expected byte counter in line, got %q", line)
	}
}

func TestValidateCocoonVMManifest_RejectsUnknownLayerTypes(t *testing.T) {
	t.Parallel()
	manifest := ociManifest{
		SchemaVersion: 2,
		Config: ociDescriptor{
			MediaType: MediaTypeVMConfig,
			Digest:    "sha256:0000000000000000000000000000000000000000000000000000000000000000",
			Size:      100,
		},
		Layers: []ociDescriptor{
			{MediaType: MediaTypeKernelLayer, Digest: "sha256:0000000000000000000000000000000000000000000000000000000000000001", Size: 100},
			{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Digest: "sha256:0000000000000000000000000000000000000000000000000000000000000002", Size: 100},
		},
	}
	data, _ := json.Marshal(manifest)
	if err := validateCocoonVMManifest(data); err == nil {
		t.Fatal("expected error for unsupported layer media type, got nil")
	}
}

func TestClassifyPullError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		err           error
		wantTransient bool
	}{
		{name: "unauthorized is permanent", err: fmt.Errorf("UNAUTHORIZED"), wantTransient: false},
		{name: "manifest unknown is permanent", err: fmt.Errorf("MANIFEST_UNKNOWN"), wantTransient: false},
		{name: "timeout is transient", err: fmt.Errorf("connection timeout"), wantTransient: true},
		{name: "503 is transient", err: fmt.Errorf("503 Service Unavailable"), wantTransient: true},
		{
			name:          "net.OpError is transient",
			err:           &net.OpError{Op: "dial", Net: "tcp", Err: fmt.Errorf("refused")},
			wantTransient: true,
		},
		{name: "unknown is permanent", err: fmt.Errorf("something weird"), wantTransient: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			classified := classifyPullError(tt.err)
			if types.IsTransient(classified) != tt.wantTransient {
				t.Errorf("IsTransient = %v, want %v", types.IsTransient(classified), tt.wantTransient)
			}
		})
	}
}

func TestCheckIdempotent(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	store := NewStore(cfg)

	found, _ := checkIdempotent(store, "notexist:latest", "deadbeef")
	if found {
		t.Fatal("expected not found for missing tag")
	}
}

func TestStripSHA256Prefix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
	}{
		{"sha256:abcdef", "abcdef"},
		{"abcdef", "abcdef"},
		{"sha256:", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := stripSHA256Prefix(tt.in); got != tt.want {
			t.Errorf("stripSHA256Prefix(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestCreatePullLayoutWorkDir(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	dir, err := createPullLayoutWorkDir(cfg)
	if err != nil {
		t.Fatalf("createPullLayoutWorkDir: %v", err)
	}
	defer os.RemoveAll(dir) //nolint:errcheck

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("layout work dir does not exist: %v", err)
	}
	if filepath.Dir(filepath.Dir(dir)) == cfg.OCILayoutDir() {
		t.Fatalf("work dir %s should not be under layouts dir %s", dir, cfg.OCILayoutDir())
	}
}

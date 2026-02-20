package oci

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// buildTestOCILayout creates a minimal OCI layout directory with the given
// layers and returns the layout path and LayoutInfo.
func buildTestOCILayout(t *testing.T, layers []testLayerSpec) (string, *LayoutInfo) {
	t.Helper()

	layoutPath := filepath.Join(t.TempDir(), "layout")
	blobsDir := filepath.Join(layoutPath, "blobs", "sha256")
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		t.Fatalf("create blobs dir: %v", err)
	}

	// Build each layer blob.
	layerInfos := make([]LayerInfo, 0, len(layers))
	for _, spec := range layers {
		var tarData []byte
		if spec.compressed {
			tarData = buildGzippedTarBlob(t, spec.entries)
		} else {
			tarData = buildTarBlob(t, spec.entries)
		}

		hash := sha256.Sum256(tarData)
		digestHex := hex.EncodeToString(hash[:])
		if err := os.WriteFile(filepath.Join(blobsDir, digestHex), tarData, 0o644); err != nil {
			t.Fatalf("write layer blob: %v", err)
		}

		layerInfos = append(layerInfos, LayerInfo{
			MediaType: spec.mediaType,
			Digest:    "sha256:" + digestHex,
			Size:      int64(len(tarData)),
		})
	}

	// Build a minimal config blob.
	configData := []byte(`{"architecture":"amd64","os":"linux"}`)
	configHash := sha256.Sum256(configData)
	configDigest := hex.EncodeToString(configHash[:])
	if err := os.WriteFile(filepath.Join(blobsDir, configDigest), configData, 0o644); err != nil {
		t.Fatalf("write config blob: %v", err)
	}

	// Build manifest.
	manifestLayers := make([]ociDescriptor, 0, len(layerInfos))
	for _, li := range layerInfos {
		manifestLayers = append(manifestLayers, ociDescriptor{
			MediaType: li.MediaType,
			Digest:    li.Digest,
			Size:      li.Size,
		})
	}
	manifest := ociManifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.manifest.v1+json",
		Config: ociDescriptor{
			MediaType: "application/vnd.oci.image.config.v1+json",
			Digest:    "sha256:" + configDigest,
			Size:      int64(len(configData)),
		},
		Layers: manifestLayers,
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	manifestHash := sha256.Sum256(manifestData)
	manifestDigest := hex.EncodeToString(manifestHash[:])
	if err := os.WriteFile(filepath.Join(blobsDir, manifestDigest), manifestData, 0o644); err != nil {
		t.Fatalf("write manifest blob: %v", err)
	}

	// Build index.json.
	index := ociIndex{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.index.v1+json",
		Manifests: []ociDescriptor{
			{
				MediaType: "application/vnd.oci.image.manifest.v1+json",
				Digest:    "sha256:" + manifestDigest,
				Size:      int64(len(manifestData)),
			},
		},
	}
	indexData, err := json.Marshal(index)
	if err != nil {
		t.Fatalf("marshal index.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(layoutPath, "index.json"), indexData, 0o644); err != nil {
		t.Fatalf("write index.json: %v", err)
	}

	// Build oci-layout.
	if err := os.WriteFile(filepath.Join(layoutPath, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644); err != nil {
		t.Fatalf("write oci-layout: %v", err)
	}

	info := &LayoutInfo{
		ManifestDigest: "sha256:" + manifestDigest,
		Layers:         layerInfos,
		Config:         &VMImageConfig{Arch: "amd64"},
	}
	return layoutPath, info
}

type testLayerSpec struct {
	mediaType  string
	entries    []testTarEntry
	compressed bool
}

type testTarEntry struct {
	Name    string
	Content []byte
	IsDir   bool
}

func buildTarBlob(t *testing.T, entries []testTarEntry) []byte {
	t.Helper()
	tmpFile := filepath.Join(t.TempDir(), "layer.tar")
	f, err := os.Create(tmpFile)
	if err != nil {
		t.Fatalf("create tar file: %v", err)
	}
	tw := tar.NewWriter(f)
	for _, e := range entries {
		if e.IsDir {
			if err := tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeDir,
				Name:     e.Name,
				Mode:     0o755,
			}); err != nil {
				t.Fatalf("write tar dir header: %v", err)
			}
		} else {
			if err := tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeReg,
				Name:     e.Name,
				Size:     int64(len(e.Content)),
				Mode:     0o644,
			}); err != nil {
				t.Fatalf("write tar file header: %v", err)
			}
			if _, err := tw.Write(e.Content); err != nil {
				t.Fatalf("write tar file content: %v", err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close tar file: %v", err)
	}
	data, err := os.ReadFile(tmpFile)
	if err != nil {
		t.Fatalf("read tar file: %v", err)
	}
	return data
}

func buildGzippedTarBlob(t *testing.T, entries []testTarEntry) []byte {
	t.Helper()
	tmpFile := filepath.Join(t.TempDir(), "layer.tar.gz")
	f, err := os.Create(tmpFile)
	if err != nil {
		t.Fatalf("create gzip file: %v", err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		if e.IsDir {
			if err := tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeDir,
				Name:     e.Name,
				Mode:     0o755,
			}); err != nil {
				t.Fatalf("write tar dir header: %v", err)
			}
		} else {
			if err := tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeReg,
				Name:     e.Name,
				Size:     int64(len(e.Content)),
				Mode:     0o644,
			}); err != nil {
				t.Fatalf("write tar file header: %v", err)
			}
			if _, err := tw.Write(e.Content); err != nil {
				t.Fatalf("write tar file content: %v", err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close gzip file: %v", err)
	}
	data, err := os.ReadFile(tmpFile)
	if err != nil {
		t.Fatalf("read gzip file: %v", err)
	}
	return data
}

func TestMaterializeRootfs_SingleUncompressedLayer(t *testing.T) {
	t.Parallel()

	layoutPath, info := buildTestOCILayout(t, []testLayerSpec{
		{
			mediaType: "application/vnd.oci.image.layer.v1.tar",
			entries: []testTarEntry{
				{Name: "etc/", IsDir: true},
				{Name: "etc/hostname", Content: []byte("testhost\n")},
				{Name: "bin/", IsDir: true},
				{Name: "bin/sh", Content: []byte("#!/bin/sh\n")},
			},
		},
	})

	targetDir := filepath.Join(t.TempDir(), "rootfs")
	if err := MaterializeRootfs(t.Context(), layoutPath, targetDir, info); err != nil {
		t.Fatalf("MaterializeRootfs: %v", err)
	}

	assertFileContent(t, filepath.Join(targetDir, "etc", "hostname"), "testhost\n")
	assertFileContent(t, filepath.Join(targetDir, "bin", "sh"), "#!/bin/sh\n")
}

func TestMaterializeRootfs_CompressedLayer(t *testing.T) {
	t.Parallel()

	layoutPath, info := buildTestOCILayout(t, []testLayerSpec{
		{
			mediaType:  "application/vnd.oci.image.layer.v1.tar+gzip",
			compressed: true,
			entries: []testTarEntry{
				{Name: "etc/", IsDir: true},
				{Name: "etc/os-release", Content: []byte("ID=ubuntu\n")},
			},
		},
	})

	targetDir := filepath.Join(t.TempDir(), "rootfs")
	if err := MaterializeRootfs(t.Context(), layoutPath, targetDir, info); err != nil {
		t.Fatalf("MaterializeRootfs: %v", err)
	}

	assertFileContent(t, filepath.Join(targetDir, "etc", "os-release"), "ID=ubuntu\n")
}

func TestMaterializeRootfs_CompressedLayer_DockerMediaType(t *testing.T) {
	t.Parallel()

	layoutPath, info := buildTestOCILayout(t, []testLayerSpec{
		{
			mediaType:  "application/vnd.docker.image.rootfs.diff.tar.gzip",
			compressed: true,
			entries: []testTarEntry{
				{Name: "etc/", IsDir: true},
				{Name: "etc/issue", Content: []byte("docker-media-type\n")},
			},
		},
	})

	targetDir := filepath.Join(t.TempDir(), "rootfs")
	if err := MaterializeRootfs(t.Context(), layoutPath, targetDir, info); err != nil {
		t.Fatalf("MaterializeRootfs: %v", err)
	}

	assertFileContent(t, filepath.Join(targetDir, "etc", "issue"), "docker-media-type\n")
}

func TestMaterializeRootfs_MultiLayerWithWhiteout(t *testing.T) {
	t.Parallel()

	layoutPath, info := buildTestOCILayout(t, []testLayerSpec{
		// Base layer: creates /etc/old-config and /tmp/data.
		{
			mediaType:  "application/vnd.oci.image.layer.v1.tar+gzip",
			compressed: true,
			entries: []testTarEntry{
				{Name: "etc/", IsDir: true},
				{Name: "etc/old-config", Content: []byte("old\n")},
				{Name: "tmp/", IsDir: true},
				{Name: "tmp/data", Content: []byte("data\n")},
			},
		},
		// Upper layer: whiteout /etc/old-config, add /etc/new-config.
		{
			mediaType:  "application/vnd.oci.image.layer.v1.tar+gzip",
			compressed: true,
			entries: []testTarEntry{
				{Name: "etc/", IsDir: true},
				{Name: "etc/.wh.old-config", Content: nil},
				{Name: "etc/new-config", Content: []byte("new\n")},
			},
		},
	})

	targetDir := filepath.Join(t.TempDir(), "rootfs")
	if err := MaterializeRootfs(t.Context(), layoutPath, targetDir, info); err != nil {
		t.Fatalf("MaterializeRootfs: %v", err)
	}

	// old-config should be deleted by whiteout.
	if _, err := os.Stat(filepath.Join(targetDir, "etc", "old-config")); !os.IsNotExist(err) {
		t.Errorf("etc/old-config should be removed by whiteout, got err=%v", err)
	}
	// new-config should exist.
	assertFileContent(t, filepath.Join(targetDir, "etc", "new-config"), "new\n")
	// tmp/data should survive from base layer.
	assertFileContent(t, filepath.Join(targetDir, "tmp", "data"), "data\n")
}

func TestMaterializeRootfs_OpaqueWhiteout(t *testing.T) {
	t.Parallel()

	layoutPath, info := buildTestOCILayout(t, []testLayerSpec{
		// Base layer: creates /etc/a and /etc/b.
		{
			mediaType: "application/vnd.oci.image.layer.v1.tar",
			entries: []testTarEntry{
				{Name: "etc/", IsDir: true},
				{Name: "etc/a", Content: []byte("a\n")},
				{Name: "etc/b", Content: []byte("b\n")},
			},
		},
		// Upper layer: opaque whiteout on /etc/ (remove all children), add /etc/c.
		{
			mediaType: "application/vnd.oci.image.layer.v1.tar",
			entries: []testTarEntry{
				{Name: "etc/", IsDir: true},
				{Name: "etc/.wh..wh..opq"},
				{Name: "etc/c", Content: []byte("c\n")},
			},
		},
	})

	targetDir := filepath.Join(t.TempDir(), "rootfs")
	if err := MaterializeRootfs(t.Context(), layoutPath, targetDir, info); err != nil {
		t.Fatalf("MaterializeRootfs: %v", err)
	}

	if _, err := os.Stat(filepath.Join(targetDir, "etc", "a")); !os.IsNotExist(err) {
		t.Errorf("etc/a should be removed by opaque whiteout, got err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(targetDir, "etc", "b")); !os.IsNotExist(err) {
		t.Errorf("etc/b should be removed by opaque whiteout, got err=%v", err)
	}
	assertFileContent(t, filepath.Join(targetDir, "etc", "c"), "c\n")
}

func TestMaterializeRootfs_SkipsKernelLayers(t *testing.T) {
	t.Parallel()

	layoutPath, info := buildTestOCILayout(t, []testLayerSpec{
		// Kernel layer (should be skipped).
		{
			mediaType: MediaTypeKernelLayer,
			entries: []testTarEntry{
				{Name: "vmlinuz", Content: []byte("kernel-data")},
			},
		},
		// Rootfs layer (should be materialized).
		{
			mediaType: MediaTypeRootfsLayer,
			entries: []testTarEntry{
				{Name: "etc/", IsDir: true},
				{Name: "etc/hostname", Content: []byte("vm-host\n")},
			},
		},
	})

	targetDir := filepath.Join(t.TempDir(), "rootfs")
	if err := MaterializeRootfs(t.Context(), layoutPath, targetDir, info); err != nil {
		t.Fatalf("MaterializeRootfs: %v", err)
	}

	// Kernel files should NOT be present (layer was skipped).
	if _, err := os.Stat(filepath.Join(targetDir, "vmlinuz")); !os.IsNotExist(err) {
		t.Errorf("vmlinuz should not exist (kernel layer skipped), got err=%v", err)
	}
	assertFileContent(t, filepath.Join(targetDir, "etc", "hostname"), "vm-host\n")
}

func TestMaterializeRootfs_NilLayoutInfo(t *testing.T) {
	t.Parallel()

	err := MaterializeRootfs(t.Context(), t.TempDir(), t.TempDir(), nil)
	if err == nil {
		t.Fatal("expected error for nil layout info, got nil")
	}
}

func TestMaterializeRootfs_CancelledContext(t *testing.T) {
	t.Parallel()

	layoutPath, info := buildTestOCILayout(t, []testLayerSpec{
		{
			mediaType: "application/vnd.oci.image.layer.v1.tar",
			entries: []testTarEntry{
				{Name: "foo", Content: []byte("bar")},
			},
		},
	})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := MaterializeRootfs(ctx, layoutPath, t.TempDir(), info)
	if err == nil {
		t.Fatal("expected context cancelled error, got nil")
	}
}

func assertFileContent(t *testing.T, path, expected string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(data) != expected {
		t.Errorf("%s content = %q, want %q", path, string(data), expected)
	}
}

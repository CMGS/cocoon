package engine

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/oci"
)

func TestNormalizeVirtiofsKernelCmdline_EnforcesSerialAndVirtioConsoles(t *testing.T) {
	t.Parallel()

	got := normalizeVirtiofsKernelCmdline("quiet splash", "/dev/root")
	want := "quiet splash console=ttyS0 console=hvc0 root=/dev/root rootfstype=virtiofs rw"
	if got != want {
		t.Fatalf("normalizeVirtiofsKernelCmdline() = %q, want %q", got, want)
	}
}

func TestNormalizeVirtiofsKernelCmdline_RewritesRootAndConsoleArgs(t *testing.T) {
	t.Parallel()

	got := normalizeVirtiofsKernelCmdline("foo=bar console=ttyS1 root=/dev/vda1 ro console=hvc0", "/dev/root")
	want := "foo=bar console=ttyS0 console=hvc0 root=/dev/root rootfstype=virtiofs rw"
	if got != want {
		t.Fatalf("normalizeVirtiofsKernelCmdline() = %q, want %q", got, want)
	}
}

func TestMaterializeOCIRuntimeCache_BadInitramfsNotPromoted(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultConfig()
	cfg.RebaseRootDir(t.TempDir())
	cfg.RuntimeDir = t.TempDir()
	cfg.LogDir = t.TempDir()
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}

	layoutPath := t.TempDir()
	blobDir := filepath.Join(layoutPath, "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatalf("mkdir blob dir: %v", err)
	}

	const (
		runtimeKey  = "badinitrdruntimekey"
		kernelHex   = "1111111111111111111111111111111111111111111111111111111111111111"
		rootfsHex   = "2222222222222222222222222222222222222222222222222222222222222222"
		manifestHex = "3333333333333333333333333333333333333333333333333333333333333333"
	)

	rootfsTar := buildLayerTarForTest(t, map[string][]byte{
		"etc/os-release": []byte("NAME=cocoon\n"),
	})
	if err := os.WriteFile(filepath.Join(blobDir, rootfsHex), rootfsTar, 0o644); err != nil {
		t.Fatalf("write rootfs blob: %v", err)
	}

	badInitrd := []byte{0xde, 0xad, 0xbe, 0xef, 0x00, 0x01}
	kernelTar := buildLayerTarForTest(t, map[string][]byte{
		"vmlinuz":    bytes.Repeat([]byte("K"), 2048),
		"initrd.img": badInitrd,
	})
	if err := os.WriteFile(filepath.Join(blobDir, kernelHex), kernelTar, 0o644); err != nil {
		t.Fatalf("write kernel blob: %v", err)
	}

	info := &oci.LayoutInfo{
		ManifestDigest: "sha256:" + manifestHex,
		Layers: []oci.LayerInfo{
			{MediaType: oci.MediaTypeKernelLayer, Digest: "sha256:" + kernelHex},
			{MediaType: oci.MediaTypeRootfsLayer, Digest: "sha256:" + rootfsHex},
		},
		Config: &oci.VMImageConfig{
			KernelPath: "/vmlinuz",
			InitrdPath: "/initrd.img",
		},
	}

	err := materializeOCIRuntimeCache(t.Context(), cfg, runtimeKey, layoutPath, info)
	if err == nil {
		t.Fatal("expected materializeOCIRuntimeCache error, got nil")
	}
	if !strings.Contains(err.Error(), "virtiofs module detection failed") {
		t.Fatalf("unexpected error: %v", err)
	}

	// Entry metadata should NOT be written when kernel layer extraction fails.
	if _, statErr := os.Stat(cfg.OCIRuntimeEntryMetaPath(runtimeKey)); !os.IsNotExist(statErr) {
		t.Fatalf("runtime entry meta should not exist, stat err=%v", statErr)
	}

	// Kernel layer dir should NOT be promoted (failed validation).
	if _, statErr := os.Stat(cfg.OCIRuntimeLayerDir(kernelHex)); !os.IsNotExist(statErr) {
		t.Fatalf("kernel layer cache should not exist, stat err=%v", statErr)
	}

	// Leftover work dirs should be cleaned up.
	workDirs, globErr := filepath.Glob(filepath.Join(cfg.OCIRuntimeLayersDir(), kernelHex+"-work-*"))
	if globErr != nil {
		t.Fatalf("glob work dirs: %v", globErr)
	}
	if len(workDirs) != 0 {
		t.Fatalf("expected no leftover work dirs, got %v", workDirs)
	}
}

func TestExtractLayerToCache_Idempotent(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultConfig()
	cfg.RebaseRootDir(t.TempDir())
	cfg.RuntimeDir = t.TempDir()
	cfg.LogDir = t.TempDir()
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}

	layoutPath := t.TempDir()
	blobDir := filepath.Join(layoutPath, "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatalf("mkdir blob dir: %v", err)
	}

	const rootfsHex = "4444444444444444444444444444444444444444444444444444444444444444"

	rootfsTar := buildLayerTarForTest(t, map[string][]byte{
		"etc/hostname": []byte("cocoon-test\n"),
	})
	if err := os.WriteFile(filepath.Join(blobDir, rootfsHex), rootfsTar, 0o644); err != nil {
		t.Fatalf("write rootfs blob: %v", err)
	}

	layer := oci.LayerInfo{
		MediaType: oci.MediaTypeRootfsLayer,
		Digest:    "sha256:" + rootfsHex,
	}

	// First extraction.
	if err := extractLayerToCache(t.Context(), cfg, rootfsHex, layoutPath, layer, false, nil); err != nil {
		t.Fatalf("first extraction: %v", err)
	}
	layerDir := cfg.OCIRuntimeLayerDir(rootfsHex)
	if _, err := os.Stat(filepath.Join(layerDir, "rootfs", "etc", "hostname")); err != nil {
		t.Fatalf("expected extracted file: %v", err)
	}

	// Second extraction should be a no-op.
	if err := extractLayerToCache(t.Context(), cfg, rootfsHex, layoutPath, layer, false, nil); err != nil {
		t.Fatalf("second extraction (idempotent): %v", err)
	}
}

func buildLayerTarForTest(t *testing.T, files map[string][]byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for path, data := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: path,
			Mode: 0o644,
			Size: int64(len(data)),
		}); err != nil {
			t.Fatalf("write tar header for %s: %v", path, err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("write tar payload for %s: %v", path, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	return buf.Bytes()
}

package oci

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBuildContextReadWrite(t *testing.T) {
	t.Parallel()

	imagePath := filepath.Join(t.TempDir(), "base.qcow2")
	if err := os.WriteFile(imagePath, []byte("fake"), 0o644); err != nil {
		t.Fatalf("write image: %v", err)
	}

	want := &BuildContext{
		SourceType:     BuildSourceLocalOCITag,
		BaseTag:        "cmgs/base:latest",
		BaseLayoutPath: "/tmp/layout",
		BaseConfig: VMImageConfig{
			Arch:           "amd64",
			KernelPath:     "/vmlinuz",
			InitrdPath:     "/initrd.img",
			KernelCmdline:  "console=hvc0",
			VirtiofsTag:    "cocoon-rootfs",
			RootfsPartUUID: "18E34A04-F507-4BCB-B834-2C1BF1FD48F2",
		},
		KernelLayer:   LayerInfo{MediaType: MediaTypeKernelLayer, Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Size: 11},
		RootfsLayers:  []LayerInfo{{MediaType: MediaTypeRootfsLayer, Digest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Size: 22}},
		BaseRootfsDir: "/tmp/rootfs",
	}

	if err := WriteBuildContextForImage(imagePath, want); err != nil {
		t.Fatalf("WriteBuildContextForImage: %v", err)
	}
	got, err := ReadBuildContextForImage(imagePath)
	if err != nil {
		t.Fatalf("ReadBuildContextForImage: %v", err)
	}
	if got == nil {
		t.Fatal("ReadBuildContextForImage returned nil context")
	}
	if got.SourceType != want.SourceType {
		t.Fatalf("SourceType=%q, want %q", got.SourceType, want.SourceType)
	}
	if got.BaseTag != want.BaseTag {
		t.Fatalf("BaseTag=%q, want %q", got.BaseTag, want.BaseTag)
	}
	if got.BaseLayoutPath != want.BaseLayoutPath {
		t.Fatalf("BaseLayoutPath=%q, want %q", got.BaseLayoutPath, want.BaseLayoutPath)
	}
	if got.KernelLayer.Digest != want.KernelLayer.Digest {
		t.Fatalf("KernelLayer.Digest=%q, want %q", got.KernelLayer.Digest, want.KernelLayer.Digest)
	}
	if len(got.RootfsLayers) != 1 || got.RootfsLayers[0].Digest != want.RootfsLayers[0].Digest {
		t.Fatalf("RootfsLayers=%v, want %v", got.RootfsLayers, want.RootfsLayers)
	}
}

func TestReadBuildContextForImage_NotFound(t *testing.T) {
	t.Parallel()

	imagePath := filepath.Join(t.TempDir(), "base.qcow2")
	if err := os.WriteFile(imagePath, []byte("fake"), 0o644); err != nil {
		t.Fatalf("write image: %v", err)
	}

	got, err := ReadBuildContextForImage(imagePath)
	if err != nil {
		t.Fatalf("ReadBuildContextForImage: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil context for missing sidecar, got %+v", got)
	}
}

package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/image/refcache"
	"github.com/CMGS/cocoon/oci"
	"github.com/CMGS/cocoon/types"
)

func testResolverConfig(t *testing.T) *config.CocoonConfig {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.RebaseRootDir(t.TempDir())
	cfg.RuntimeDir = t.TempDir()
	cfg.LogDir = t.TempDir()
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}
	return cfg
}

func TestResolveRuntimeImageRef_LocalPath(t *testing.T) {
	t.Parallel()
	cfg := testResolverConfig(t)

	imagePath := filepath.Join(t.TempDir(), "base.img")
	if err := os.WriteFile(imagePath, []byte("img"), 0o644); err != nil {
		t.Fatalf("write local image: %v", err)
	}

	got, err := resolveRuntimeImageRef(cfg, imagePath)
	if err != nil {
		t.Fatalf("resolveRuntimeImageRef: %v", err)
	}
	if got.Source != runtimeImageSourceLocalPath {
		t.Fatalf("Source = %q, want %q", got.Source, runtimeImageSourceLocalPath)
	}
	if got.VMImageType != types.VMImageTypeQCOW2 {
		t.Fatalf("VMImageType = %q, want %q", got.VMImageType, types.VMImageTypeQCOW2)
	}
	if got.PrepareRef != imagePath {
		t.Fatalf("PrepareRef = %q, want %q", got.PrepareRef, imagePath)
	}
}

func TestResolveRuntimeImageRef_MissingExplicitLocalPath(t *testing.T) {
	t.Parallel()
	cfg := testResolverConfig(t)

	_, err := resolveRuntimeImageRef(cfg, "./missing.img")
	if err == nil {
		t.Fatal("expected error for missing explicit local path, got nil")
	}
	if !strings.Contains(err.Error(), "local image path") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveRuntimeImageRef_LocalOCITagImplicitLatest(t *testing.T) {
	t.Parallel()
	cfg := testResolverConfig(t)

	store := oci.NewStore(cfg)
	if err := store.SaveTag("demo:latest", filepath.Join(t.TempDir(), "layout"), "sha256:1111"); err != nil {
		t.Fatalf("SaveTag: %v", err)
	}

	got, err := resolveRuntimeImageRef(cfg, "demo")
	if err != nil {
		t.Fatalf("resolveRuntimeImageRef: %v", err)
	}
	if got.Source != runtimeImageSourceLocalOCITag {
		t.Fatalf("Source = %q, want %q", got.Source, runtimeImageSourceLocalOCITag)
	}
	if got.LocalOCITag != "demo:latest" {
		t.Fatalf("LocalOCITag = %q, want %q", got.LocalOCITag, "demo:latest")
	}
	if got.VMImageType != types.VMImageTypeOCIVM {
		t.Fatalf("VMImageType = %q, want %q", got.VMImageType, types.VMImageTypeOCIVM)
	}
}

func TestResolveRuntimeImageRef_AmbiguousOCITagAndCacheAlias(t *testing.T) {
	t.Parallel()
	cfg := testResolverConfig(t)

	store := oci.NewStore(cfg)
	if err := store.SaveTag("demo:latest", filepath.Join(t.TempDir(), "layout"), "sha256:1111"); err != nil {
		t.Fatalf("SaveTag: %v", err)
	}
	if err := refcache.Upsert(cfg, "demo", "0123456789abcdef_amd64", strings.Repeat("a", 64)); err != nil {
		t.Fatalf("refcache.Upsert: %v", err)
	}

	_, err := resolveRuntimeImageRef(cfg, "demo")
	if err == nil {
		t.Fatal("expected ambiguity error, got nil")
	}
	if !strings.Contains(err.Error(), "ambiguous image reference") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveRuntimeImageRef_LocalCacheAlias(t *testing.T) {
	t.Parallel()
	cfg := testResolverConfig(t)

	if err := refcache.Upsert(cfg, "ubuntu-24.04", "fedcba9876543210_amd64", strings.Repeat("b", 64)); err != nil {
		t.Fatalf("refcache.Upsert: %v", err)
	}

	got, err := resolveRuntimeImageRef(cfg, "ubuntu-24.04")
	if err != nil {
		t.Fatalf("resolveRuntimeImageRef: %v", err)
	}
	if got.Source != runtimeImageSourceLocalCache {
		t.Fatalf("Source = %q, want %q", got.Source, runtimeImageSourceLocalCache)
	}
	if got.LocalBaseKey != "fedcba9876543210_amd64" {
		t.Fatalf("LocalBaseKey = %q, want %q", got.LocalBaseKey, "fedcba9876543210_amd64")
	}
}

func TestResolveRuntimeImageRef_URLAndRegistry(t *testing.T) {
	t.Parallel()
	cfg := testResolverConfig(t)

	urlResolved, err := resolveRuntimeImageRef(cfg, "https://cloud-images.ubuntu.com/jammy/current/jammy-server-cloudimg-amd64.img")
	if err != nil {
		t.Fatalf("resolveRuntimeImageRef(url): %v", err)
	}
	if urlResolved.Source != runtimeImageSourceURL {
		t.Fatalf("URL source = %q, want %q", urlResolved.Source, runtimeImageSourceURL)
	}

	regResolved, err := resolveRuntimeImageRef(cfg, "docker.io/library/ubuntu:24.04")
	if err != nil {
		t.Fatalf("resolveRuntimeImageRef(registry): %v", err)
	}
	if regResolved.Source != runtimeImageSourceRegistry {
		t.Fatalf("registry source = %q, want %q", regResolved.Source, runtimeImageSourceRegistry)
	}
}

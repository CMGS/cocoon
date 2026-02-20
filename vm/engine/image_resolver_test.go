package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/image/refcache"
	"github.com/CMGS/cocoon/oci"
	"github.com/CMGS/cocoon/types"
)

func resolverProbeStub(_ context.Context, _ *config.CocoonConfig, ref string) (types.VMImageType, error) {
	switch {
	case strings.Contains(ref, "probe-error"):
		return types.VMImageTypeQCOW2, errors.New("network timeout")
	case strings.Contains(ref, "cocoon-vm"):
		return types.VMImageTypeOCIVM, nil
	}
	return types.VMImageTypeQCOW2, nil
}

func resolveRuntimeImageRefForTest(ctx context.Context, cfg *config.CocoonConfig, ref string) (*resolvedRuntimeImage, error) {
	return resolveRuntimeImageRefWithProbe(ctx, cfg, ref, resolverProbeStub)
}

func writeRefcacheIndex(t *testing.T, cfg *config.CocoonConfig, entries map[string]map[string]any) {
	t.Helper()
	path := filepath.Join(cfg.ManifestCacheDir(), "index.json")
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("marshal refcache index: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write refcache index: %v", err)
	}
}

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

	got, err := resolveRuntimeImageRefForTest(t.Context(), cfg, imagePath)
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

	_, err := resolveRuntimeImageRefForTest(t.Context(), cfg, "./missing.img")
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

	got, err := resolveRuntimeImageRefForTest(t.Context(), cfg, "demo")
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
	writeRefcacheIndex(t, cfg, map[string]map[string]any{
		"demo": {
			"base_key":     "0123456789abcdef_amd64",
			"digest_full":  strings.Repeat("a", 64),
			"last_seen_at": "2026-02-18T00:00:00Z",
		},
		"demo:latest": {
			"base_key":     "fedcba9876543210_amd64",
			"digest_full":  strings.Repeat("b", 64),
			"last_seen_at": "2026-02-18T00:00:00Z",
		},
	})

	_, err := resolveRuntimeImageRefForTest(t.Context(), cfg, "demo")
	if err == nil {
		t.Fatal("expected ambiguity error, got nil")
	}
	if !strings.Contains(err.Error(), "ambiguous image reference") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveRuntimeImageRef_PrefersLocalOCITagWhenIdentitySame(t *testing.T) {
	t.Parallel()
	cfg := testResolverConfig(t)

	store := oci.NewStore(cfg)
	if err := store.SaveTag("demo:latest", filepath.Join(t.TempDir(), "layout"), "sha256:1111"); err != nil {
		t.Fatalf("SaveTag: %v", err)
	}
	writeRefcacheIndex(t, cfg, map[string]map[string]any{
		"demo": {
			"base_key":     "fedcba9876543210_amd64",
			"digest_full":  strings.Repeat("b", 64),
			"last_seen_at": "2026-02-18T00:00:00Z",
		},
		"demo:latest": {
			"base_key":     "fedcba9876543210_amd64",
			"digest_full":  strings.Repeat("b", 64),
			"last_seen_at": "2026-02-18T00:00:00Z",
		},
	})

	got, err := resolveRuntimeImageRefForTest(t.Context(), cfg, "demo")
	if err != nil {
		t.Fatalf("resolveRuntimeImageRef: %v", err)
	}
	if got.Source != runtimeImageSourceLocalOCITag {
		t.Fatalf("Source = %q, want %q", got.Source, runtimeImageSourceLocalOCITag)
	}
}

func TestResolveRuntimeImageRef_LocalCacheAlias(t *testing.T) {
	t.Parallel()
	cfg := testResolverConfig(t)

	if err := refcache.Upsert(cfg, "ubuntu-24.04", "fedcba9876543210_amd64", strings.Repeat("b", 64)); err != nil {
		t.Fatalf("refcache.Upsert: %v", err)
	}

	got, err := resolveRuntimeImageRefForTest(t.Context(), cfg, "ubuntu-24.04")
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
	cfg := testResolverConfig(t)

	urlResolved, err := resolveRuntimeImageRefForTest(t.Context(), cfg, "https://cloud-images.ubuntu.com/jammy/current/jammy-server-cloudimg-amd64.img")
	if err != nil {
		t.Fatalf("resolveRuntimeImageRef(url): %v", err)
	}
	if urlResolved.Source != runtimeImageSourceURL {
		t.Fatalf("URL source = %q, want %q", urlResolved.Source, runtimeImageSourceURL)
	}

	regResolved, err := resolveRuntimeImageRefWithProbe(t.Context(), cfg, "docker.io/library/ubuntu:24.04", resolverProbeStub)
	if err != nil {
		t.Fatalf("resolveRuntimeImageRef(registry): %v", err)
	}
	if regResolved.Source != runtimeImageSourceRegistry {
		t.Fatalf("registry source = %q, want %q", regResolved.Source, runtimeImageSourceRegistry)
	}
	if regResolved.VMImageType != types.VMImageTypeQCOW2 {
		t.Fatalf("registry VMImageType = %q, want %q", regResolved.VMImageType, types.VMImageTypeQCOW2)
	}
}

func TestResolveRuntimeImageRef_RegistryProbeFailureReturnsError(t *testing.T) {
	cfg := testResolverConfig(t)

	_, err := resolveRuntimeImageRefWithProbe(t.Context(), cfg, "registry.example.com/ns/probe-error:latest", resolverProbeStub)
	if err == nil {
		t.Fatal("expected registry probe error, got nil")
	}
	if !strings.Contains(err.Error(), "probe registry image type") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveRuntimeImageRef_RegistryCocoonVMMediaTypes(t *testing.T) {
	cfg := testResolverConfig(t)

	resolved, err := resolveRuntimeImageRefWithProbe(t.Context(), cfg, "registry.example.com/ns/cocoon-vm:latest", resolverProbeStub)
	if err != nil {
		t.Fatalf("resolveRuntimeImageRef: %v", err)
	}
	if resolved.Source != runtimeImageSourceRegistry {
		t.Fatalf("Source = %q, want %q", resolved.Source, runtimeImageSourceRegistry)
	}
	if resolved.VMImageType != types.VMImageTypeOCIVM {
		t.Fatalf("VMImageType = %q, want %q", resolved.VMImageType, types.VMImageTypeOCIVM)
	}
}

func TestResolveRuntimeImageRef_BareFilenameResolvedAsLocal(t *testing.T) {
	cfg := testResolverConfig(t)

	tmpDir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if chdirErr := os.Chdir(tmpDir); chdirErr != nil {
		t.Fatalf("chdir temp dir: %v", chdirErr)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldWD)
	})

	// Create a bare filename in CWD with no OCI tag or cache alias match.
	if err := os.WriteFile(filepath.Join(tmpDir, "ubuntu.qcow2"), []byte("img"), 0o644); err != nil {
		t.Fatalf("write local image: %v", err)
	}

	got, err := resolveRuntimeImageRefForTest(t.Context(), cfg, "ubuntu.qcow2")
	if err != nil {
		t.Fatalf("resolveRuntimeImageRef: %v", err)
	}
	if got.Source != runtimeImageSourceLocalPath {
		t.Fatalf("Source = %q, want %q", got.Source, runtimeImageSourceLocalPath)
	}
	if got.VMImageType != types.VMImageTypeQCOW2 {
		t.Fatalf("VMImageType = %q, want %q", got.VMImageType, types.VMImageTypeQCOW2)
	}
	// On macOS, /var is a symlink to /private/var. resolveLocalPathRef uses
	// filepath.Abs which may resolve differently, so compare real paths.
	wantPath, _ := filepath.EvalSymlinks(filepath.Join(tmpDir, "ubuntu.qcow2"))
	gotPath, _ := filepath.EvalSymlinks(got.PrepareRef)
	if gotPath != wantPath {
		t.Fatalf("PrepareRef = %q, want %q", got.PrepareRef, wantPath)
	}
}

func TestResolveRuntimeImageRef_NonExplicitLocalFileDoesNotShadowOCITag(t *testing.T) {
	cfg := testResolverConfig(t)
	store := oci.NewStore(cfg)
	if err := store.SaveTag("demo:latest", filepath.Join(t.TempDir(), "layout"), "sha256:1111"); err != nil {
		t.Fatalf("SaveTag: %v", err)
	}

	tmpDir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if chdirErr := os.Chdir(tmpDir); chdirErr != nil {
		t.Fatalf("chdir temp dir: %v", chdirErr)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldWD)
	})

	if err := os.WriteFile(filepath.Join(tmpDir, "demo"), []byte("dummy"), 0o644); err != nil {
		t.Fatalf("write demo local file: %v", err)
	}

	resolved, err := resolveRuntimeImageRefForTest(t.Context(), cfg, "demo")
	if err != nil {
		t.Fatalf("resolveRuntimeImageRef: %v", err)
	}
	if resolved.Source != runtimeImageSourceLocalOCITag {
		t.Fatalf("Source = %q, want %q", resolved.Source, runtimeImageSourceLocalOCITag)
	}
}

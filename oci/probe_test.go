package oci

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/types"
)

func testProbeConfig(t *testing.T) *config.CocoonConfig {
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

func buildManifestJSON(configMediaType, artifactType string, layers []ociDescriptor) []byte {
	m := ociManifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.manifest.v1+json",
		ArtifactType:  artifactType,
		Config: ociDescriptor{
			MediaType: configMediaType,
			Digest:    "sha256:" + strings.Repeat("a", 64),
			Size:      100,
		},
		Layers: layers,
	}
	data, _ := json.Marshal(m)
	return data
}

func buildConfigJSON() []byte {
	c := map[string]any{
		"architecture": "amd64",
		"os":           "linux",
	}
	data, _ := json.Marshal(c)
	return data
}

// testRegistryHandler returns an http.Handler that serves OCI manifests.
// The manifestByRef map keys are "repo:tag" and values are raw manifest JSON.
func testRegistryHandler(manifestByRef map[string][]byte, configJSON []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// /v2/ ping (exact match only)
		if path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}

		if !strings.HasPrefix(path, "/v2/") {
			http.NotFound(w, r)
			return
		}
		trimmed := strings.TrimPrefix(path, "/v2/")

		// /v2/{name}/manifests/{reference}
		if idx := strings.Index(trimmed, "/manifests/"); idx >= 0 {
			repo := trimmed[:idx]
			ref := trimmed[idx+len("/manifests/"):]
			key := repo + ":" + ref
			manifest, ok := manifestByRef[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprintf(w, `{"errors":[{"code":"MANIFEST_UNKNOWN","message":"not found"}]}`)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("b", 64))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(manifest)
			return
		}

		// /v2/{name}/blobs/{digest}
		if idx := strings.Index(trimmed, "/blobs/"); idx >= 0 {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(configJSON)
			return
		}

		http.NotFound(w, r)
	})
}

func TestProbeRegistryVMImageType_CocoonVM(t *testing.T) {
	t.Parallel()
	cfg := testProbeConfig(t)

	manifest := buildManifestJSON(
		MediaTypeVMConfig,
		ArtifactTypeVMImage,
		[]ociDescriptor{
			{MediaType: MediaTypeKernelLayer, Digest: "sha256:" + strings.Repeat("c", 64), Size: 50},
			{MediaType: MediaTypeRootfsLayer, Digest: "sha256:" + strings.Repeat("d", 64), Size: 200},
		},
	)
	configData := buildConfigJSON()

	srv := httptest.NewServer(testRegistryHandler(map[string][]byte{
		"test/cocoon-vm:latest": manifest,
	}, configData))
	defer srv.Close()

	// Strip scheme for registry ref.
	host := strings.TrimPrefix(srv.URL, "http://")
	ref := host + "/test/cocoon-vm:latest"

	vmType, err := ProbeRegistryVMImageType(t.Context(), cfg, ref)
	if err != nil {
		t.Fatalf("ProbeRegistryVMImageType: %v", err)
	}
	if vmType != types.VMImageTypeOCIVM {
		t.Fatalf("vmType = %q, want %q", vmType, types.VMImageTypeOCIVM)
	}
}

func TestProbeRegistryVMImageType_StandardContainer(t *testing.T) {
	t.Parallel()
	cfg := testProbeConfig(t)

	manifest := buildManifestJSON(
		"application/vnd.oci.image.config.v1+json",
		"",
		[]ociDescriptor{
			{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Digest: "sha256:" + strings.Repeat("e", 64), Size: 100},
		},
	)
	configData := buildConfigJSON()

	srv := httptest.NewServer(testRegistryHandler(map[string][]byte{
		"test/standard:latest": manifest,
	}, configData))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	ref := host + "/test/standard:latest"

	vmType, err := ProbeRegistryVMImageType(t.Context(), cfg, ref)
	if err != nil {
		t.Fatalf("ProbeRegistryVMImageType: %v", err)
	}
	if vmType != types.VMImageTypeQCOW2 {
		t.Fatalf("vmType = %q, want %q", vmType, types.VMImageTypeQCOW2)
	}
}

func TestProbeRegistryVMImageType_NotFound(t *testing.T) {
	t.Parallel()
	cfg := testProbeConfig(t)

	configData := buildConfigJSON()
	srv := httptest.NewServer(testRegistryHandler(map[string][]byte{}, configData))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	ref := host + "/test/missing:latest"

	vmType, err := ProbeRegistryVMImageType(t.Context(), cfg, ref)
	if err == nil {
		t.Fatal("expected error for 404, got nil")
	}
	if vmType != types.VMImageTypeQCOW2 {
		t.Fatalf("vmType = %q, want %q on error", vmType, types.VMImageTypeQCOW2)
	}
}

func TestProbeRegistryVMImageType_Unauthorized(t *testing.T) {
	t.Parallel()
	cfg := testProbeConfig(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprintf(w, `{"errors":[{"code":"UNAUTHORIZED","message":"unauthorized"}]}`)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, `{"errors":[{"code":"UNAUTHORIZED","message":"unauthorized"}]}`)
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	ref := host + "/test/private:latest"

	_, err := ProbeRegistryVMImageType(t.Context(), cfg, ref)
	if err == nil {
		t.Fatal("expected error for 401, got nil")
	}
	var ce *types.ClassifiedError
	if !errors.As(err, &ce) || ce.Category != types.ErrorCategoryPermanent {
		t.Fatalf("expected permanent error for 401, got: %v", err)
	}
}

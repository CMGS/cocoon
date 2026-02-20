package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/CMGS/cocoon/image"
)

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal json: %v", err)
	}
	return data
}

func digestHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func testIdentifyRegistryServer(t *testing.T, index []byte, manifests map[string][]byte) *httptest.Server {
	t.Helper()

	indexDigest := "sha256:" + digestHex(index)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}

		const repoPrefix = "/v2/test/app/"
		if !strings.HasPrefix(r.URL.Path, repoPrefix) {
			http.NotFound(w, r)
			return
		}

		rest := strings.TrimPrefix(r.URL.Path, repoPrefix)
		if strings.HasPrefix(rest, "manifests/") {
			ref := strings.TrimPrefix(rest, "manifests/")
			payload := []byte(nil)
			mediaType := ""
			contentDigest := ""

			switch ref {
			case "latest":
				payload = index
				mediaType = "application/vnd.oci.image.index.v1+json"
				contentDigest = indexDigest
			default:
				manifest, ok := manifests[ref]
				if !ok {
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"errors":[{"code":"MANIFEST_UNKNOWN","message":"not found"}]}`))
					return
				}
				payload = manifest
				mediaType = "application/vnd.oci.image.manifest.v1+json"
				contentDigest = ref
			}

			w.Header().Set("Content-Type", mediaType)
			w.Header().Set("Docker-Content-Digest", contentDigest)
			w.WriteHeader(http.StatusOK)
			if r.Method != http.MethodHead {
				_, _ = w.Write(payload)
			}
			return
		}

		if strings.HasPrefix(rest, "blobs/") {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
			if r.Method != http.MethodHead {
				_, _ = w.Write([]byte("{}"))
			}
			return
		}

		http.NotFound(w, r)
	})

	return httptest.NewServer(handler)
}

func TestIdentifyOCIRemote_UsesPlatformManifestFromIndex(t *testing.T) {
	t.Parallel()

	arch := defaultArch()
	if arch != "amd64" && arch != "arm64" {
		t.Skipf("unsupported test arch %q", arch)
	}

	amdConfig := "sha256:" + strings.Repeat("a", 64)
	amdLayers := []string{
		"sha256:" + strings.Repeat("b", 64),
		"sha256:" + strings.Repeat("c", 64),
	}
	armConfig := "sha256:" + strings.Repeat("d", 64)
	armLayers := []string{
		"sha256:" + strings.Repeat("e", 64),
		"sha256:" + strings.Repeat("f", 64),
	}

	buildManifest := func(configDigest string, layerDigests []string) []byte {
		layers := make([]map[string]any, 0, len(layerDigests))
		for _, digest := range layerDigests {
			layers = append(layers, map[string]any{
				"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
				"digest":    digest,
				"size":      123,
			})
		}
		return mustJSON(t, map[string]any{
			"schemaVersion": 2,
			"mediaType":     "application/vnd.oci.image.manifest.v1+json",
			"config": map[string]any{
				"mediaType": "application/vnd.oci.image.config.v1+json",
				"digest":    configDigest,
				"size":      456,
			},
			"layers": layers,
		})
	}

	amdManifest := buildManifest(amdConfig, amdLayers)
	armManifest := buildManifest(armConfig, armLayers)
	amdManifestDigest := "sha256:" + digestHex(amdManifest)
	armManifestDigest := "sha256:" + digestHex(armManifest)

	index := mustJSON(t, map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests": []map[string]any{
			{
				"mediaType": "application/vnd.oci.image.manifest.v1+json",
				"digest":    amdManifestDigest,
				"size":      len(amdManifest),
				"platform": map[string]any{
					"os":           "linux",
					"architecture": "amd64",
				},
			},
			{
				"mediaType": "application/vnd.oci.image.manifest.v1+json",
				"digest":    armManifestDigest,
				"size":      len(armManifest),
				"platform": map[string]any{
					"os":           "linux",
					"architecture": "arm64",
				},
			},
		},
	})

	srv := testIdentifyRegistryServer(t, index, map[string][]byte{
		amdManifestDigest: amdManifest,
		armManifestDigest: armManifest,
	})
	defer srv.Close()

	ref := strings.TrimPrefix(srv.URL, "http://") + "/test/app:latest"
	identity, err := identifyOCIRemote(t.Context(), ref)
	if err != nil {
		t.Fatalf("identifyOCIRemote: %v", err)
	}

	expectedConfig := amdConfig
	expectedLayers := amdLayers
	expectedManifestDigestHex := strings.TrimPrefix(amdManifestDigest, "sha256:")
	if arch == "arm64" {
		expectedConfig = armConfig
		expectedLayers = armLayers
		expectedManifestDigestHex = strings.TrimPrefix(armManifestDigest, "sha256:")
	}
	expectedFullDigest, expectedChecksum := computeOCIChecksum(expectedConfig, expectedLayers, arch)

	if identity.Arch != arch {
		t.Fatalf("Arch = %q, want %q", identity.Arch, arch)
	}
	if identity.ManifestDigest != expectedManifestDigestHex {
		t.Fatalf("ManifestDigest = %q, want %q", identity.ManifestDigest, expectedManifestDigestHex)
	}
	if identity.FullDigest != expectedFullDigest {
		t.Fatalf("FullDigest = %q, want %q", identity.FullDigest, expectedFullDigest)
	}
	if identity.Checksum != expectedChecksum {
		t.Fatalf("Checksum = %q, want %q", identity.Checksum, expectedChecksum)
	}
	if identity.SourceRef != ref {
		t.Fatalf("SourceRef = %q, want %q", identity.SourceRef, ref)
	}
	if identity.ImageType != image.ImageTypeOCI {
		t.Fatalf("ImageType = %q, want %q", identity.ImageType, image.ImageTypeOCI)
	}
}

func TestIdentifyOCIRemote_ManifestDigestMatchesImageDigest(t *testing.T) {
	t.Parallel()

	// The selected platform manifest digest must be the value used for TOCTOU
	// verification in pullAndMaterializeOCI, not the index digest.
	arch := defaultArch()
	if arch != "amd64" && arch != "arm64" {
		t.Skipf("unsupported test arch %q", arch)
	}

	manifest := mustJSON(t, map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{
			"mediaType": "application/vnd.oci.image.config.v1+json",
			"digest":    "sha256:" + strings.Repeat("1", 64),
			"size":      123,
		},
		"layers": []map[string]any{
			{
				"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
				"digest":    "sha256:" + strings.Repeat("2", 64),
				"size":      456,
			},
		},
	})
	manifestDigest := "sha256:" + digestHex(manifest)
	index := mustJSON(t, map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests": []map[string]any{
			{
				"mediaType": "application/vnd.oci.image.manifest.v1+json",
				"digest":    manifestDigest,
				"size":      len(manifest),
				"platform": map[string]any{
					"os":           "linux",
					"architecture": arch,
				},
			},
		},
	})

	srv := testIdentifyRegistryServer(t, index, map[string][]byte{
		manifestDigest: manifest,
	})
	defer srv.Close()

	ref := fmt.Sprintf("%s/test/app:latest", strings.TrimPrefix(srv.URL, "http://"))
	identity, err := identifyOCIRemote(t.Context(), ref)
	if err != nil {
		t.Fatalf("identifyOCIRemote: %v", err)
	}
	if identity.ManifestDigest != strings.TrimPrefix(manifestDigest, "sha256:") {
		t.Fatalf("ManifestDigest = %q, want %q", identity.ManifestDigest, strings.TrimPrefix(manifestDigest, "sha256:"))
	}
}

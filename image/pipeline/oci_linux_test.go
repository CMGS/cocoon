//go:build linux

package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/image"
	"github.com/CMGS/cocoon/types"
)

func TestPullAndMountOCIPlatformFallbackUsesStableRefForFrom(t *testing.T) {
	tmp := t.TempDir()
	fakeBinDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(fakeBinDir, 0o755); err != nil {
		t.Fatalf("MkdirAll fake bin: %v", err)
	}

	const ref = "example.com/repo:tag"
	rawManifest := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:1111111111111111111111111111111111111111111111111111111111111111"},"layers":[{"digest":"sha256:2222222222222222222222222222222222222222222222222222222222222222"}]}`
	manifestHash := sha256.Sum256([]byte(rawManifest))
	manifest := hex.EncodeToString(manifestHash[:])
	const pulledImageID = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	fakeBuildah := `#!/usr/bin/env bash
set -euo pipefail
if [ "$1" != "--root" ]; then
  echo "missing --root" >&2
  exit 1
fi
cmd="$3"
case "$cmd" in
  pull)
    if [ "${4:-}" != "--quiet" ]; then
      echo "pull missing --quiet" >&2
      exit 1
    fi
    target="${5:-}"
    if [ "$target" = "` + ref + `@sha256:` + manifest + `" ]; then
      echo "digest pull unsupported" >&2
      exit 1
    fi
    if [ "$target" = "` + ref + `" ]; then
      echo "` + pulledImageID + `"
      exit 0
    fi
    ;;
  from)
    target="${4:-}"
    if [ "$target" = "` + ref + `@sha256:` + manifest + `" ]; then
      echo "from digest unsupported" >&2
      exit 1
    fi
    if [ "$target" = "` + pulledImageID + `" ]; then
      echo "container-123"
      exit 0
    fi
    ;;
  mount)
    target="${4:-}"
    if [ "$target" = "container-123" ]; then
      echo "/tmp/mount-point"
      exit 0
    fi
    ;;
esac
echo "unexpected args: $*" >&2
exit 1
`
	fakeBuildahPath := filepath.Join(fakeBinDir, "buildah")
	if err := os.WriteFile(fakeBuildahPath, []byte(fakeBuildah), 0o755); err != nil {
		t.Fatalf("WriteFile fake buildah: %v", err)
	}
	fakeSkopeo := `#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" != "inspect" ] || [ "${2:-}" != "--raw" ]; then
  echo "unexpected args: $*" >&2
  exit 1
fi
printf '%s' '` + rawManifest + `'
`
	fakeSkopeoPath := filepath.Join(fakeBinDir, "skopeo")
	if err := os.WriteFile(fakeSkopeoPath, []byte(fakeSkopeo), 0o755); err != nil {
		t.Fatalf("WriteFile fake skopeo: %v", err)
	}

	t.Setenv("PATH", fakeBinDir+":"+os.Getenv("PATH"))

	cfg := config.DefaultConfig()
	cfg.RootDir = tmp
	cfg.RuntimeDir = filepath.Join(tmp, "run")
	cfg.LogDir = filepath.Join(tmp, "log")
	cfg.BuildahRoot = filepath.Join(tmp, "buildah-root")
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}

	identity := &image.ImageIdentity{
		SourceRef:      ref,
		ManifestDigest: manifest,
	}
	if err := pullAndMountOCIPlatform(context.Background(), cfg, identity); err != nil {
		t.Fatalf("pullAndMountOCIPlatform: %v", err)
	}
	if identity.ContainerID != "container-123" {
		t.Fatalf("ContainerID=%q, want container-123", identity.ContainerID)
	}
	if identity.TempPath != "/tmp/mount-point" {
		t.Fatalf("TempPath=%q, want /tmp/mount-point", identity.TempPath)
	}
}

func TestPullAndMountOCIPlatformFallbackRejectsMutableFromRef(t *testing.T) {
	tmp := t.TempDir()
	fakeBinDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(fakeBinDir, 0o755); err != nil {
		t.Fatalf("MkdirAll fake bin: %v", err)
	}

	const ref = "example.com/repo:tag"
	rawManifest := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:1111111111111111111111111111111111111111111111111111111111111111"},"layers":[{"digest":"sha256:2222222222222222222222222222222222222222222222222222222222222222"}]}`
	manifestHash := sha256.Sum256([]byte(rawManifest))
	manifest := hex.EncodeToString(manifestHash[:])
	fakeBuildah := `#!/usr/bin/env bash
set -euo pipefail
if [ "$1" != "--root" ]; then
  echo "missing --root" >&2
  exit 1
fi
cmd="$3"
case "$cmd" in
  pull)
    target="${5:-}"
    if [ "$target" = "` + ref + `@sha256:` + manifest + `" ]; then
      echo "digest pull unsupported" >&2
      exit 1
    fi
    if [ "$target" = "` + ref + `" ]; then
      # Simulate old buildah output with no immutable image ID on stdout.
      exit 0
    fi
    ;;
  from)
    echo "unexpected buildah from call: $*" >&2
    exit 1
    ;;
esac
echo "unexpected args: $*" >&2
exit 1
`
	fakeBuildahPath := filepath.Join(fakeBinDir, "buildah")
	if err := os.WriteFile(fakeBuildahPath, []byte(fakeBuildah), 0o755); err != nil {
		t.Fatalf("WriteFile fake buildah: %v", err)
	}
	fakeSkopeo := `#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" != "inspect" ] || [ "${2:-}" != "--raw" ]; then
  echo "unexpected args: $*" >&2
  exit 1
fi
printf '%s' '` + rawManifest + `'
`
	fakeSkopeoPath := filepath.Join(fakeBinDir, "skopeo")
	if err := os.WriteFile(fakeSkopeoPath, []byte(fakeSkopeo), 0o755); err != nil {
		t.Fatalf("WriteFile fake skopeo: %v", err)
	}

	t.Setenv("PATH", fakeBinDir+":"+os.Getenv("PATH"))

	cfg := config.DefaultConfig()
	cfg.RootDir = tmp
	cfg.RuntimeDir = filepath.Join(tmp, "run")
	cfg.LogDir = filepath.Join(tmp, "log")
	cfg.BuildahRoot = filepath.Join(tmp, "buildah-root")
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}

	identity := &image.ImageIdentity{
		SourceRef:      ref,
		ManifestDigest: manifest,
	}
	err := pullAndMountOCIPlatform(context.Background(), cfg, identity)
	if err == nil {
		t.Fatal("expected pullAndMountOCIPlatform to fail when fallback pull lacks immutable local ref")
	}
}

func TestClassifySkopeoError_CaseInsensitivePermanent(t *testing.T) {
	t.Parallel()

	err := classifySkopeoError(fmt.Errorf("skopeo inspect failed: MANIFEST UNKNOWN"))
	var classified *types.ClassifiedError
	if !errors.As(err, &classified) {
		t.Fatalf("expected classified error, got %T", err)
	}
	if classified.Category != types.ErrorCategoryPermanent {
		t.Fatalf("category=%s, want %s", classified.Category, types.ErrorCategoryPermanent)
	}
}

func TestClassifySkopeoError_DefaultTransient(t *testing.T) {
	t.Parallel()

	err := classifySkopeoError(fmt.Errorf("skopeo inspect failed: temporary i/o timeout"))
	if !types.IsTransient(err) {
		t.Fatalf("expected transient error, got %v", err)
	}
}

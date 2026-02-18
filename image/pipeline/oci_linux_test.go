//go:build linux

package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/image"
)

func TestPullAndMountOCIPlatformFallbackUsesTagForFrom(t *testing.T) {
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
target="${4:-}"
case "$cmd" in
  pull)
    if [ "$target" = "` + ref + `@sha256:` + manifest + `" ]; then
      echo "digest pull unsupported" >&2
      exit 1
    fi
    if [ "$target" = "` + ref + `" ]; then
      exit 0
    fi
    ;;
  from)
    if [ "$target" = "` + ref + `@sha256:` + manifest + `" ]; then
      echo "from digest unsupported" >&2
      exit 1
    fi
    if [ "$target" = "` + ref + `" ]; then
      echo "container-123"
      exit 0
    fi
    ;;
  mount)
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

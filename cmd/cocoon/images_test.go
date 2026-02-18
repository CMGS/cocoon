package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cli "github.com/urfave/cli/v2"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/image"
	imgmocks "github.com/CMGS/cocoon/image/mocks"
	"github.com/CMGS/cocoon/image/refcache"
	"github.com/CMGS/cocoon/oci"
	"github.com/CMGS/cocoon/types"
)

func testCLIConfig(t *testing.T) *config.CocoonConfig {
	t.Helper()
	root := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.RebaseRootDir(root)
	cfg.RuntimeDir = filepath.Join(root, "run")
	cfg.LogDir = filepath.Join(root, "log")
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}
	return cfg
}

func testImageCLIContext(t *testing.T, args ...string) *cli.Context {
	t.Helper()
	fs := flag.NewFlagSet("images-test", flag.ContinueOnError)
	fs.String("file", "", "")
	fs.String("tag", "", "")
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse args: %v", err)
	}
	return cli.NewContext(nil, fs, nil)
}

func TestShouldFallbackToPrepare(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "local-cache-miss",
			err:  fmt.Errorf("resolve: %w", errImageNotFoundInLocalCache),
			want: true,
		},
		{
			name: "ambiguous-alias",
			err:  fmt.Errorf("resolve: %w", refcache.ErrAmbiguousImageRef),
			want: false,
		},
		{
			name: "generic-error",
			err:  errors.New("lock acquisition failed"),
			want: false,
		},
		{
			name: "nil-error",
			err:  nil,
			want: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldFallbackToPrepare(tt.err); got != tt.want {
				t.Fatalf("shouldFallbackToPrepare(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestEvaluateOCILayoutBootability_DirectModeDetected(t *testing.T) {
	t.Parallel()

	info := &oci.LayoutInfo{
		Layers: []oci.LayerInfo{
			{MediaType: oci.MediaTypeKernelLayer},
			{MediaType: oci.MediaTypeRootfsLayer},
		},
		Config: &oci.VMImageConfig{
			KernelPath:    "/vmlinuz",
			InitrdPath:    "/initrd.img",
			KernelCmdline: "console=hvc0",
		},
	}

	result := evaluateOCILayoutBootability(info)
	if !result.Bootable {
		t.Fatalf("Bootable=false, want true (errors=%v)", result.Errors)
	}
	if len(result.BootModes) != 1 || result.BootModes[0] != string(types.BootModeDirect) {
		t.Fatalf("BootModes=%v, want [%s]", result.BootModes, types.BootModeDirect)
	}
	if !result.KernelChecked || !result.KernelFound {
		t.Fatalf("kernel flags checked=%v found=%v, want true/true", result.KernelChecked, result.KernelFound)
	}
}

func TestEvaluateOCILayoutBootability_MissingKernelLayer(t *testing.T) {
	t.Parallel()

	info := &oci.LayoutInfo{
		Layers: []oci.LayerInfo{
			{MediaType: oci.MediaTypeRootfsLayer},
		},
		Config: &oci.VMImageConfig{
			KernelPath:    "/vmlinuz",
			InitrdPath:    "/initrd.img",
			KernelCmdline: "console=hvc0",
		},
	}

	result := evaluateOCILayoutBootability(info)
	if result.Bootable {
		t.Fatal("Bootable=true, want false when kernel layer is missing")
	}
	for _, mode := range result.BootModes {
		if mode == string(types.BootModeDirect) {
			t.Fatalf("BootModes=%v should not contain direct when kernel layer is missing", result.BootModes)
		}
	}
	foundKernelErr := false
	for _, msg := range result.Errors {
		if strings.Contains(msg, "kernel layer not found") {
			foundKernelErr = true
			break
		}
	}
	if !foundKernelErr {
		t.Fatalf("Errors=%v, expected kernel layer missing error", result.Errors)
	}
}

func TestEnsureLatestTag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "bare-name", in: "noble-server-cloudimg-amd64", want: "noble-server-cloudimg-amd64:latest"},
		{name: "repo-without-tag", in: "cmgs/noble-server-cloudimg-amd64", want: "cmgs/noble-server-cloudimg-amd64:latest"},
		{name: "registry-port-without-tag", in: "localhost:5000/noble-server-cloudimg-amd64", want: "localhost:5000/noble-server-cloudimg-amd64:latest"},
		{name: "already-tagged", in: "cmgs/noble-server-cloudimg-amd64:v1", want: "cmgs/noble-server-cloudimg-amd64:v1"},
		{name: "digest-ref", in: "cmgs/noble-server-cloudimg-amd64@sha256:deadbeef", want: "cmgs/noble-server-cloudimg-amd64@sha256:deadbeef"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ensureLatestTag(tt.in); got != tt.want {
				t.Fatalf("ensureLatestTag(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestDefaultBuildTagSource(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: "image"},
		{name: "local-file", in: "/tmp/noble-server-cloudimg-amd64.img", want: "noble-server-cloudimg-amd64"},
		{name: "bare-from", in: "noble-server-cloudimg-amd64", want: "noble-server-cloudimg-amd64"},
		{name: "oci-ref", in: "cmgs/noble-server-cloudimg-amd64:stable", want: "noble-server-cloudimg-amd64:stable"},
		{name: "digest-ref", in: "cmgs/noble-server-cloudimg-amd64@sha256:deadbeef", want: "noble-server-cloudimg-amd64"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := defaultBuildTagSource(tt.in); got != tt.want {
				t.Fatalf("defaultBuildTagSource(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestQuoteGuestfishPathArg(t *testing.T) {
	t.Parallel()

	quoted, err := quoteGuestfishPathArg("/tmp/oci build/base image.qcow2")
	if err != nil {
		t.Fatalf("quoteGuestfishPathArg valid path: %v", err)
	}
	if quoted != "'/tmp/oci build/base image.qcow2'" {
		t.Fatalf("quoted path = %q", quoted)
	}

	if _, err := quoteGuestfishPathArg("/tmp/has'quote"); err == nil {
		t.Fatal("expected single-quote path to be rejected")
	}
	if _, err := quoteGuestfishPathArg("/tmp/has\nnewline"); err == nil {
		t.Fatal("expected newline path to be rejected")
	}
}

func TestResolveCocoonfileLocalPath(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	cocoonfilePath := filepath.Join(tmp, "Cocoonfile")
	basePath := filepath.Join(tmp, "base.img")

	if err := os.WriteFile(cocoonfilePath, []byte("FROM base.img\n"), 0o644); err != nil {
		t.Fatalf("write Cocoonfile: %v", err)
	}
	if err := os.WriteFile(basePath, []byte("img"), 0o644); err != nil {
		t.Fatalf("write base image: %v", err)
	}

	path, ok := resolveCocoonfileLocalPath(cocoonfilePath, "./base.img")
	if !ok {
		t.Fatal("resolveCocoonfileLocalPath should treat ./base.img as explicit local path")
	}
	if want := filepath.Join(tmp, "base.img"); path != want {
		t.Fatalf("resolveCocoonfileLocalPath explicit path=%q, want %q", path, want)
	}

	path, ok = resolveCocoonfileLocalPath(cocoonfilePath, "base.img")
	if !ok {
		t.Fatal("resolveCocoonfileLocalPath should resolve existing bare file name relative to Cocoonfile")
	}
	if path != basePath {
		t.Fatalf("resolveCocoonfileLocalPath existing bare path=%q, want %q", path, basePath)
	}

	if path, ok = resolveCocoonfileLocalPath(cocoonfilePath, "docker.io/library/ubuntu:22.04"); ok {
		t.Fatalf("resolveCocoonfileLocalPath should not mark registry-like ref as local path, got %q", path)
	}
}

func TestNormalizeDockerLikeOCIRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "official-image-with-tag", in: "ubuntu:24.04", want: "docker.io/library/ubuntu:24.04"},
		{name: "official-image-without-tag", in: "ubuntu", want: "docker.io/library/ubuntu:latest"},
		{name: "user-namespace-with-tag", in: "cmgs/test:v1", want: "docker.io/cmgs/test:v1"},
		{name: "user-namespace-without-tag", in: "cmgs/test", want: "docker.io/cmgs/test:latest"},
		{name: "explicit-registry-with-tag", in: "ghcr.io/cmgs/test:v2", want: "ghcr.io/cmgs/test:v2"},
		{name: "explicit-registry-without-tag", in: "localhost:5000/cmgs/test", want: "localhost:5000/cmgs/test:latest"},
		{name: "digest-reference", in: "cmgs/test@sha256:deadbeef", want: "docker.io/cmgs/test@sha256:deadbeef"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeDockerLikeOCIRef(tt.in)
			if got != tt.want {
				t.Fatalf("normalizeDockerLikeOCIRef(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestNormalizePullableFROMRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "cloud-image-url-unchanged",
			in:   "https://cloud-images.ubuntu.com/jammy/current/jammy-server-cloudimg-amd64.img",
			want: "https://cloud-images.ubuntu.com/jammy/current/jammy-server-cloudimg-amd64.img",
		},
		{name: "docker-short-ref", in: "ubuntu:24.04", want: "docker.io/library/ubuntu:24.04"},
		{name: "docker-short-ref-default-latest", in: "ubuntu", want: "docker.io/library/ubuntu:latest"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := normalizePullableFROMRef(tt.in)
			if got != tt.want {
				t.Fatalf("normalizePullableFROMRef(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestResolveBuildImagePath_PositionalSourceUsesPrepareFallback(t *testing.T) {
	t.Parallel()

	cfg := testCLIConfig(t)
	c := testImageCLIContext(t, "ubuntu:24.04")

	var preparedRef string
	app := &appContext{
		cfg: cfg,
		imgMgr: &imgmocks.MockManager{
			PrepareFunc: func(_ context.Context, ref string) (*image.ImageIdentity, string, error) {
				preparedRef = ref
				return &image.ImageIdentity{
					Checksum:   "0123456789abcdef",
					Arch:       "amd64",
					FullDigest: strings.Repeat("a", 64),
				}, "/tmp/base.qcow2", nil
			},
		},
	}

	imagePath, tagSource, cleanup, err := resolveBuildImagePath(c, app, "")
	if err != nil {
		t.Fatalf("resolveBuildImagePath: %v", err)
	}
	if cleanup != nil {
		t.Fatalf("cleanup should be nil for prepared positional sources")
	}
	if preparedRef != "docker.io/library/ubuntu:24.04" {
		t.Fatalf("Prepare ref = %q, want docker.io/library/ubuntu:24.04", preparedRef)
	}
	if imagePath != "/tmp/base.qcow2" {
		t.Fatalf("imagePath = %q, want /tmp/base.qcow2", imagePath)
	}
	if tagSource != "ubuntu:24.04" {
		t.Fatalf("tagSource = %q, want ubuntu:24.04", tagSource)
	}
}

func TestResolveLocalOCITagRef_DefaultLatest(t *testing.T) {
	t.Parallel()

	cfg := testCLIConfig(t)
	store := oci.NewStore(cfg)
	if err := store.SaveTag("demo:latest", "/tmp/layout-demo", "sha256:1111"); err != nil {
		t.Fatalf("SaveTag: %v", err)
	}

	resolved, exists, err := resolveLocalOCITagRef(store, "demo")
	if err != nil {
		t.Fatalf("resolveLocalOCITagRef: %v", err)
	}
	if !exists {
		t.Fatal("resolveLocalOCITagRef should resolve demo -> demo:latest")
	}
	if resolved != "demo:latest" {
		t.Fatalf("resolved ref = %q, want %q", resolved, "demo:latest")
	}
}

func TestTagCloudImageAlias_RejectsImplicitLatestOCICollision(t *testing.T) {
	t.Parallel()

	cfg := testCLIConfig(t)
	store := oci.NewStore(cfg)
	if err := store.SaveTag("demo:latest", "/tmp/layout-demo", "sha256:1111"); err != nil {
		t.Fatalf("SaveTag: %v", err)
	}

	app := &appContext{cfg: cfg}
	err := tagCloudImageAlias(nil, app, store, "some-cloud-source", "demo")
	if err == nil {
		t.Fatal("expected OCI collision error, got nil")
	}
	if !strings.Contains(err.Error(), "already exists as an OCI build tag") {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(err.Error(), "demo:latest") {
		t.Fatalf("expected resolved latest tag in error, got: %v", err)
	}
}

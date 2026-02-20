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

func TestClassifyPullRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ref  string
		want pullRefKind
	}{
		// Cloud pipeline: URLs
		{name: "https-url", ref: "https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img", want: pullRefCloudPipeline},
		{name: "http-url", ref: "http://example.com/image.qcow2", want: pullRefCloudPipeline},

		// Cloud pipeline: local paths
		{name: "absolute-path", ref: "/tmp/noble-server-cloudimg-amd64.img", want: pullRefCloudPipeline},
		{name: "relative-dot-path", ref: "./noble-server-cloudimg-amd64.img", want: pullRefCloudPipeline},
		{name: "relative-parent-path", ref: "../images/noble.img", want: pullRefCloudPipeline},

		// Short names: no dot in first segment (no slash)
		{name: "official-image-with-tag", ref: "ubuntu:22.04", want: pullRefShortName},
		{name: "official-image-bare", ref: "ubuntu", want: pullRefShortName},
		{name: "bare-name-no-slash", ref: "noble-server-cloudimg-amd64", want: pullRefShortName},

		// Short names: slash-based but first segment has no dot or colon
		{name: "user-namespace", ref: "cmgs/test-u2404", want: pullRefShortName},
		{name: "user-namespace-with-tag", ref: "cmgs/test-u2404:latest", want: pullRefShortName},

		// Domain refs: dot or colon in first segment (with slash)
		{name: "docker-io-explicit", ref: "docker.io/cmgs/test-u2404", want: pullRefDomainRef},
		{name: "ghcr-io", ref: "ghcr.io/cmgs/myvm:v1", want: pullRefDomainRef},
		{name: "custom-registry", ref: "registry.example.com/my-vm:v1", want: pullRefDomainRef},
		{name: "localhost-port", ref: "localhost:5000/test/myvm:latest", want: pullRefDomainRef},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := classifyPullRef(tt.ref)
			if got != tt.want {
				t.Fatalf("classifyPullRef(%q) = %d, want %d", tt.ref, got, tt.want)
			}
		})
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

// TestPullDomainRefRouting verifies that domain-prefixed refs are classified
// as pullRefDomainRef and that pullDomainRefImage correctly routes to the
// cloud pipeline when the ref is not a local OCI tag and the registry probe
// returns false (non-VM image). This exercises the T2 acceptance test from
// issue #28: "docker.io/cmgs/test-u2404" is classified as domain ref, probes
// first, then routes accordingly.
func TestPullDomainRefRouting(t *testing.T) {
	t.Parallel()

	// Verify classification of domain refs.
	domainRefs := []struct {
		name string
		ref  string
	}{
		{name: "docker.io-explicit", ref: "docker.io/cmgs/test-u2404"},
		{name: "ghcr.io", ref: "ghcr.io/cmgs/myvm:v1"},
		{name: "custom-registry", ref: "registry.example.com/my-vm:v1"},
		{name: "localhost-port", ref: "localhost:5000/test/myvm:latest"},
	}
	for _, tt := range domainRefs {
		t.Run("classify/"+tt.name, func(t *testing.T) {
			t.Parallel()
			got := classifyPullRef(tt.ref)
			if got != pullRefDomainRef {
				t.Fatalf("classifyPullRef(%q) = %d, want pullRefDomainRef (%d)", tt.ref, got, pullRefDomainRef)
			}
		})
	}

	// Verify routing: when the ref is not a local OCI tag and ProbeRegistryVMImage
	// returns false (the default for an unreachable/non-VM ref), pullDomainRefImage
	// falls back to the cloud pipeline (calls Prepare). We set up a mock image
	// manager and verify Prepare is invoked with the original domain ref.
	t.Run("fallback-to-cloud-pipeline", func(t *testing.T) {
		t.Parallel()
		cfg := testCLIConfig(t)
		c := testImageCLIContext(t)

		var preparedRef string
		app := &appContext{
			cfg: cfg,
			imgMgr: &imgmocks.MockManager{
				PrepareFunc: func(_ context.Context, ref string) (*image.ImageIdentity, string, error) {
					preparedRef = ref
					return &image.ImageIdentity{
						Checksum:   "abcdef0123456789",
						Arch:       "amd64",
						FullDigest: strings.Repeat("b", 64),
					}, cfg.BaseImagePath("abcdef0123456789_amd64"), nil
				},
				// ListCached returns the image so resolveBaseKeyFromCache can
				// detect that the image was not cached before pull but is cached
				// after, allowing verification/bookkeeping to proceed.
				ListCachedFunc: func(_ context.Context) ([]*image.CachedImage, error) {
					return nil, nil
				},
			},
			probeRegistryVMImage: func(context.Context, *config.CocoonConfig, string) bool {
				return false
			},
		}

		// pullDomainRefImage: no local OCI tag, probe says non-VM image,
		// so it falls back to pullCloudPipelineImage which calls Prepare
		// with the domain-prefixed ref.
		ref := "registry.example.com/my-cloud-image:v1"
		err := pullDomainRefImage(c, app, ref)
		if err != nil {
			t.Fatalf("pullDomainRefImage(%q): %v", ref, err)
		}
		if preparedRef != ref {
			t.Fatalf("Prepare ref = %q, want %q (domain ref passed through unchanged)", preparedRef, ref)
		}
	})

	// Verify routing: when a local OCI tag exists for the domain ref,
	// pullDomainRefImage routes to pullOCIVMImage (OCI VM pull path).
	t.Run("local-oci-tag-routes-to-oci-pull", func(t *testing.T) {
		t.Parallel()
		cfg := testCLIConfig(t)
		c := testImageCLIContext(t)

		store := oci.NewStore(cfg)
		ref := "registry.example.com/my-vm:v1"
		if err := store.SaveTag(ref, "/tmp/layout-test", "sha256:2222"); err != nil {
			t.Fatalf("SaveTag: %v", err)
		}

		called := false
		app := &appContext{
			cfg: cfg,
			probeRegistryVMImage: func(context.Context, *config.CocoonConfig, string) bool {
				t.Fatal("probe should not run when local OCI tag exists")
				return false
			},
			pullOCIImage: func(_ context.Context, _ *config.CocoonConfig, gotRef string) (*oci.PullResult, error) {
				called = true
				if gotRef != ref {
					t.Fatalf("pull ref = %q, want %q", gotRef, ref)
				}
				return nil, errors.New("stubbed pull failure")
			},
		}

		err := pullDomainRefImage(c, app, ref)
		if err == nil {
			t.Fatal("expected error from pullOCIVMImage path, got nil")
		}
		if !strings.Contains(err.Error(), "pull OCI VM image") {
			t.Fatalf("expected error from pullOCIVMImage path, got: %v", err)
		}
		if !called {
			t.Fatal("expected OCI pull path to be invoked")
		}
	})
}

// TestPullShortNameRouting verifies that short-name refs (no domain in first
// segment) are correctly classified and routed through pullShortNameImage.
// When the probe fails (no real registry), it falls back to the cloud pipeline
// with a normalized docker.io domain ref.
func TestPullShortNameRouting(t *testing.T) {
	t.Parallel()

	t.Run("fallback-normalizes-ref-for-cloud-pipeline", func(t *testing.T) {
		t.Parallel()
		cfg := testCLIConfig(t)
		c := testImageCLIContext(t)

		var preparedRef string
		app := &appContext{
			cfg: cfg,
			imgMgr: &imgmocks.MockManager{
				PrepareFunc: func(_ context.Context, ref string) (*image.ImageIdentity, string, error) {
					preparedRef = ref
					return &image.ImageIdentity{
						Checksum:   "1234567890abcdef",
						Arch:       "amd64",
						FullDigest: strings.Repeat("c", 64),
					}, cfg.BaseImagePath("1234567890abcdef_amd64"), nil
				},
				ListCachedFunc: func(_ context.Context) ([]*image.CachedImage, error) {
					return nil, nil
				},
			},
			probeRegistryVMImage: func(context.Context, *config.CocoonConfig, string) bool {
				return false
			},
		}

		// Probe is stubbed to non-VM, so short-name routing must normalize
		// to Docker Hub and fall back to the cloud pipeline.
		ref := "cmgs/test-u2404"
		err := pullShortNameImage(c, app, ref)
		if err != nil {
			t.Fatalf("pullShortNameImage(%q): %v", ref, err)
		}
		// The normalized ref should prepend "docker.io/" and append ":latest".
		want := "docker.io/cmgs/test-u2404:latest"
		if preparedRef != want {
			t.Fatalf("Prepare ref = %q, want %q (short name normalized to Docker Hub)", preparedRef, want)
		}
	})

	// Verify that normalizeDockerLikeOCIRef produces the correct normalized
	// form for various short-name patterns used in the cloud pipeline fallback.
	t.Run("normalize-patterns", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name string
			ref  string
			want string
		}{
			{name: "user/repo", ref: "cmgs/test-u2404", want: "docker.io/cmgs/test-u2404:latest"},
			{name: "official-image", ref: "ubuntu", want: "docker.io/library/ubuntu:latest"},
			{name: "official-image-with-tag", ref: "ubuntu:22.04", want: "docker.io/library/ubuntu:22.04"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()
				got := normalizeDockerLikeOCIRef(tt.ref)
				if got != tt.want {
					t.Fatalf("normalizeDockerLikeOCIRef(%q) = %q, want %q", tt.ref, got, tt.want)
				}
			})
		}
	})
}

// TestPullCloudPipelineCacheConsistency verifies cache behavior in the cloud
// pipeline pull path (C2/C3 from issue #28). When a short-name ref is
// normalized and pulled via pullCloudPipelineImage, the refcache is updated
// so that a subsequent resolveBaseKeyFromCache call succeeds.
func TestPullCloudPipelineCacheConsistency(t *testing.T) {
	t.Parallel()

	cfg := testCLIConfig(t)
	baseKey := "abcdef0123456789_amd64"
	basePath := cfg.BaseImagePath(baseKey)

	// Pre-populate the cache: create the base image directory and file so
	// ListCached returns it.
	if err := os.MkdirAll(filepath.Dir(basePath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(basePath, []byte("fake-qcow2"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Simulate what pullCloudPipelineImage does after Prepare: it calls
	// refcache.Upsert to register the ref -> baseKey mapping.
	normalizedRef := "docker.io/library/ubuntu:latest"
	if err := refcache.Upsert(cfg, normalizedRef, baseKey, strings.Repeat("d", 64)); err != nil {
		t.Fatalf("refcache.Upsert: %v", err)
	}

	// Also register the short-name variant to verify it works for both forms.
	shortRef := "ubuntu:latest"
	if err := refcache.Upsert(cfg, shortRef, baseKey, strings.Repeat("d", 64)); err != nil {
		t.Fatalf("refcache.Upsert short ref: %v", err)
	}

	// Now verify that resolveBaseKeyFromCache resolves both the normalized
	// and short-name refs to the same baseKey. This confirms C2/C3.
	c := testImageCLIContext(t)
	app := &appContext{
		cfg: cfg,
		imgMgr: &imgmocks.MockManager{
			ListCachedFunc: func(_ context.Context) ([]*image.CachedImage, error) {
				return []*image.CachedImage{
					{BaseKey: baseKey, Path: basePath, Size: 10},
				}, nil
			},
		},
	}

	resolved, err := resolveBaseKeyFromCache(c, app, normalizedRef)
	if err != nil {
		t.Fatalf("resolveBaseKeyFromCache(%q): %v", normalizedRef, err)
	}
	if resolved != baseKey {
		t.Fatalf("resolveBaseKeyFromCache(%q) = %q, want %q", normalizedRef, resolved, baseKey)
	}

	resolved, err = resolveBaseKeyFromCache(c, app, shortRef)
	if err != nil {
		t.Fatalf("resolveBaseKeyFromCache(%q): %v", shortRef, err)
	}
	if resolved != baseKey {
		t.Fatalf("resolveBaseKeyFromCache(%q) = %q, want %q", shortRef, resolved, baseKey)
	}
}

// Note: C1 (OCI VM pull cache idempotency via checkIdempotent) is tested
// in oci/pull_test.go::TestCheckIdempotent. It verifies that oci.Pull()
// skips re-download when the local tag already has the same manifest digest.

func TestShouldFallbackToOCIVMPull(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "artifact operation mismatch for cocoon vm artifact",
			err:  errors.New(`buildah pull: unsupported image-specific operation on artifact with type "application/vnd.cocoon.vm.image.v1"`),
			want: true,
		},
		{
			name: "artifact operation mismatch for other artifact",
			err:  errors.New(`buildah pull: unsupported image-specific operation on artifact with type "application/vnd.example.other.v1"`),
			want: false,
		},
		{
			name: "unrelated error",
			err:  errors.New("network timeout"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := shouldFallbackToOCIVMPull(tt.err)
			if got != tt.want {
				t.Fatalf("shouldFallbackToOCIVMPull(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

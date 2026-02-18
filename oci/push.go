package oci

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/types"
)

// Push uploads a locally built OCI VM image to a container registry.
// The ref must match a tag previously created by Build.
func Push(ctx context.Context, cfg *config.CocoonConfig, ref string) (*PushResult, error) {
	store := NewStore(cfg)

	layoutPath, err := store.ResolveTag(ref)
	if err != nil {
		return nil, fmt.Errorf("resolve tag %q: %w", ref, err)
	}

	// Load OCI image layout.
	idx, err := layout.ImageIndexFromPath(layoutPath)
	if err != nil {
		return nil, fmt.Errorf("read OCI layout from %s: %w", layoutPath, err)
	}

	// Get the manifest index to find our single image.
	indexManifest, err := idx.IndexManifest()
	if err != nil {
		return nil, fmt.Errorf("read index manifest: %w", err)
	}
	if len(indexManifest.Manifests) == 0 {
		return nil, fmt.Errorf("OCI layout at %s contains no manifests", layoutPath)
	}

	// Get the first (and only) image from the index.
	desc := indexManifest.Manifests[0]
	img, err := idx.Image(desc.Digest)
	if err != nil {
		return nil, fmt.Errorf("load image from layout: %w", err)
	}

	// Parse the registry reference.
	tag, err := name.NewTag(ref)
	if err != nil {
		return nil, types.NewPermanentError(fmt.Errorf("invalid registry reference %q: %w", ref, err))
	}

	// Push to registry using Cocoon's own keychain, falling back to Docker's default.
	keychain := authn.NewMultiKeychain(CocoonKeychain(), authn.DefaultKeychain)
	if err = remote.Write(tag, img, remote.WithAuthFromKeychain(keychain), remote.WithContext(ctx)); err != nil {
		return nil, classifyPushError(err)
	}

	// Get the manifest digest for the result.
	manifestDigest, err := img.Digest()
	if err != nil {
		// Push succeeded but we couldn't get the digest — non-critical.
		return &PushResult{
			Ref: ref,
		}, nil
	}

	return &PushResult{
		Ref:            ref,
		ManifestDigest: manifestDigest.String(),
	}, nil
}

// classifyPushError categorizes push errors as transient or permanent.
func classifyPushError(err error) error {
	errStr := err.Error()

	// Permanent errors: authentication, authorization, not found.
	permanentPatterns := []string{
		"UNAUTHORIZED",
		"unauthorized",
		"DENIED",
		"denied",
		"FORBIDDEN",
		"forbidden",
		"NAME_UNKNOWN",
		"not found",
		"invalid reference",
	}
	for _, p := range permanentPatterns {
		if strings.Contains(errStr, p) {
			return types.NewPermanentError(fmt.Errorf("push %w", err))
		}
	}

	// Transient errors: network issues, timeouts, server errors.
	if isNetworkError(err) {
		return types.NewTransientError(fmt.Errorf("push %w", err))
	}
	transientPatterns := []string{
		"timeout",
		"connection refused",
		"connection reset",
		"EOF",
		"INTERNAL_ERROR",
		"BAD_GATEWAY",
		"SERVICE_UNAVAILABLE",
		"TOO_MANY_REQUESTS",
		"Service Unavailable",
		"Too Many Requests",
		"Internal Server Error",
		"Bad Gateway",
	}
	errUpper := strings.ToUpper(errStr)
	for _, p := range transientPatterns {
		if strings.Contains(errUpper, strings.ToUpper(p)) {
			return types.NewTransientError(fmt.Errorf("push %w", err))
		}
	}

	// Default to permanent for unknown errors.
	return types.NewPermanentError(fmt.Errorf("push %w", err))
}

func isNetworkError(err error) bool {
	if _, ok := err.(*net.OpError); ok { //nolint:errorlint // direct type check is intentional
		return true
	}
	if os.IsTimeout(err) {
		return true
	}
	return false
}

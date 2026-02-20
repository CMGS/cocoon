package oci

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/types"
	"github.com/CMGS/cocoon/utils"
)

const pushProgressRefreshInterval = 250 * time.Millisecond

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
	progressUpdates := make(chan v1.Update, 64)
	progressDone := make(chan struct{})
	var progressWG sync.WaitGroup
	progressWG.Go(func() {
		renderPushProgress(progressUpdates, progressDone, os.Stderr)
	})
	fmt.Fprintf(os.Stderr, "Pushing image: %s\n", ref)
	if err = remote.Write(
		tag,
		img,
		remote.WithAuthFromKeychain(keychain),
		remote.WithContext(ctx),
		remote.WithProgress(progressUpdates),
	); err != nil {
		close(progressDone)
		progressWG.Wait()
		return nil, classifyRegistryError("push", err)
	}
	close(progressDone)
	progressWG.Wait()

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

// renderPushProgress drains push progress updates and periodically renders
// them to w. It returns only when the done channel is closed.
//
// Contract: the caller MUST close done to signal completion; otherwise this
// goroutine will block indefinitely. The updates channel may be closed
// independently (e.g., by the remote.Write call) — renderPushProgress
// handles that by nil-ing the channel and continuing to wait for done.
func renderPushProgress(updates <-chan v1.Update, done <-chan struct{}, w io.Writer) {
	ticker := time.NewTicker(pushProgressRefreshInterval)
	defer ticker.Stop()

	var latest v1.Update
	hasUpdate := false
	for {
		select {
		case update, ok := <-updates:
			if !ok {
				updates = nil
				continue
			}
			latest = update
			hasUpdate = true
		case <-ticker.C:
			if !hasUpdate {
				continue
			}
			_, _ = fmt.Fprintf(w, "\r%s", formatPushProgressLine(latest))
		case <-done:
			if hasUpdate {
				_, _ = fmt.Fprintf(w, "\r%s\n", formatPushProgressLine(latest))
			}
			return
		}
	}
}

func formatPushProgressLine(update v1.Update) string {
	if update.Total > 0 {
		percent := int64(0)
		if update.Complete > 0 {
			percent = (update.Complete * 100) / update.Total
		}
		if percent > 100 {
			percent = 100
		}
		return fmt.Sprintf("Pushing image: %3d%% (%s / %s)", percent, utils.HumanBytes(update.Complete), utils.HumanBytes(update.Total))
	}
	return fmt.Sprintf("Pushing image: %s", utils.HumanBytes(update.Complete))
}

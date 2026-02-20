package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	cli "github.com/urfave/cli/v2"
	"golang.org/x/term"

	"github.com/CMGS/cocoon/image"
	"github.com/CMGS/cocoon/image/refcache"
	"github.com/CMGS/cocoon/lock/flock"
	"github.com/CMGS/cocoon/oci"
	"github.com/CMGS/cocoon/types"
	"github.com/CMGS/cocoon/utils"
)

var errImageNotFoundInLocalCache = errors.New("image not found in local cache")

const defaultDerivedBuildTag = "image"

func shouldFallbackToPrepare(resolveErr error) bool {
	return errors.Is(resolveErr, errImageNotFoundInLocalCache)
}

func resolveVerifyImagePath(c *cli.Context, app *appContext, arg string) (string, error) {
	// If the argument is a local file path, use it directly.
	if _, statErr := os.Stat(arg); statErr == nil {
		return arg, nil
	}

	// Prefer local cache resolution first to avoid network pulls.
	baseKey, resolveErr := resolveBaseKeyFromCache(c, app, arg)
	if resolveErr == nil {
		return app.cfg.BaseImagePath(baseKey), nil
	}

	// Only fall back to Prepare when the image is simply absent from
	// local cache. For ambiguous refs or cache read errors, surface the
	// local resolution error to avoid unexpected network pulls.
	if !shouldFallbackToPrepare(resolveErr) {
		return "", resolveErr
	}

	_, basePath, prepErr := app.imgMgr.Prepare(c.Context, arg)
	if prepErr != nil {
		return "", fmt.Errorf("resolve image ref %q: %w", arg, prepErr)
	}
	return basePath, nil
}

func imagesCommand() *cli.Command {
	return &cli.Command{
		Name:  "image",
		Usage: "Manage VM images",
		Subcommands: []*cli.Command{
			imageListCommand(),
			imagePullCommand(),
			imageBuildCommand(),
			imagePushCommand(),
			imageLoginCommand(),
			imageInspectCommand(),
			imageTagCommand(),
			imageRemoveCommand(),
			imageVerifyCommand(),
		},
	}
}

func imageListCommand() *cli.Command {
	return &cli.Command{
		Name:    "list",
		Aliases: []string{"ls"},
		Usage:   "List cached VM images",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "format",
				Value: "table",
				Usage: "output format (table, json)",
			},
		},
		Action: imagesAction,
	}
}

// unifiedImageRow is the CLI view model for the unified image list output.
type unifiedImageRow struct {
	Type      string `json:"type"`
	Ref       string `json:"ref"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	SizeHuman string `json:"size_human"`
	Source    string `json:"source"`
	CreatedAt string `json:"created_at"`
}

func imagesAction(c *cli.Context) error {
	app, err := initApp(c)
	if err != nil {
		return err
	}

	format := c.String("format")
	if fmtErr := validateOutputFormat(format); fmtErr != nil {
		return fmtErr
	}
	var rows []unifiedImageRow

	// 1. Cloud images.
	images, err := app.imgMgr.ListCached(c.Context)
	if err != nil {
		return fmt.Errorf("list cached images: %w", err)
	}
	for _, img := range images {
		sourceRefs, _, refsErr := refcache.RefsForBaseKey(app.cfg, img.BaseKey)
		if refsErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: read manifest cache for %s: %v\n", img.BaseKey, refsErr)
		}
		source := summarizeSourceRefs(sourceRefs)
		rows = append(rows, unifiedImageRow{
			Type:      "cloudimg",
			Ref:       img.BaseKey,
			Digest:    "", // cloud images don't have OCI digests
			Size:      img.Size,
			SizeHuman: humanBytes(img.Size),
			Source:    source,
			CreatedAt: img.CreatedAt.Format("2006-01-02T15:04:05Z"),
		})
	}

	// 2. OCI images.
	ociImages, err := app.imgBuild.ListBuilds(c.Context)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: list OCI builds: %v\n", err)
	}
	for _, entry := range ociImages {
		size := ociEntrySize(entry.LayoutPath)
		digest := entry.ManifestDigest
		if digest != "" && !strings.HasPrefix(digest, "sha256:") {
			digest = "sha256:" + digest
		}
		rows = append(rows, unifiedImageRow{
			Type:      "oci",
			Ref:       entry.Tag,
			Digest:    digest,
			Size:      size,
			SizeHuman: humanBytes(size),
			Source:    "local",
			CreatedAt: entry.CreatedAt.Format("2006-01-02T15:04:05Z"),
		})
	}

	// Sort by creation time, newest first.
	sort.Slice(rows, func(i, j int) bool {
		ti, _ := time.Parse("2006-01-02T15:04:05Z", rows[i].CreatedAt)
		tj, _ := time.Parse("2006-01-02T15:04:05Z", rows[j].CreatedAt)
		return ti.After(tj)
	})

	// JSON output.
	if format == formatJSON {
		return printJSON(rows)
	}

	// Table output (default).
	headers := []string{"TYPE", "REF", "DIGEST", "SIZE", "SOURCE", "CREATED"}
	tableRows := make([][]string, 0, len(rows))
	for _, row := range rows {
		digest := row.Digest
		if digest != "" {
			digest = truncateDigest(digest)
		} else {
			digest = "-"
		}
		tableRows = append(tableRows, []string{
			row.Type,
			row.Ref,
			digest,
			row.SizeHuman,
			row.Source,
			row.CreatedAt,
		})
	}
	printTable(headers, tableRows)
	return nil
}

func imagePullCommand() *cli.Command {
	return &cli.Command{
		Name:      "pull",
		Usage:     "Pull and cache an image without creating a VM",
		ArgsUsage: "IMAGE_REF",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "skip-verify",
				Usage: "skip bootability verification after pull",
			},
		},
		Action: imagePullAction,
	}
}

// pullRefKind categorizes a pull reference for three-way routing.
type pullRefKind int

const (
	// pullRefCloudPipeline routes through the existing cloud image pipeline
	// (Buildah/skopeo for OCI container images, or direct download for URLs
	// and local paths).
	pullRefCloudPipeline pullRefKind = iota
	// pullRefShortName is a Docker-style short name (e.g., "cmgs/test-u2404",
	// "ubuntu:22.04") where the first path segment has no dot. These are
	// routed through the OCI pipeline (go-containerregistry) which auto-
	// resolves short names to Docker Hub without requiring registries.conf.
	pullRefShortName
	// pullRefDomainRef is a domain-prefixed registry reference (e.g.,
	// "docker.io/cmgs/test", "registry.example.com/my-vm:v1") where the
	// first path segment contains a dot or colon. These are probed first
	// for Cocoon VM media types, then fall back to the cloud pipeline.
	pullRefDomainRef
)

// classifyPullRef determines the routing category for a pull reference.
//
// Classification rules follow the Docker reference specification:
//   - URL (http:// or https:// prefix) -> cloud pipeline
//   - Local path (/, ./, ../ prefix, or stat() succeeds) -> cloud pipeline
//   - Short name (no "/" or first segment before "/" has no "." or ":") -> OCI pipeline
//   - Domain ref (first segment before "/" contains "." or ":") -> probe then route
func classifyPullRef(ref string) pullRefKind {
	// URLs always go to the cloud pipeline.
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return pullRefCloudPipeline
	}
	// Explicit local path prefixes.
	if strings.HasPrefix(ref, "/") || strings.HasPrefix(ref, "./") || strings.HasPrefix(ref, "../") {
		return pullRefCloudPipeline
	}
	// Bare filename that exists on disk.
	if fi, err := os.Stat(ref); err == nil && fi.Mode().IsRegular() {
		return pullRefCloudPipeline
	}

	// A ref with no "/" is always a short name — the ":" and "." characters
	// are part of the tag (e.g., "ubuntu:22.04"), not a domain or port.
	// Domain detection only applies when there is a "/" separating a
	// potential hostname from the repository path.
	firstSeg, _, hasSlash := strings.Cut(ref, "/")
	if !hasSlash {
		return pullRefShortName
	}

	// With a slash present, inspect the first segment. If it contains "."
	// (e.g., "docker.io", "registry.example.com") or ":" (e.g.,
	// "localhost:5000") it is a domain-prefixed ref.
	if strings.Contains(firstSeg, ".") || strings.Contains(firstSeg, ":") {
		return pullRefDomainRef
	}

	// A slash-containing ref whose first segment has no "." or ":"
	// is a Docker Hub short name (e.g., "cmgs/test-u2404").
	return pullRefShortName
}

func imagePullAction(c *cli.Context) error {
	if c.NArg() < 1 {
		return fmt.Errorf("IMAGE_REF argument required\n\nUsage: cocoon image pull IMAGE_REF")
	}

	app, err := initApp(c)
	if err != nil {
		return err
	}

	ref := c.Args().Get(0)

	switch classifyPullRef(ref) {
	case pullRefShortName:
		return pullShortNameImage(c, app, ref)
	case pullRefDomainRef:
		return pullDomainRefImage(c, app, ref)
	default:
		return pullCloudPipelineImage(c, app, ref)
	}
}

// pullShortNameImage handles short-name refs (no domain in first segment).
// Short names are routed through the OCI pipeline (go-containerregistry)
// which auto-resolves them to Docker Hub. If the image is a Cocoon VM
// image, it is stored in the OCI store. If it is a standard OCI image,
// it falls back to the cloud image pipeline with a normalized domain ref.
func pullShortNameImage(c *cli.Context, app *appContext, ref string) error {
	// Check for a local OCI tag first — supports repull/update semantics.
	store := oci.NewStore(app.cfg)
	_, exists, tagErr := resolveLocalOCITagRef(store, ref)
	if tagErr == nil && exists {
		return pullOCIVMImage(c, app, ref)
	}

	// Probe the remote registry using the go-containerregistry default
	// resolution (short names auto-resolve to index.docker.io).
	if probeRegistryVMImage(app, c.Context, ref) {
		return pullOCIVMImage(c, app, ref)
	}

	// Not a Cocoon VM image — fall back to cloud pipeline with a
	// Docker-normalized domain ref so buildah/skopeo can resolve it.
	normalizedRef := normalizeDockerLikeOCIRef(ref)
	return pullCloudPipelineImage(c, app, normalizedRef)
}

// pullDomainRefImage handles domain-prefixed refs. It probes for a Cocoon
// VM image first (local OCI tag or remote manifest), then falls back to
// the cloud pipeline.
func pullDomainRefImage(c *cli.Context, app *appContext, ref string) error {
	// Check for a local OCI tag first.
	store := oci.NewStore(app.cfg)
	_, exists, tagErr := resolveLocalOCITagRef(store, ref)
	if tagErr == nil && exists {
		return pullOCIVMImage(c, app, ref)
	}

	// Probe registry for Cocoon VM media types.
	if probeRegistryVMImage(app, c.Context, ref) {
		return pullOCIVMImage(c, app, ref)
	}

	// Not a Cocoon VM image — use the cloud image pipeline.
	return pullCloudPipelineImage(c, app, ref)
}

// pullCloudPipelineImage runs the existing cloud image pipeline (Prepare)
// with bootability verification and cache bookkeeping.
func pullCloudPipelineImage(c *cli.Context, app *appContext, ref string) error {
	// Check if the image is already cached before pulling.
	_, cacheLookupErr := resolveBaseKeyFromCache(c, app, ref)
	cachedBefore := cacheLookupErr == nil

	// Use the Prepare pipeline to pull + convert + cache the image.
	identity, basePath, err := app.imgMgr.Prepare(c.Context, ref)
	if err != nil {
		if shouldFallbackToOCIVMPull(err) {
			log.Printf("warning: cloud-image pull path rejected OCI artifact for %q; retrying with OCI VM pull pipeline", ref)
			return pullOCIVMImage(c, app, ref)
		}
		return fmt.Errorf("pull image %q: %w", ref, err)
	}

	// Post-pull bootability verification.
	if shouldVerifyPulledImage(app, identity.BaseKey(), cachedBefore, c.Bool("skip-verify")) {
		if err := verifyPulledImage(c.Context, app, ref, basePath, identity.BaseKey()); err != nil {
			return err
		}
	}
	if idxErr := refcache.Upsert(app.cfg, ref, identity.BaseKey(), identity.FullDigest); idxErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: update manifest cache for %q: %v\n", ref, idxErr)
	}

	fmt.Printf("Pulled: %s\n", ref)
	fmt.Printf("Base key: %s\n", identity.BaseKey())
	fmt.Printf("Cached at: %s\n", basePath)

	return nil
}

// pullOCIVMImage pulls an OCI VM image from a remote registry into the local store.
func pullOCIVMImage(c *cli.Context, app *appContext, ref string) error {
	result, err := pullOCIImage(app, c.Context, ref)
	if err != nil {
		return fmt.Errorf("pull OCI VM image %q: %w", ref, err)
	}

	fmt.Printf("Pulled OCI VM image: %s\n", result.Ref)
	fmt.Printf("Manifest digest: %s\n", result.ManifestDigest)
	fmt.Printf("Layout: %s\n", result.LayoutPath)
	return nil
}

func probeRegistryVMImage(app *appContext, ctx context.Context, ref string) bool {
	if app == nil || app.cfg == nil {
		return false
	}
	if app.probeRegistryVMImage != nil {
		return app.probeRegistryVMImage(ctx, app.cfg, ref)
	}
	return oci.ProbeRegistryVMImage(ctx, app.cfg, ref)
}

func pullOCIImage(app *appContext, ctx context.Context, ref string) (*oci.PullResult, error) {
	if app == nil || app.cfg == nil {
		return nil, fmt.Errorf("application context is not initialized")
	}
	if app.pullOCIImage != nil {
		return app.pullOCIImage(ctx, app.cfg, ref)
	}
	return oci.Pull(ctx, app.cfg, ref)
}

func shouldFallbackToOCIVMPull(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unsupported image-specific operation on artifact with type") &&
		strings.Contains(msg, strings.ToLower(oci.ArtifactTypeVMImage))
}

func shouldVerifyPulledImage(app *appContext, baseKey string, cachedBefore bool, skipVerify bool) bool {
	if skipVerify {
		return false
	}
	verified, err := refcache.IsVerified(app.cfg, baseKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: read verify state for %s: %v\n", baseKey, err)
		return true
	}
	return !cachedBefore || !verified
}

func verifyPulledImage(ctx context.Context, app *appContext, ref, basePath, baseKey string) error {
	result, verifyErr := app.imgMgr.VerifyBootability(ctx, basePath)
	if verifyErr != nil {
		return fmt.Errorf("verify bootability for %q: %w", ref, verifyErr)
	}
	if !result.Bootable {
		if len(result.Errors) > 0 {
			return fmt.Errorf("%w: %s - %v", types.ErrImageNotBootable, ref, result.Errors)
		}
		return fmt.Errorf("%w: %s", types.ErrImageNotBootable, ref)
	}
	for _, warning := range result.Warnings {
		fmt.Fprintf(os.Stderr, "Warning: image %s: %s\n", ref, warning)
	}
	if markErr := refcache.MarkVerified(app.cfg, baseKey); markErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: persist verify state for %s: %v\n", baseKey, markErr)
	}
	return nil
}

func imageInspectCommand() *cli.Command {
	return &cli.Command{
		Name:      "inspect",
		Usage:     "Show details of a cached cloud image or locally built OCI VM image",
		ArgsUsage: "IMAGE_REF",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "format",
				Value: "json",
				Usage: "output format (json)",
			},
		},
		Action: imageInspectAction,
	}
}

func imageInspectAction(c *cli.Context) error {
	if c.NArg() < 1 {
		return fmt.Errorf("IMAGE_REF argument required\n\nUsage: cocoon image inspect IMAGE_REF")
	}

	app, err := initApp(c)
	if err != nil {
		return err
	}

	ref := c.Args().Get(0)

	// Resolution priority:
	// 1. Try OCI VM image store (local builds).
	// 2. Try cloud image cache.
	// 3. Error.
	store := oci.NewStore(app.cfg)
	ociRef, ociExists, ociErr := resolveLocalOCITagRef(store, ref)
	if ociErr != nil {
		return fmt.Errorf("check OCI tag %q: %w", ref, ociErr)
	}
	if ociExists {
		layoutPath, resolveErr := store.ResolveTag(ociRef)
		if resolveErr != nil {
			return fmt.Errorf("resolve OCI tag %q: %w", ociRef, resolveErr)
		}
		return inspectOCIImage(ociRef, layoutPath)
	}

	// Fall back to cloud image cache.
	baseKey, err := resolveBaseKeyFromCache(c, app, ref)
	if err != nil {
		if errors.Is(err, errImageNotFoundInLocalCache) {
			return fmt.Errorf("image not found: %q is not a local OCI build tag or cached cloud image", ref)
		}
		return err
	}

	return inspectCloudImage(c, app, baseKey)
}

func inspectOCIImage(tag, layoutPath string) error {
	info, err := oci.InspectLayout(layoutPath)
	if err != nil {
		return fmt.Errorf("inspect OCI layout: %w", err)
	}

	result := ociInspectInfo{
		Type:           "oci",
		Tag:            tag,
		ManifestDigest: info.ManifestDigest,
		Layers:         info.Layers,
		Config:         info.Config,
		LayoutPath:     layoutPath,
	}
	return printJSON(result)
}

func inspectCloudImage(c *cli.Context, app *appContext, baseKey string) error {
	images, err := app.imgMgr.ListCached(c.Context)
	if err != nil {
		return fmt.Errorf("list cached images: %w", err)
	}

	for _, img := range images {
		if img.BaseKey == baseKey {
			refs, refErr := app.refCtr.GetReferences(baseKey)
			refCount := img.RefCount
			if refErr == nil {
				refCount = len(refs)
			}

			found := cloudInspectInfo{
				Type:       "cloudimg",
				BaseKey:    img.BaseKey,
				Path:       img.Path,
				Size:       img.Size,
				SizeHuman:  humanBytes(img.Size),
				RefCount:   refCount,
				Refs:       refs,
				CachedAt:   img.CreatedAt.Format("2006-01-02T15:04:05Z"),
				SourceRefs: []string{},
			}
			sourceRefs, digestFull, idxErr := refcache.RefsForBaseKey(app.cfg, baseKey)
			if idxErr != nil {
				fmt.Fprintf(os.Stderr, "Warning: read manifest cache for %s: %v\n", baseKey, idxErr)
			} else {
				found.SourceRefs = sourceRefs
				found.DigestFull = digestFull
			}
			return printJSON(found)
		}
	}

	return fmt.Errorf("cached image not found: %s", baseKey)
}

// ociInspectInfo is the output for OCI VM image inspection.
type ociInspectInfo struct {
	Type           string             `json:"type"`
	Tag            string             `json:"tag"`
	ManifestDigest string             `json:"manifest_digest"`
	Layers         []oci.LayerInfo    `json:"layers"`
	Config         *oci.VMImageConfig `json:"config"`
	LayoutPath     string             `json:"layout_path"`
}

// cloudInspectInfo is the output for cloud image inspection.
type cloudInspectInfo struct {
	Type       string   `json:"type"`
	BaseKey    string   `json:"base_key"`
	Path       string   `json:"path"`
	Size       int64    `json:"size"`
	SizeHuman  string   `json:"size_human"`
	RefCount   int      `json:"ref_count"`
	Refs       []string `json:"refs,omitempty"`
	SourceRefs []string `json:"source_refs,omitempty"`
	DigestFull string   `json:"digest_full,omitempty"`
	CachedAt   string   `json:"cached_at"`
}

func imageTagCommand() *cli.Command {
	return &cli.Command{
		Name:      "tag",
		Usage:     "Create or update a local image tag/alias",
		ArgsUsage: "SOURCE_REF TARGET_REF",
		Action:    imageTagAction,
	}
}

func imageTagAction(c *cli.Context) error {
	if c.NArg() < 2 {
		return fmt.Errorf("SOURCE_REF and TARGET_REF arguments required\n\nUsage: cocoon image tag SOURCE_REF TARGET_REF")
	}

	app, err := initApp(c)
	if err != nil {
		return err
	}

	sourceRef := strings.TrimSpace(c.Args().Get(0))
	targetRef := strings.TrimSpace(c.Args().Get(1))
	if sourceRef == "" || targetRef == "" {
		return fmt.Errorf("source and target refs must be non-empty")
	}
	if sourceRef == targetRef {
		fmt.Printf("Tag unchanged: %s\n", sourceRef)
		return nil
	}

	store := oci.NewStore(app.cfg)
	ociSourceRef, sourceIsOCI, err := resolveLocalOCITagRef(store, sourceRef)
	if err != nil {
		return fmt.Errorf("check OCI source tag %q: %w", sourceRef, err)
	}
	if sourceIsOCI {
		targetRef = ensureLatestTag(targetRef)
		return tagOCIImage(app, store, ociSourceRef, targetRef)
	}
	return tagCloudImageAlias(c, app, store, sourceRef, targetRef)
}

func tagOCIImage(app *appContext, store *oci.Store, sourceRef, targetRef string) error {
	targetBaseKey, targetCloudFound, resolveErr := refcache.ResolveBaseKey(app.cfg, targetRef)
	if resolveErr != nil {
		return fmt.Errorf("check cloud image aliases for target %q: %w", targetRef, resolveErr)
	}
	if targetCloudFound {
		return fmt.Errorf("target ref %q already maps to cached cloud image %s; choose a different target ref", targetRef, targetBaseKey)
	}

	entry, err := store.GetTag(sourceRef)
	if err != nil {
		return fmt.Errorf("read OCI source tag %q: %w", sourceRef, err)
	}
	if _, statErr := os.Stat(entry.LayoutPath); statErr != nil {
		return fmt.Errorf("source OCI layout for %q is not available at %s: %w", sourceRef, entry.LayoutPath, statErr)
	}
	if saveErr := store.SaveTag(targetRef, entry.LayoutPath, entry.ManifestDigest); saveErr != nil {
		return fmt.Errorf("save OCI tag %q: %w", targetRef, saveErr)
	}

	fmt.Printf("Tagged OCI image: %s -> %s\n", sourceRef, targetRef)
	if entry.ManifestDigest != "" {
		fmt.Printf("Manifest: %s\n", entry.ManifestDigest)
	}
	return nil
}

func tagCloudImageAlias(c *cli.Context, app *appContext, store *oci.Store, sourceRef, targetRef string) error {
	resolvedOCITarget, targetIsOCI, err := resolveLocalOCITagRef(store, targetRef)
	if err != nil {
		return fmt.Errorf("check OCI target tag %q: %w", targetRef, err)
	}
	if targetIsOCI {
		return fmt.Errorf("target ref %q already exists as an OCI build tag (%s); choose a different target ref", targetRef, resolvedOCITarget)
	}

	baseKey, digestFull, err := resolveSourceCloudAlias(c, app, sourceRef)
	if err != nil {
		return err
	}
	if upsertErr := refcache.Upsert(app.cfg, targetRef, baseKey, digestFull); upsertErr != nil {
		return fmt.Errorf("save cloud image alias %q -> %s: %w", targetRef, baseKey, upsertErr)
	}

	fmt.Printf("Tagged cloud image: %s -> %s\n", sourceRef, targetRef)
	fmt.Printf("Base key: %s\n", baseKey)
	return nil
}

func resolveSourceCloudAlias(c *cli.Context, app *appContext, sourceRef string) (string, string, error) {
	baseKey, resolveErr := resolveBaseKeyFromCache(c, app, sourceRef)
	if resolveErr == nil {
		_, digest, refsErr := refcache.RefsForBaseKey(app.cfg, baseKey)
		if refsErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: read manifest cache for %s: %v\n", baseKey, refsErr)
			return baseKey, "", nil
		}
		return baseKey, digest, nil
	}

	// Allow local file sources by preparing them into cache before aliasing.
	if _, statErr := os.Stat(sourceRef); statErr != nil {
		return "", "", fmt.Errorf("resolve source cloud image %q: %w", sourceRef, resolveErr)
	}
	identity, _, prepErr := app.imgMgr.Prepare(c.Context, sourceRef)
	if prepErr != nil {
		return "", "", fmt.Errorf("prepare local source image %q: %w", sourceRef, prepErr)
	}
	return identity.BaseKey(), identity.FullDigest, nil
}

func imageRemoveCommand() *cli.Command {
	return &cli.Command{
		Name:      "remove",
		Aliases:   []string{"rm"},
		Usage:     "Remove one or more cached images (only if unreferenced)",
		ArgsUsage: "IMAGE_REF [IMAGE_REF...]",
		Action:    imageRemoveAction,
	}
}

func imageRemoveAction(c *cli.Context) error {
	if c.NArg() < 1 {
		return fmt.Errorf("IMAGE_REF argument required\n\nUsage: cocoon image remove IMAGE_REF [IMAGE_REF...]")
	}

	app, err := initApp(c)
	if err != nil {
		return err
	}

	var errs []string
	removed := 0
	for i := 0; i < c.NArg(); i++ {
		ref := c.Args().Get(i)
		if rmErr := removeOneImage(c, app, ref); rmErr != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", ref, rmErr))
			continue
		}
		removed++
	}

	if len(errs) > 0 {
		return fmt.Errorf("removed %d of %d image(s); %d failed:\n  %s", removed, c.NArg(), len(errs), strings.Join(errs, "\n  "))
	}
	return nil
}

func removeOneImage(c *cli.Context, app *appContext, ref string) error {
	// Try OCI image removal first.
	// Use tag-index existence instead of ResolveTag so we can clean stale tags
	// even when the layout directory has already been deleted.
	store := oci.NewStore(app.cfg)
	ociRef, tagExists, tagErr := resolveLocalOCITagRef(store, ref)
	if tagErr != nil {
		return fmt.Errorf("check OCI tag %q: %w", ref, tagErr)
	}
	if tagExists {
		manifestDigest, zeroRefBlobs, removeErr := store.RemoveTag(ociRef)
		if removeErr != nil {
			return fmt.Errorf("remove OCI image: %w", removeErr)
		}
		// Remove zero-referenced blobs from shared store.
		blobStore := oci.NewBlobStore(app.cfg)
		for _, digest := range zeroRefBlobs {
			if blobErr := blobStore.RemoveBlob(digest); blobErr != nil { // best-effort
				log.Printf("warning: remove unreferenced blob %s after tag removal: %v", digest, blobErr)
			}
		}
		fmt.Printf("Removed OCI image: %s\n", ociRef)
		if manifestDigest != "" {
			fmt.Printf("Manifest: %s\n", manifestDigest)
		}
		if len(zeroRefBlobs) > 0 {
			fmt.Printf("Collected %d unreferenced blob(s)\n", len(zeroRefBlobs))
		}
		return nil
	}

	// Fall back to cloud image removal.
	baseKey, err := resolveBaseKeyFromCache(c, app, ref)
	if err != nil {
		return err
	}

	if err := app.imgMgr.RemoveCached(c.Context, baseKey); err != nil {
		return fmt.Errorf("remove cached image: %w", err)
	}
	if idxErr := refcache.DeleteByBaseKey(app.cfg, baseKey); idxErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: clean manifest cache for %s: %v\n", baseKey, idxErr)
	}

	fmt.Printf("Removed: %s\n", baseKey)
	return nil
}

// resolveBaseKeyFromCache resolves an image reference to a base_key using only
// the local cache. It checks if ref matches a cached image's BaseKey directly.
// It never triggers a pull or conversion — making it safe for read-only
// operations like inspect and remove.
func resolveBaseKeyFromCache(c *cli.Context, app *appContext, ref string) (string, error) {
	images, err := app.imgMgr.ListCached(c.Context)
	if err != nil {
		return "", fmt.Errorf("list cached images: %w", err)
	}
	for _, img := range images {
		if img.BaseKey == ref {
			return ref, nil
		}
	}
	baseKey, ok, err := refcache.ResolveBaseKey(app.cfg, ref)
	if err != nil {
		return "", fmt.Errorf("resolve image ref from manifest cache %q: %w", ref, err)
	}
	if ok {
		for _, img := range images {
			if img.BaseKey == baseKey {
				return baseKey, nil
			}
		}
	}
	return "", fmt.Errorf("%w: %q", errImageNotFoundInLocalCache, ref)
}

func imageVerifyCommand() *cli.Command {
	return &cli.Command{
		Name:      "verify",
		Usage:     "Check if an image is bootable",
		ArgsUsage: "IMAGE_REF",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "format",
				Value: "table",
				Usage: "output format (table, json)",
			},
		},
		Action: imageVerifyAction,
	}
}

func imageVerifyAction(c *cli.Context) error {
	if c.NArg() < 1 {
		return fmt.Errorf("IMAGE_REF argument required\n\nUsage: cocoon image verify IMAGE_REF")
	}

	app, err := initApp(c)
	if err != nil {
		return err
	}

	arg := c.Args().Get(0)
	format := c.String("format")
	if fmtErr := validateOutputFormat(format); fmtErr != nil {
		return fmtErr
	}

	result, err := resolveVerifyResult(c, app, arg)
	if err != nil {
		return err
	}

	return renderVerifyResult(format, result)
}

func resolveVerifyResult(c *cli.Context, app *appContext, arg string) (*image.BootCheckResult, error) {
	// Local OCI tags are verified from their OCI layout metadata.
	store := oci.NewStore(app.cfg)
	ociRef, ociTagExists, err := resolveLocalOCITagRef(store, arg)
	if err != nil {
		return nil, fmt.Errorf("check OCI tag %q: %w", arg, err)
	}
	if ociTagExists {
		layoutPath, resolveErr := store.ResolveTag(ociRef)
		if resolveErr != nil {
			return nil, fmt.Errorf("resolve OCI tag %q: %w", ociRef, resolveErr)
		}
		result, verifyErr := verifyOCILayoutBootability(layoutPath)
		if verifyErr != nil {
			return nil, fmt.Errorf("verify OCI image %q: %w", ociRef, verifyErr)
		}
		return result, nil
	}

	imagePath, resolveErr := resolveVerifyImagePath(c, app, arg)
	if resolveErr != nil {
		return nil, resolveErr
	}
	result, verifyErr := app.imgMgr.VerifyBootability(c.Context, imagePath)
	if verifyErr != nil {
		return nil, fmt.Errorf("verify image: %w", verifyErr)
	}
	return result, nil
}

func renderVerifyResult(format string, result *image.BootCheckResult) error {
	// JSON output.
	if format == formatJSON {
		if err := printJSON(result); err != nil {
			return err
		}
		if !result.Bootable {
			return cli.Exit("", 1)
		}
		return nil
	}

	// Table-style summary (default).
	if result.Bootable {
		fmt.Println("Bootable: YES")
	} else {
		fmt.Println("Bootable: NO")
	}

	if len(result.BootModes) > 0 {
		fmt.Printf("Boot modes: %v\n", result.BootModes)
	}

	if len(result.Errors) > 0 {
		fmt.Println("\nErrors:")
		for _, e := range result.Errors {
			fmt.Printf("  - %s\n", e)
		}
	}

	if len(result.Warnings) > 0 {
		fmt.Println("\nWarnings:")
		for _, w := range result.Warnings {
			fmt.Printf("  - %s\n", w)
		}
	}

	if !result.Bootable {
		return cli.Exit("", 1)
	}

	return nil
}

func verifyOCILayoutBootability(layoutPath string) (*image.BootCheckResult, error) {
	info, err := oci.InspectLayout(layoutPath)
	if err != nil {
		return nil, fmt.Errorf("inspect OCI layout: %w", err)
	}
	return evaluateOCILayoutBootability(info), nil
}

func evaluateOCILayoutBootability(info *oci.LayoutInfo) *image.BootCheckResult {
	result := &image.BootCheckResult{
		Bootable:  false,
		BootModes: []string{},
		Errors:    []string{},
		Warnings:  []string{},
	}
	if info == nil {
		result.Errors = append(result.Errors, "OCI layout metadata is empty")
		return result
	}

	hasKernelLayer := false
	hasRootfsLayer := false
	for _, layer := range info.Layers {
		switch layer.MediaType {
		case oci.MediaTypeKernelLayer:
			hasKernelLayer = true
		case oci.MediaTypeRootfsLayer:
			hasRootfsLayer = true
		}
	}

	if hasKernelLayer {
		result.BootModes = append(result.BootModes, string(types.BootModeDirect))
		result.KernelChecked = true
		result.KernelFound = true
	}
	if !hasKernelLayer {
		result.Errors = append(result.Errors, "kernel layer not found in OCI manifest")
	}
	if !hasRootfsLayer {
		result.Errors = append(result.Errors, "rootfs layer not found in OCI manifest")
	}

	validateOCIVMConfig(result, info.Config)

	result.Bootable = len(result.Errors) == 0
	return result
}

func validateOCIVMConfig(result *image.BootCheckResult, cfg *oci.VMImageConfig) {
	if cfg == nil {
		result.Errors = append(result.Errors, "OCI VM config blob is missing")
		return
	}
	if strings.TrimSpace(cfg.KernelPath) == "" {
		result.Warnings = append(result.Warnings, "config.kernel_path is empty")
	}
	if strings.TrimSpace(cfg.InitrdPath) == "" {
		result.Warnings = append(result.Warnings, "config.initrd_path is empty")
	} else {
		result.InitrdChecked = true
		result.InitrdFound = true
	}
	if strings.TrimSpace(cfg.KernelCmdline) == "" {
		result.Warnings = append(result.Warnings, "config.kernel_cmdline is empty")
	}
}

func imageBuildCommand() *cli.Command {
	return &cli.Command{
		Name:      "build",
		Usage:     "Build an OCI VM image from a cloud image or Cocoonfile",
		ArgsUsage: "[CLOUD_IMAGE]",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "tag",
				Aliases: []string{"t"},
				Usage:   "OCI reference tag for the built image",
			},
			&cli.StringFlag{
				Name:    "file",
				Aliases: []string{"f"},
				Usage:   "path to Cocoonfile",
			},
		},
		Action: imageBuildAction,
	}
}

// resolveBuildImagePath determines the base image path for a build, handling
// both Cocoonfile-based and positional-argument-based invocations.
// It also returns a source string used for default tag derivation when --tag
// is omitted.
func resolveBuildImagePath(c *cli.Context, app *appContext, cocoonfilePath string) (string, string, func(), error) {
	if cocoonfilePath == "" {
		if c.NArg() < 1 {
			return "", "", nil, fmt.Errorf("CLOUD_IMAGE argument required (or use --file Cocoonfile)\n\nUsage: cocoon image build [CLOUD_IMAGE] [--tag REF] [--file Cocoonfile]")
		}
		source := strings.TrimSpace(c.Args().Get(0))
		imagePath, cleanup, err := resolveBuildPositionalSource(c, app, source)
		if err != nil {
			return "", "", nil, fmt.Errorf("resolve build source %q: %w", source, err)
		}
		return imagePath, source, cleanup, nil
	}

	// Positional arg overrides FROM if provided.
	if c.NArg() > 0 {
		source := strings.TrimSpace(c.Args().Get(0))
		imagePath, cleanup, err := resolveBuildPositionalSource(c, app, source)
		if err != nil {
			return "", "", nil, fmt.Errorf("resolve build source %q: %w", source, err)
		}
		return imagePath, source, cleanup, nil
	}

	cf, err := oci.ParseCocoonfile(cocoonfilePath)
	if err != nil {
		return "", "", nil, fmt.Errorf("parse Cocoonfile: %w", err)
	}

	imagePath, cleanup, err := resolveCocoonfileFrom(c, app, cocoonfilePath, cf.From)
	if err != nil {
		return "", "", nil, err
	}
	return imagePath, strings.TrimSpace(cf.From), cleanup, nil
}

func resolveBuildPositionalSource(c *cli.Context, app *appContext, source string) (string, func(), error) {
	if source == "" {
		return "", nil, fmt.Errorf("build source cannot be empty")
	}
	// Reuse Cocoonfile FROM resolution behavior with cwd as the base directory:
	// local path -> local OCI tag -> pullable ref/URL.
	cwd, err := os.Getwd()
	if err != nil {
		return "", nil, fmt.Errorf("get working directory: %w", err)
	}
	pseudoCocoonfilePath := filepath.Join(cwd, "Cocoonfile")
	return resolveCocoonfileFrom(c, app, pseudoCocoonfilePath, source)
}

func resolveCocoonfileFrom(c *cli.Context, app *appContext, cocoonfilePath, from string) (string, func(), error) {
	from = strings.TrimSpace(from)
	if from == "" {
		return "", nil, fmt.Errorf("cocoonfile FROM is empty")
	}

	// Explicit/existing local paths are always preferred.
	if imagePath, isPath := resolveCocoonfileLocalPath(cocoonfilePath, from); isPath {
		return imagePath, nil, nil
	}

	// Local OCI build tags take precedence over remote pulls.
	store := oci.NewStore(app.cfg)
	localOCITag, tagExists, err := resolveLocalOCITagRef(store, from)
	if err != nil {
		return "", nil, fmt.Errorf("check local OCI tag %q: %w", from, err)
	}
	if tagExists {
		return materializeOCITagForBuild(c.Context, app, store, localOCITag)
	}

	// Fallback: treat FROM as a pullable reference.
	// Docker-like behavior:
	// - "ubuntu:latest"           -> docker.io/library/ubuntu:latest
	// - "cmgs/myimg:latest"       -> docker.io/cmgs/myimg:latest
	// - "ghcr.io/ns/img:latest"   -> unchanged (explicit registry)
	// - "https://..."             -> unchanged (cloud image URL)
	pullRef := normalizePullableFROMRef(from)
	identity, basePath, err := app.imgMgr.Prepare(c.Context, pullRef)
	if err != nil {
		return "", nil, fmt.Errorf("resolve FROM %q via pull (%q): %w", from, pullRef, err)
	}
	if idxErr := refcache.Upsert(app.cfg, from, identity.BaseKey(), identity.FullDigest); idxErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: update manifest cache for FROM %q: %v\n", from, idxErr)
	}
	if pullRef != from {
		if idxErr := refcache.Upsert(app.cfg, pullRef, identity.BaseKey(), identity.FullDigest); idxErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: update manifest cache for FROM %q: %v\n", pullRef, idxErr)
		}
	}
	return basePath, nil, nil
}

func normalizePullableFROMRef(from string) string {
	ref := strings.TrimSpace(from)
	if ref == "" {
		return ref
	}
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return ref
	}
	return normalizeDockerLikeOCIRef(ref)
}

func normalizeDockerLikeOCIRef(ref string) string {
	namePart := ref
	digestPart := ""
	if idx := strings.Index(ref, "@"); idx >= 0 {
		namePart = ref[:idx]
		digestPart = ref[idx:]
	}

	var normalized string
	if strings.Count(namePart, "/") == 0 {
		normalized = "docker.io/library/" + namePart + digestPart
	} else {
		first, _, _ := strings.Cut(namePart, "/")
		if isExplicitRegistryHost(first) {
			normalized = ref
		} else {
			normalized = "docker.io/" + namePart + digestPart
		}
	}
	if digestPart != "" {
		return normalized
	}
	return ensureLatestTag(normalized)
}

func isExplicitRegistryHost(segment string) bool {
	return segment == "localhost" || strings.Contains(segment, ".") || strings.Contains(segment, ":")
}

func resolveCocoonfileLocalPath(cocoonfilePath, from string) (string, bool) {
	if filepath.IsAbs(from) {
		return from, true
	}
	baseDir := filepath.Dir(cocoonfilePath)
	if strings.HasPrefix(from, "./") || strings.HasPrefix(from, "../") {
		return filepath.Clean(filepath.Join(baseDir, from)), true
	}
	candidate := filepath.Join(baseDir, from)
	if _, err := os.Stat(candidate); err == nil {
		return candidate, true
	}
	return "", false
}

func materializeOCITagForBuild(ctx context.Context, app *appContext, store *oci.Store, tag string) (string, func(), error) {
	// Hold build txn lock for the full materialization so a concurrent
	// `image rm` cannot delete the layout mid-read.
	txnLock := flock.New(app.cfg.OCIBuildTxnLock())
	if err := txnLock.Lock(); err != nil {
		return "", nil, fmt.Errorf("acquire OCI build txn lock for FROM %q: %w", tag, err)
	}
	defer txnLock.Unlock() //nolint:errcheck

	entry, err := store.GetTag(tag)
	if err != nil {
		return "", nil, fmt.Errorf("read local OCI tag %q: %w", tag, err)
	}
	if _, statErr := os.Stat(entry.LayoutPath); statErr != nil {
		return "", nil, fmt.Errorf("local OCI layout missing at %s: %w", entry.LayoutPath, statErr)
	}

	info, err := oci.InspectLayout(entry.LayoutPath)
	if err != nil {
		return "", nil, fmt.Errorf("inspect local OCI layout %s: %w", entry.LayoutPath, err)
	}

	kernelLayer, rootfsLayers, err := locateLayerDescriptors(info)
	if err != nil {
		return "", nil, fmt.Errorf("resolve layers from local OCI tag %q: %w", tag, err)
	}

	workDir, err := os.MkdirTemp(app.cfg.TempDir(), "cocoon-build-from-oci-*")
	if err != nil {
		return "", nil, fmt.Errorf("create temp workspace for FROM %q: %w", tag, err)
	}
	cleanup := func() {
		_ = os.RemoveAll(workDir)
	}

	rootfsDir := filepath.Join(workDir, "rootfs")
	if mkErr := os.MkdirAll(rootfsDir, 0o755); mkErr != nil { //nolint:gosec // temporary local workspace
		cleanup()
		return "", nil, fmt.Errorf("create rootfs workspace: %w", mkErr)
	}

	for _, layer := range rootfsLayers {
		layerPath, layerErr := oci.LayoutBlobPath(entry.LayoutPath, layer.Digest)
		if layerErr != nil {
			cleanup()
			return "", nil, fmt.Errorf("resolve rootfs layer %s: %w", layer.Digest, layerErr)
		}
		if layerErr = extractTarLayer(ctx, layerPath, rootfsDir); layerErr != nil {
			cleanup()
			return "", nil, fmt.Errorf("extract rootfs layer %s: %w", layer.Digest, layerErr)
		}
	}

	kernelDir := filepath.Join(workDir, "kernel")
	if mkErr := os.MkdirAll(kernelDir, 0o755); mkErr != nil { //nolint:gosec // temporary local workspace
		cleanup()
		return "", nil, fmt.Errorf("create kernel workspace: %w", mkErr)
	}
	kernelLayerPath, err := oci.LayoutBlobPath(entry.LayoutPath, kernelLayer.Digest)
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("resolve kernel layer %s: %w", kernelLayer.Digest, err)
	}
	if err := extractTarLayer(ctx, kernelLayerPath, kernelDir); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("extract kernel layer %s: %w", kernelLayer.Digest, err)
	}

	bootDir := filepath.Join(rootfsDir, "boot")
	if err := os.MkdirAll(bootDir, 0o755); err != nil { //nolint:gosec // temporary local workspace
		cleanup()
		return "", nil, fmt.Errorf("create boot directory in materialized rootfs: %w", err)
	}

	if err := installKernelArtifacts(kernelDir, bootDir, info.Config); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("materialize kernel artifacts from local OCI tag %q: %w", tag, err)
	}

	baseImagePath := filepath.Join(workDir, "base.qcow2")
	if err := buildQcow2FromRootfs(ctx, rootfsDir, baseImagePath); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("materialize qcow2 from local OCI tag %q: %w", tag, err)
	}

	var baseCfg oci.VMImageConfig
	if info.Config != nil {
		baseCfg = *info.Config
	}
	buildCtx := &oci.BuildContext{
		SourceType:     oci.BuildSourceLocalOCITag,
		BaseTag:        tag,
		BaseLayoutPath: entry.LayoutPath,
		BaseConfig:     baseCfg,
		KernelLayer:    kernelLayer,
		RootfsLayers:   rootfsLayers,
		BaseRootfsDir:  rootfsDir,
	}
	if err := oci.WriteBuildContextForImage(baseImagePath, buildCtx); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("write build context for local OCI tag %q: %w", tag, err)
	}

	return baseImagePath, cleanup, nil
}

func locateLayerDescriptors(info *oci.LayoutInfo) (oci.LayerInfo, []oci.LayerInfo, error) {
	if info == nil {
		return oci.LayerInfo{}, nil, fmt.Errorf("layout info is nil")
	}
	var kernelLayer oci.LayerInfo
	rootfsLayers := make([]oci.LayerInfo, 0, len(info.Layers))
	for _, layer := range info.Layers {
		switch layer.MediaType {
		case oci.MediaTypeKernelLayer:
			if kernelLayer.Digest == "" {
				kernelLayer = layer
			}
		case oci.MediaTypeRootfsLayer:
			rootfsLayers = append(rootfsLayers, layer)
		}
	}
	if kernelLayer.Digest == "" {
		return oci.LayerInfo{}, nil, fmt.Errorf("kernel layer not found")
	}
	if len(rootfsLayers) == 0 {
		return oci.LayerInfo{}, nil, fmt.Errorf("rootfs layer not found")
	}
	return kernelLayer, rootfsLayers, nil
}

func extractTarLayer(ctx context.Context, layerPath, targetDir string) error {
	if err := utils.ExtractOCILayerTarToDir(ctx, layerPath, targetDir); err != nil {
		return fmt.Errorf("extract tar %s: %w", layerPath, err)
	}
	return nil
}

func installKernelArtifacts(kernelDir, bootDir string, cfg *oci.VMImageConfig) error {
	kernelCandidates := []string{"vmlinuz"}
	initrdCandidates := []string{"initrd.img"}
	if cfg != nil {
		if p := strings.TrimPrefix(strings.TrimSpace(cfg.KernelPath), "/"); p != "" {
			kernelCandidates = append([]string{filepath.FromSlash(p)}, kernelCandidates...)
		}
		if p := strings.TrimPrefix(strings.TrimSpace(cfg.InitrdPath), "/"); p != "" {
			initrdCandidates = append([]string{filepath.FromSlash(p)}, initrdCandidates...)
		}
	}

	kernelSrc, err := utils.FirstExistingPath(kernelDir, kernelCandidates)
	if err != nil {
		return fmt.Errorf("locate kernel in extracted layer: %w", err)
	}
	initrdSrc, err := utils.FirstExistingPath(kernelDir, initrdCandidates)
	if err != nil {
		return fmt.Errorf("locate initrd in extracted layer: %w", err)
	}

	if err := utils.CopyFile(kernelSrc, filepath.Join(bootDir, "vmlinuz"), 0o644); err != nil { //nolint:gosec // host-side temp file
		return fmt.Errorf("copy kernel into /boot: %w", err)
	}
	if err := utils.CopyFile(initrdSrc, filepath.Join(bootDir, "initrd.img"), 0o644); err != nil { //nolint:gosec // host-side temp file
		return fmt.Errorf("copy initrd into /boot: %w", err)
	}
	return nil
}

func buildQcow2FromRootfs(ctx context.Context, rootfsPath, outputPath string) error {
	if err := validateSafeBuildPath(rootfsPath); err != nil {
		return fmt.Errorf("invalid rootfs path: %w", err)
	}
	if err := validateSafeBuildPath(outputPath); err != nil {
		return fmt.Errorf("invalid output path: %w", err)
	}

	createCmd := exec.CommandContext(ctx, "qemu-img", "create", "-f", "qcow2", outputPath, "10G") //nolint:gosec // internal command invocation with validated local paths
	if out, err := createCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img create: %s: %w", strings.TrimSpace(string(out)), err)
	}

	rootfsTarPath := filepath.Join(filepath.Dir(outputPath), fmt.Sprintf(".rootfs-%d.tar", time.Now().UnixNano()))
	if err := validateSafeBuildPath(rootfsTarPath); err != nil {
		return fmt.Errorf("invalid rootfs tar path: %w", err)
	}
	if err := utils.PackDirectoryToTar(ctx, rootfsPath, rootfsTarPath); err != nil {
		return fmt.Errorf("pack rootfs tar: %w", err)
	}
	defer os.Remove(rootfsTarPath) //nolint:errcheck,gosec // best-effort cleanup of temporary file

	outputArg, err := quoteGuestfishPathArg(outputPath)
	if err != nil {
		return fmt.Errorf("quote output path for guestfish: %w", err)
	}
	rootfsTarArg, err := quoteGuestfishPathArg(rootfsTarPath)
	if err != nil {
		return fmt.Errorf("quote rootfs tar path for guestfish: %w", err)
	}

	script := fmt.Sprintf(`add %s
run
part-init /dev/sda gpt
part-add /dev/sda primary 2048 206847
part-set-gpt-type /dev/sda 1 C12A7328-F81F-11D2-BA4B-00A0C93EC93B
part-add /dev/sda primary 206848 -1
part-set-gpt-type /dev/sda 2 0FC63DAF-8483-4772-8E79-3D69D8477DE4
mkfs fat /dev/sda1
mkfs ext4 /dev/sda2
mount /dev/sda2 /
mkdir-p /boot/efi
mount /dev/sda1 /boot/efi
tar-in %s /
sync
umount-all
`, outputArg, rootfsTarArg)

	gfCmd := exec.CommandContext(ctx, "guestfish") //nolint:gosec // guestfish script references validated local paths
	gfCmd.Stdin = strings.NewReader(script)
	if out, err := gfCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("guestfish create qcow2: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func validateSafeBuildPath(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("path cannot be empty")
	}
	if strings.ContainsRune(path, '\x00') {
		return fmt.Errorf("path contains NUL byte")
	}
	if strings.ContainsAny(path, "\r\n") {
		return fmt.Errorf("path contains newline characters")
	}
	return nil
}

// quoteGuestfishPathArg returns a single-quoted guestfish script argument.
// We reject control characters and single quotes to keep script embedding safe.
func quoteGuestfishPathArg(path string) (string, error) {
	if err := validateSafeBuildPath(path); err != nil {
		return "", err
	}
	if strings.ContainsRune(path, '\'') {
		return "", fmt.Errorf("path contains unsupported single quote character")
	}
	return "'" + path + "'", nil
}

func defaultBuildTagSource(source string) string {
	source = strings.TrimSpace(source)
	if source == "" {
		return defaultDerivedBuildTag
	}
	source = strings.TrimSuffix(source, "/")
	if source == "" {
		return defaultDerivedBuildTag
	}
	last := source
	if idx := strings.LastIndex(source, "/"); idx >= 0 && idx+1 < len(source) {
		last = source[idx+1:]
	}
	if at := strings.Index(last, "@"); at >= 0 {
		last = last[:at]
	}
	if last == "" {
		last = defaultDerivedBuildTag
	}
	// For local files, strip extension before appending :latest.
	if !strings.Contains(last, ":") {
		if ext := filepath.Ext(last); ext != "" {
			last = strings.TrimSuffix(last, ext)
		}
		if last == "" {
			last = defaultDerivedBuildTag
		}
	}
	return last
}

// ensureLatestTag delegates to oci.EnsureLatestTag.
var ensureLatestTag = oci.EnsureLatestTag

// hasExplicitTagOrDigest delegates to oci.HasExplicitTagOrDigest.
var hasExplicitTagOrDigest = oci.HasExplicitTagOrDigest

// resolveLocalOCITagRef delegates to oci.ResolveLocalTagRef.
var resolveLocalOCITagRef = oci.ResolveLocalTagRef

func imageBuildAction(c *cli.Context) error {
	app, err := initApp(c)
	if err != nil {
		return err
	}

	cocoonfilePath := c.String("file")
	tag := c.String("tag")

	imagePath, tagSource, cleanup, err := resolveBuildImagePath(c, app, cocoonfilePath)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}

	// Default tag: filename without extension.
	if tag == "" {
		tag = defaultBuildTagSource(tagSource)
	}
	tag = ensureLatestTag(tag)

	result, err := app.imgBuild.Build(c.Context, imagePath, tag, cocoonfilePath)
	if err != nil {
		return fmt.Errorf("build OCI VM image: %w", err)
	}

	fmt.Printf("Built: %s\n", result.Tag)
	fmt.Printf("Kernel: %s\n", result.KernelVersion)
	fmt.Printf("Digest: %s\n", result.ManifestDigest)
	fmt.Printf("Layout: %s\n", result.LayoutPath)

	return nil
}

func imagePushCommand() *cli.Command {
	return &cli.Command{
		Name:      "push",
		Usage:     "Push a locally built OCI VM image to a container registry",
		ArgsUsage: "REF",
		Action:    imagePushAction,
	}
}

func imagePushAction(c *cli.Context) error {
	if c.NArg() < 1 {
		return fmt.Errorf("REF argument required\n\nUsage: cocoon image push REF")
	}

	app, err := initApp(c)
	if err != nil {
		return err
	}

	ref := strings.TrimSpace(c.Args().Get(0))
	if !hasExplicitTagOrDigest(ref) {
		ref = ensureLatestTag(ref)
	}

	result, err := app.imgBuild.Push(c.Context, ref)
	if err != nil {
		return fmt.Errorf("push OCI VM image: %w", err)
	}

	fmt.Printf("Pushed: %s\n", result.Ref)
	if result.ManifestDigest != "" {
		fmt.Printf("Digest: %s\n", result.ManifestDigest)
	}

	return nil
}

func imageLoginCommand() *cli.Command {
	return &cli.Command{
		Name:      "login",
		Usage:     "Log in to a container registry",
		ArgsUsage: "REGISTRY",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "username",
				Aliases: []string{"u"},
				Usage:   "registry username",
			},
			&cli.StringFlag{
				Name:    "password",
				Aliases: []string{"p"},
				Usage:   "registry password",
			},
		},
		Action: imageLoginAction,
	}
}

func imageLoginAction(c *cli.Context) error {
	if c.NArg() < 1 {
		return fmt.Errorf("REGISTRY argument required\n\nUsage: cocoon image login REGISTRY [-u USERNAME] [-p PASSWORD]")
	}

	registry := c.Args().Get(0)
	username := c.String("username")
	password := c.String("password")

	// Prompt for username if not provided.
	if username == "" {
		fmt.Print("Username: ")
		if _, err := fmt.Scanln(&username); err != nil {
			return fmt.Errorf("read username: %w", err)
		}
	}

	// Prompt for password if not provided.
	if password == "" {
		fmt.Print("Password: ")
		passwordBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
		if err != nil {
			return fmt.Errorf("read password: %w", err)
		}
		fmt.Println() // newline after hidden input
		password = string(passwordBytes)
	}

	app, err := initApp(c)
	if err != nil {
		return err
	}

	if err := app.imgBuild.Login(c.Context, registry, username, password); err != nil {
		return fmt.Errorf("login: %w", err)
	}

	fmt.Printf("Login succeeded for %s\n", registry)
	return nil
}

func ociEntrySize(layoutPath string) int64 {
	if layoutPath == "" {
		return 0
	}
	s, err := oci.LayoutSize(layoutPath)
	if err != nil {
		return 0
	}
	return s
}

func summarizeSourceRefs(refs []string) string {
	if len(refs) == 0 {
		return "-"
	}
	if len(refs) == 1 {
		return refs[0]
	}
	top := strings.Join(refs[:2], ", ")
	if len(refs) == 2 {
		return top
	}
	return fmt.Sprintf("%s (+%d)", top, len(refs)-2)
}

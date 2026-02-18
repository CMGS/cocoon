package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	cli "github.com/urfave/cli/v2"
	"golang.org/x/term"

	"github.com/CMGS/cocoon/image"
	"github.com/CMGS/cocoon/image/refcache"
	"github.com/CMGS/cocoon/oci"
	"github.com/CMGS/cocoon/types"
)

var errImageNotFoundInLocalCache = errors.New("image not found in local cache")

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

func imagePullAction(c *cli.Context) error {
	if c.NArg() < 1 {
		return fmt.Errorf("IMAGE_REF argument required\n\nUsage: cocoon image pull IMAGE_REF")
	}

	app, err := initApp(c)
	if err != nil {
		return err
	}

	ref := c.Args().Get(0)

	// Check if the image is already cached before pulling.
	_, wasCached := resolveBaseKeyFromCache(c, app, ref)

	// Use the Prepare pipeline to pull + convert + cache the image.
	identity, basePath, err := app.imgMgr.Prepare(c.Context, ref)
	if err != nil {
		return fmt.Errorf("pull image %q: %w", ref, err)
	}

	// Post-pull bootability verification.
	// Skip for already-cached images (verified on first pull).
	// VerifyBootability requires guestfish for deep checks (Linux-only).
	// On Darwin or when guestfish is unavailable, it falls back to basic
	// qcow2 validation with an optimistic result.
	if !c.Bool("skip-verify") && wasCached != nil {
		result, verifyErr := app.imgMgr.VerifyBootability(c.Context, basePath)
		if verifyErr != nil {
			return fmt.Errorf("verify bootability for %q: %w", ref, verifyErr)
		} else if !result.Bootable {
			if len(result.Errors) > 0 {
				return fmt.Errorf("%w: %s - %v", types.ErrImageNotBootable, ref, result.Errors)
			}
			return fmt.Errorf("%w: %s", types.ErrImageNotBootable, ref)
		}
		for _, w := range result.Warnings {
			fmt.Fprintf(os.Stderr, "Warning: image %s: %s\n", ref, w)
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
	if layoutPath, resolveErr := store.ResolveTag(ref); resolveErr == nil {
		return inspectOCIImage(ref, layoutPath)
	}

	// Fall back to cloud image cache.
	baseKey, err := resolveBaseKeyFromCache(c, app, ref)
	if err != nil {
		return fmt.Errorf("image not found: %q is not a local OCI build tag or cached cloud image", ref)
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
	sourceIsOCI, err := store.HasTag(sourceRef)
	if err != nil {
		return fmt.Errorf("check OCI source tag %q: %w", sourceRef, err)
	}
	if sourceIsOCI {
		return tagOCIImage(app, store, sourceRef, targetRef)
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
	targetIsOCI, err := store.HasTag(targetRef)
	if err != nil {
		return fmt.Errorf("check OCI target tag %q: %w", targetRef, err)
	}
	if targetIsOCI {
		return fmt.Errorf("target ref %q already exists as an OCI build tag; choose a different target ref", targetRef)
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
		Usage:     "Remove a cached image (only if unreferenced)",
		ArgsUsage: "IMAGE_REF",
		Action:    imageRemoveAction,
	}
}

func imageRemoveAction(c *cli.Context) error {
	if c.NArg() < 1 {
		return fmt.Errorf("IMAGE_REF argument required\n\nUsage: cocoon image remove IMAGE_REF")
	}

	app, err := initApp(c)
	if err != nil {
		return err
	}

	ref := c.Args().Get(0)

	// Try OCI image removal first.
	// Use tag-index existence instead of ResolveTag so we can clean stale tags
	// even when the layout directory has already been deleted.
	store := oci.NewStore(app.cfg)
	tagExists, tagErr := store.HasTag(ref)
	if tagErr != nil {
		return fmt.Errorf("check OCI tag %q: %w", ref, tagErr)
	}
	if tagExists {
		manifestDigest, zeroRefBlobs, removeErr := store.RemoveTag(ref)
		if removeErr != nil {
			return fmt.Errorf("remove OCI image: %w", removeErr)
		}
		// Remove zero-referenced blobs from shared store.
		blobStore := oci.NewBlobStore(app.cfg)
		for _, digest := range zeroRefBlobs {
			_ = blobStore.RemoveBlob(digest) // best-effort
		}
		fmt.Printf("Removed OCI image: %s\n", ref)
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

	result, err := resolveVerifyResult(c, app, arg)
	if err != nil {
		return err
	}

	return renderVerifyResult(format, result)
}

func resolveVerifyResult(c *cli.Context, app *appContext, arg string) (*image.BootCheckResult, error) {
	// Local OCI tags are verified from their OCI layout metadata.
	store := oci.NewStore(app.cfg)
	ociTagExists, err := store.HasTag(arg)
	if err != nil {
		return nil, fmt.Errorf("check OCI tag %q: %w", arg, err)
	}
	if ociTagExists {
		layoutPath, resolveErr := store.ResolveTag(arg)
		if resolveErr != nil {
			return nil, fmt.Errorf("resolve OCI tag %q: %w", arg, resolveErr)
		}
		result, verifyErr := verifyOCILayoutBootability(layoutPath)
		if verifyErr != nil {
			return nil, fmt.Errorf("verify OCI image %q: %w", arg, verifyErr)
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
func resolveBuildImagePath(c *cli.Context, cocoonfilePath string) (string, error) {
	if cocoonfilePath == "" {
		if c.NArg() < 1 {
			return "", fmt.Errorf("CLOUD_IMAGE argument required (or use --file Cocoonfile)\n\nUsage: cocoon image build [CLOUD_IMAGE] [--tag REF] [--file Cocoonfile]")
		}
		return c.Args().Get(0), nil
	}

	// Positional arg overrides FROM if provided.
	if c.NArg() > 0 {
		return c.Args().Get(0), nil
	}

	cf, err := oci.ParseCocoonfile(cocoonfilePath)
	if err != nil {
		return "", fmt.Errorf("parse Cocoonfile: %w", err)
	}
	imagePath := cf.From
	if !filepath.IsAbs(imagePath) {
		imagePath = filepath.Join(filepath.Dir(cocoonfilePath), imagePath)
	}
	return imagePath, nil
}

func imageBuildAction(c *cli.Context) error {
	app, err := initApp(c)
	if err != nil {
		return err
	}

	cocoonfilePath := c.String("file")
	tag := c.String("tag")

	imagePath, err := resolveBuildImagePath(c, cocoonfilePath)
	if err != nil {
		return err
	}

	// Default tag: filename without extension.
	if tag == "" {
		base := filepath.Base(imagePath)
		ext := filepath.Ext(base)
		tag = strings.TrimSuffix(base, ext)
		if tag == "" {
			tag = base
		}
	}

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

	ref := c.Args().Get(0)

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

package main

import (
	"fmt"
	"os"
	"strings"

	cli "github.com/urfave/cli/v2"

	"github.com/CMGS/cocoon/image/refcache"
	"github.com/CMGS/cocoon/types"
)

func imagesCommand() *cli.Command {
	return &cli.Command{
		Name:  "image",
		Usage: "Manage VM images",
		Subcommands: []*cli.Command{
			imageListCommand(),
			imagePullCommand(),
			imageInspectCommand(),
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

func imagesAction(c *cli.Context) error {
	app, err := initApp(c)
	if err != nil {
		return err
	}

	images, err := app.imgMgr.ListCached(c.Context)
	if err != nil {
		return fmt.Errorf("list cached images: %w", err)
	}

	format := c.String("format")
	rows := make([]imageListInfo, 0, len(images))
	for _, img := range images {
		sourceRefs, _, refsErr := refcache.RefsForBaseKey(app.cfg, img.BaseKey)
		if refsErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: read manifest cache for %s: %v\n", img.BaseKey, refsErr)
		}
		rows = append(rows, imageListInfo{
			BaseKey:    img.BaseKey,
			Size:       img.Size,
			RefCount:   img.RefCount,
			CachedAt:   img.CreatedAt.Format("2006-01-02T15:04:05Z"),
			SourceRef:  summarizeSourceRefs(sourceRefs),
			SourceRefs: sourceRefs,
		})
	}

	// JSON output.
	if format == formatJSON {
		return printJSON(rows)
	}

	// Table output (default).
	headers := []string{"BASE KEY", "SIZE", "REF COUNT", "SOURCE REF", "CACHED AT"}
	tableRows := make([][]string, 0, len(rows))
	for _, row := range rows {
		tableRows = append(tableRows, []string{
			row.BaseKey,
			humanBytes(row.Size),
			fmt.Sprintf("%d", row.RefCount),
			row.SourceRef,
			row.CachedAt,
		})
	}
	printTable(headers, tableRows)
	return nil
}

// imageListInfo is the CLI view model for image list output.
type imageListInfo struct {
	BaseKey    string   `json:"base_key"`
	Size       int64    `json:"size"`
	RefCount   int      `json:"ref_count"`
	SourceRef  string   `json:"source_ref,omitempty"`
	SourceRefs []string `json:"source_refs,omitempty"`
	CachedAt   string   `json:"cached_at"`
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

	// Use the Prepare pipeline to pull + convert + cache the image.
	identity, basePath, err := app.imgMgr.Prepare(c.Context, ref)
	if err != nil {
		return fmt.Errorf("pull image %q: %w", ref, err)
	}

	// Post-pull bootability verification.
	// VerifyBootability requires guestfish for deep checks (Linux-only).
	// On Darwin or when guestfish is unavailable, it falls back to basic
	// qcow2 validation with an optimistic result.
	if !c.Bool("skip-verify") {
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
		Usage:     "Show details of a cached image (size, checksum, ref count)",
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

	baseKey, err := resolveBaseKeyFromCache(c, app, ref)
	if err != nil {
		return err
	}

	// Look up the cached image.
	images, err := app.imgMgr.ListCached(c.Context)
	if err != nil {
		return fmt.Errorf("list cached images: %w", err)
	}

	var found *imageInspectInfo
	for _, img := range images {
		if img.BaseKey == baseKey {
			// Get actual ref count from reference counter.
			refs, refErr := app.refCtr.GetReferences(baseKey)
			refCount := img.RefCount
			if refErr == nil {
				refCount = len(refs)
			}

			found = &imageInspectInfo{
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
			break
		}
	}

	if found == nil {
		return fmt.Errorf("cached image not found: %s", baseKey)
	}

	return printJSON(found)
}

// imageInspectInfo holds detailed info for the image inspect command.
type imageInspectInfo struct {
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
	return "", fmt.Errorf("image %q not found in cache", ref)
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

	// If the argument is a local file path, use it directly.
	// Otherwise, prefer local cache resolution first to avoid network pulls.
	imagePath := arg
	if _, statErr := os.Stat(arg); statErr != nil {
		if baseKey, resolveErr := resolveBaseKeyFromCache(c, app, arg); resolveErr == nil {
			imagePath = app.cfg.BaseImagePath(baseKey)
		} else {
			_, basePath, prepErr := app.imgMgr.Prepare(c.Context, arg)
			if prepErr != nil {
				return fmt.Errorf("resolve image ref %q: %w", arg, prepErr)
			}
			imagePath = basePath
		}
	}

	result, err := app.imgMgr.VerifyBootability(c.Context, imagePath)
	if err != nil {
		return fmt.Errorf("verify image: %w", err)
	}

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

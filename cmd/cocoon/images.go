package main

import (
	"fmt"
	"os"

	cli "github.com/urfave/cli/v2"
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

	// JSON output.
	if format == formatJSON {
		return printJSON(images)
	}

	// Table output (default).
	headers := []string{"BASE KEY", "SIZE", "REF COUNT", "CACHED AT"}
	rows := make([][]string, 0, len(images))
	for _, img := range images {
		rows = append(rows, []string{
			img.BaseKey,
			humanBytes(img.Size),
			fmt.Sprintf("%d", img.RefCount),
			img.CreatedAt.Format("2006-01-02T15:04:05Z"),
		})
	}
	printTable(headers, rows)
	return nil
}

func imagePullCommand() *cli.Command {
	return &cli.Command{
		Name:      "pull",
		Usage:     "Pull and cache an image without creating a VM",
		ArgsUsage: "IMAGE_REF",
		Action:    imagePullAction,
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

	baseKey, err := resolveBaseKey(c, app, ref)
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
				BaseKey:   img.BaseKey,
				Path:      img.Path,
				Size:      img.Size,
				SizeHuman: humanBytes(img.Size),
				RefCount:  refCount,
				Refs:      refs,
				CachedAt:  img.CreatedAt.Format("2006-01-02T15:04:05Z"),
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
	BaseKey   string   `json:"base_key"`
	Path      string   `json:"path"`
	Size      int64    `json:"size"`
	SizeHuman string   `json:"size_human"`
	RefCount  int      `json:"ref_count"`
	Refs      []string `json:"refs,omitempty"`
	CachedAt  string   `json:"cached_at"`
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

	baseKey, err := resolveBaseKey(c, app, ref)
	if err != nil {
		return err
	}

	if err := app.imgMgr.RemoveCached(c.Context, baseKey); err != nil {
		return fmt.Errorf("remove cached image: %w", err)
	}

	fmt.Printf("Removed: %s\n", baseKey)
	return nil
}

// resolveBaseKey resolves an image reference to a base_key. It first checks if
// ref is already a valid base_key in the cache. If not, it treats ref as an
// image reference and resolves it via Prepare.
func resolveBaseKey(c *cli.Context, app *appContext, ref string) (string, error) {
	// Fast path: check if ref is already a valid base_key in cache.
	images, err := app.imgMgr.ListCached(c.Context)
	if err != nil {
		return "", fmt.Errorf("list cached images: %w", err)
	}
	for _, img := range images {
		if img.BaseKey == ref {
			return ref, nil
		}
	}

	// Slow path: treat ref as an image reference, resolve to base_key.
	identity, _, err := app.imgMgr.Prepare(c.Context, ref)
	if err != nil {
		return "", fmt.Errorf("resolve image ref %q: %w", ref, err)
	}
	return identity.BaseKey(), nil
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
	// Otherwise, treat it as an image reference and resolve to a cached path.
	imagePath := arg
	if _, statErr := os.Stat(arg); statErr != nil {
		_, basePath, prepErr := app.imgMgr.Prepare(c.Context, arg)
		if prepErr != nil {
			return fmt.Errorf("resolve image ref %q: %w", arg, prepErr)
		}
		imagePath = basePath
	}

	result, err := app.imgMgr.VerifyBootability(c.Context, imagePath)
	if err != nil {
		return fmt.Errorf("verify image: %w", err)
	}

	// JSON output.
	if format == formatJSON {
		return printJSON(result)
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

	return nil
}

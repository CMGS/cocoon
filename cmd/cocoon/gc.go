package main

import (
	"fmt"
	"time"

	cli "github.com/urfave/cli/v2"
)

func gcCommand() *cli.Command {
	return &cli.Command{
		Name:  "gc",
		Usage: "Run garbage collection on unreferenced images and orphaned resources",
		Flags: []cli.Flag{
			&cli.IntFlag{
				Name:  "grace-period",
				Usage: "hours before unreferenced images are collected (0 = use config default)",
			},
			&cli.BoolFlag{
				Name:  "dry-run",
				Usage: "only report what would be collected, don't actually delete",
			},
		},
		Action: gcAction,
	}
}

func gcAction(c *cli.Context) error {
	app, err := initApp(c)
	if err != nil {
		return err
	}

	gracePeriodHours := c.Int("grace-period")
	if gracePeriodHours == 0 {
		gracePeriodHours = app.cfg.GCGracePeriodHours
	}
	gracePeriod := time.Duration(gracePeriodHours) * time.Hour

	dryRun := c.Bool("dry-run")

	if dryRun {
		return gcDryRun(app, gracePeriod)
	}

	// Run full GC cycle with the specified grace period.
	// Phase 1: Collect unreferenced images.
	images, err := app.gc.CollectUnreferencedImages(gracePeriod)
	if err != nil {
		return fmt.Errorf("collect unreferenced images: %w", err)
	}
	for _, baseKey := range images {
		fmt.Printf("collected image: %s\n", baseKey)
	}

	// Phase 2: Collect orphaned overlays.
	overlays, err := app.gc.CollectOrphanedOverlays()
	if err != nil {
		return fmt.Errorf("collect orphaned overlays: %w", err)
	}
	for _, vmID := range overlays {
		fmt.Printf("collected orphaned overlay: %s\n", vmID)
	}

	// Phase 3: Collect temp files.
	tempFiles, err := app.gc.CollectTempFiles(1 * time.Hour)
	if err != nil {
		return fmt.Errorf("collect temp files: %w", err)
	}
	for _, name := range tempFiles {
		fmt.Printf("collected temp file: %s\n", name)
	}

	// Phase 4: Empty trash.
	trashRetention := time.Duration(app.cfg.GCTrashRetentDays) * 24 * time.Hour
	if err := app.gc.EmptyTrash(trashRetention); err != nil {
		return fmt.Errorf("empty trash: %w", err)
	}

	total := len(images) + len(overlays) + len(tempFiles)
	if total == 0 {
		fmt.Println("Nothing to collect.")
	} else {
		fmt.Printf("\nCollected %d item(s): %d images, %d overlays, %d temp files.\n",
			total, len(images), len(overlays), len(tempFiles))
	}

	return nil
}

// gcDryRun reports what would be collected without making changes.
// It uses the read-only methods from ReferenceCounter to identify candidates.
func gcDryRun(app *appContext, gracePeriod time.Duration) error {
	fmt.Printf("Dry run (grace period: %s)\n\n", gracePeriod)

	// Check for unreferenced images.
	unreferenced, err := app.refCtr.GetUnreferencedImages()
	if err != nil {
		return fmt.Errorf("get unreferenced images: %w", err)
	}
	if len(unreferenced) > 0 {
		fmt.Println("Unreferenced images (candidates for collection):")
		for _, baseKey := range unreferenced {
			fmt.Printf("  %s\n", baseKey)
		}
	} else {
		fmt.Println("No unreferenced images found.")
	}

	fmt.Printf("\nUse 'cocoon gc' without --dry-run to perform collection.\n")
	return nil
}

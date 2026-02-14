package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
				Name:  "aggressive",
				Usage: "collect unreferenced images immediately (alias for --grace-period 0)",
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
	if c.Bool("aggressive") {
		gracePeriodHours = 0
	} else if gracePeriodHours == 0 {
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

	// Phase 1 preview: unreferenced images that also passed grace period.
	imageCandidates, err := previewUnreferencedImageCandidates(app, gracePeriod)
	if err != nil {
		return fmt.Errorf("preview unreferenced images: %w", err)
	}
	if len(imageCandidates) > 0 {
		fmt.Println("Unreferenced images (candidates for collection):")
		for _, baseKey := range imageCandidates {
			fmt.Printf("  %s\n", baseKey)
		}
	} else {
		fmt.Println("No unreferenced images found.")
	}

	// Phase 2 preview: orphaned overlays.
	orphaned, err := previewOrphanedOverlayCandidates(app)
	if err != nil {
		return fmt.Errorf("preview orphaned overlays: %w", err)
	}
	if len(orphaned) > 0 {
		fmt.Println("\nOrphaned overlays (candidates for collection):")
		for _, vmID := range orphaned {
			fmt.Printf("  %s\n", vmID)
		}
	} else {
		fmt.Println("\nNo orphaned overlays found.")
	}

	// Phase 3 preview: old temp files (>1h).
	tempCandidates, err := previewOldFileCandidates(app.cfg.TempDir(), 1*time.Hour)
	if err != nil {
		return fmt.Errorf("preview temp files: %w", err)
	}
	if len(tempCandidates) > 0 {
		fmt.Println("\nTemp files (candidates for collection):")
		for _, name := range tempCandidates {
			fmt.Printf("  %s\n", name)
		}
	} else {
		fmt.Println("\nNo temp files found for collection.")
	}

	// Phase 4 preview: old trash items (>retention).
	trashRetention := time.Duration(app.cfg.GCTrashRetentDays) * 24 * time.Hour
	trashCandidates, err := previewOldFileCandidates(app.cfg.TrashDir(), trashRetention)
	if err != nil {
		return fmt.Errorf("preview trash cleanup: %w", err)
	}
	if len(trashCandidates) > 0 {
		fmt.Println("\nTrash items (candidates for permanent deletion):")
		for _, name := range trashCandidates {
			fmt.Printf("  %s\n", name)
		}
	} else {
		fmt.Println("\nNo trash items found for permanent deletion.")
	}

	fmt.Printf("\nUse 'cocoon gc' without --dry-run to perform collection.\n")
	return nil
}

func previewUnreferencedImageCandidates(app *appContext, gracePeriod time.Duration) ([]string, error) {
	unreferenced, err := app.refCtr.GetUnreferencedImages()
	if err != nil {
		return nil, err
	}

	cutoff := time.Now().Add(-gracePeriod)
	candidates := make([]string, 0, len(unreferenced))
	for _, baseKey := range unreferenced {
		fi, err := os.Stat(app.cfg.BaseImagePath(baseKey))
		if err != nil {
			continue
		}
		if fi.ModTime().After(cutoff) {
			continue
		}
		candidates = append(candidates, baseKey)
	}
	sort.Strings(candidates)
	return candidates, nil
}

func previewOrphanedOverlayCandidates(app *appContext) ([]string, error) {
	entries, err := os.ReadDir(app.cfg.VMDir())
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}

	candidates := make([]string, 0)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		vmID := entry.Name()
		overlayPath := filepath.Join(app.cfg.VMDir(), vmID, "overlay.qcow2")
		vmConfigPath := filepath.Join(app.cfg.VMDir(), vmID, "config.json")
		if _, err := os.Stat(overlayPath); err != nil {
			continue
		}
		if _, err := os.Stat(vmConfigPath); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			continue
		}
		candidates = append(candidates, vmID)
	}

	sort.Strings(candidates)
	return candidates, nil
}

func previewOldFileCandidates(dir string, maxAge time.Duration) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}

	cutoff := time.Now().Add(-maxAge)
	candidates := make([]string, 0)
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			candidates = append(candidates, entry.Name())
		}
	}

	sort.Strings(candidates)
	return candidates, nil
}

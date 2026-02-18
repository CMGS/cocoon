package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	cli "github.com/urfave/cli/v2"

	"github.com/CMGS/cocoon/oci"
)

func gcCommand() *cli.Command {
	return &cli.Command{
		Name:  "gc",
		Usage: "Run garbage collection on unreferenced images and orphaned resources",
		Flags: []cli.Flag{
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

	dryRun := c.Bool("dry-run")

	if dryRun {
		return gcDryRun(app)
	}

	// Phase 1: Collect unreferenced images.
	images, err := app.gc.CollectUnreferencedImages()
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

	// Phase 3: Collect orphaned OCI layouts.
	ociLayouts, err := app.gc.CollectOrphanedOCILayouts()
	if err != nil {
		return fmt.Errorf("collect orphaned OCI layouts: %w", err)
	}
	for _, name := range ociLayouts {
		fmt.Printf("collected orphaned OCI layout: %s\n", name)
	}

	// Phase 4: Collect stale OCI tags.
	staleTags, err := app.gc.CollectStaleOCITags()
	if err != nil {
		return fmt.Errorf("collect stale OCI tags: %w", err)
	}
	for _, tag := range staleTags {
		fmt.Printf("collected stale OCI tag: %s\n", tag)
	}

	// Phase 5: Collect orphaned OCI manifest refs.
	orphanedManifests, err := app.gc.CollectOrphanedOCIManifestRefs()
	if err != nil {
		return fmt.Errorf("collect orphaned OCI manifest refs: %w", err)
	}
	for _, digest := range orphanedManifests {
		fmt.Printf("collected orphaned OCI manifest ref: %s\n", digest)
	}

	// Phase 6: Collect unreferenced OCI blobs.
	ociBlobs, err := app.gc.CollectUnreferencedOCIBlobs()
	if err != nil {
		return fmt.Errorf("collect unreferenced OCI blobs: %w", err)
	}
	for _, digest := range ociBlobs {
		fmt.Printf("collected unreferenced OCI blob: %s\n", digest)
	}

	// Phase 7: Collect temp files.
	tempFiles, err := app.gc.CollectTempFiles(1 * time.Hour)
	if err != nil {
		return fmt.Errorf("collect temp files: %w", err)
	}
	for _, name := range tempFiles {
		fmt.Printf("collected temp file: %s\n", name)
	}

	total := len(images) + len(overlays) + len(ociLayouts) + len(staleTags) + len(orphanedManifests) + len(ociBlobs) + len(tempFiles)
	if total == 0 {
		fmt.Println("Nothing to collect.")
	} else {
		fmt.Printf("\nCollected %d item(s): %d images, %d overlays, %d OCI layouts, %d stale tags, %d orphaned manifests, %d OCI blobs, %d temp files.\n",
			total, len(images), len(overlays), len(ociLayouts), len(staleTags), len(orphanedManifests), len(ociBlobs), len(tempFiles))
	}

	return nil
}

// gcDryRun reports what would be collected without making changes.
func gcDryRun(app *appContext) error {
	fmt.Println("Dry run")

	// Phase 1 preview: unreferenced images.
	imageCandidates, err := previewUnreferencedImageCandidates(app)
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

	// Phase 3 preview: orphaned OCI layouts.
	ociLayoutCandidates, err := previewOrphanedOCILayoutCandidates(app)
	if err != nil {
		return fmt.Errorf("preview orphaned OCI layouts: %w", err)
	}
	if len(ociLayoutCandidates) > 0 {
		fmt.Println("\nOrphaned OCI layouts (candidates for collection):")
		for _, name := range ociLayoutCandidates {
			fmt.Printf("  %s\n", name)
		}
	} else {
		fmt.Println("\nNo orphaned OCI layouts found.")
	}

	// Phase 4 preview: stale OCI tags.
	staleTagCandidates, err := previewStaleOCITagCandidates(app)
	if err != nil {
		return fmt.Errorf("preview stale OCI tags: %w", err)
	}
	if len(staleTagCandidates) > 0 {
		fmt.Println("\nStale OCI tags (candidates for collection):")
		for _, tag := range staleTagCandidates {
			fmt.Printf("  %s\n", tag)
		}
	} else {
		fmt.Println("\nNo stale OCI tags found.")
	}

	// Phase 5 preview: orphaned OCI manifest refs.
	orphanedManifestCandidates, err := previewOrphanedOCIManifestRefCandidates(app)
	if err != nil {
		return fmt.Errorf("preview orphaned OCI manifest refs: %w", err)
	}
	if len(orphanedManifestCandidates) > 0 {
		fmt.Println("\nOrphaned OCI manifest refs (candidates for collection):")
		for _, digest := range orphanedManifestCandidates {
			fmt.Printf("  %s\n", digest)
		}
	} else {
		fmt.Println("\nNo orphaned OCI manifest refs found.")
	}

	// Phase 6 preview: unreferenced OCI blobs.
	ociBlobCandidates, err := previewUnreferencedOCIBlobCandidates(app)
	if err != nil {
		return fmt.Errorf("preview unreferenced OCI blobs: %w", err)
	}
	if len(ociBlobCandidates) > 0 {
		fmt.Println("\nUnreferenced OCI blobs (candidates for collection):")
		for _, digest := range ociBlobCandidates {
			fmt.Printf("  %s\n", digest)
		}
	} else {
		fmt.Println("\nNo unreferenced OCI blobs found.")
	}

	// Phase 7 preview: old temp files (>1h).
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

	fmt.Printf("\nUse 'cocoon gc' without --dry-run to perform collection.\n")
	return nil
}

func previewUnreferencedImageCandidates(app *appContext) ([]string, error) {
	unreferenced, err := app.refCtr.GetUnreferencedImages()
	if err != nil {
		return nil, err
	}

	candidates := make([]string, 0, len(unreferenced))
	for _, baseKey := range unreferenced {
		if _, err := os.Stat(app.cfg.BaseImagePath(baseKey)); err != nil {
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

// ociGCGracePeriod matches the 5-minute grace period used by the real GC
// in storage/local/gc_oci.go to avoid races with concurrent builds.
const ociGCGracePeriod = 5 * time.Minute

func previewOrphanedOCILayoutCandidates(app *appContext) ([]string, error) {
	layoutsDir := app.cfg.OCILayoutDir()
	entries, err := os.ReadDir(layoutsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}

	// Read tag index.
	store := oci.NewStore(app.cfg)
	tags, err := store.ListTags()
	if err != nil {
		return nil, err
	}

	knownLayouts := make(map[string]bool)
	for _, t := range tags {
		knownLayouts[t.LayoutPath] = true
	}

	cutoff := time.Now().Add(-ociGCGracePeriod)

	var candidates []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		layoutPath := filepath.Join(layoutsDir, entry.Name())
		if knownLayouts[layoutPath] {
			continue
		}
		// Apply the same 5-minute grace period as the real GC to avoid
		// reporting layouts that would actually be skipped.
		if info, statErr := entry.Info(); statErr == nil && info.ModTime().After(cutoff) {
			continue
		}
		candidates = append(candidates, entry.Name())
	}
	sort.Strings(candidates)
	return candidates, nil
}

func previewStaleOCITagCandidates(app *appContext) ([]string, error) {
	store := oci.NewStore(app.cfg)
	tags, err := store.ListTags()
	if err != nil {
		return nil, err
	}

	var candidates []string
	for _, t := range tags {
		if _, statErr := os.Stat(t.LayoutPath); os.IsNotExist(statErr) {
			candidates = append(candidates, t.Tag)
		}
	}
	sort.Strings(candidates)
	return candidates, nil
}

func previewOrphanedOCIManifestRefCandidates(app *appContext) ([]string, error) {
	store := oci.NewStore(app.cfg)
	tags, err := store.ListTags()
	if err != nil {
		return nil, err
	}

	// Build live manifest set.
	liveManifests := make(map[string]bool)
	for _, t := range tags {
		// Only count tags whose layout still exists.
		if _, statErr := os.Stat(t.LayoutPath); statErr == nil {
			liveManifests[t.ManifestDigest] = true
		}
	}

	// Get all manifest digests referenced in layer refs.
	allManifests, err := oci.GetAllManifestDigests(app.cfg)
	if err != nil {
		return nil, err
	}

	var candidates []string
	seen := make(map[string]bool)
	for _, md := range allManifests {
		if !liveManifests[md] && !seen[md] {
			candidates = append(candidates, md)
			seen[md] = true
		}
	}
	sort.Strings(candidates)
	return candidates, nil
}

func previewUnreferencedOCIBlobCandidates(app *appContext) ([]string, error) {
	blobDir := app.cfg.OCIBlobDir()
	entries, err := os.ReadDir(blobDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}

	// Get blobs tracked in layer refs that have zero manifest references.
	unreferenced, err := oci.GetUnreferencedBlobs(app.cfg)
	if err != nil {
		return nil, err
	}

	unreferencedSet := make(map[string]bool, len(unreferenced))
	for _, d := range unreferenced {
		unreferencedSet[d] = true
	}

	// Get all tracked blobs (those with refs) to identify untracked ones.
	trackedBlobs, err := oci.GetAllTrackedBlobs(app.cfg)
	if err != nil {
		return nil, err
	}
	trackedSet := make(map[string]bool, len(trackedBlobs))
	for _, d := range trackedBlobs {
		trackedSet[d] = true
	}

	cutoff := time.Now().Add(-ociGCGracePeriod)

	var candidates []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		digest := entry.Name()
		// Candidate if: in unreferenced set OR not tracked at all.
		if unreferencedSet[digest] || !trackedSet[digest] {
			// Apply the same 5-minute grace period as the real GC to avoid
			// reporting blobs that would actually be skipped.
			if info, statErr := entry.Info(); statErr == nil && info.ModTime().After(cutoff) {
				continue
			}
			candidates = append(candidates, digest)
		}
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

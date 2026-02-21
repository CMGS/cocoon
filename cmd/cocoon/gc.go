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

	"github.com/CMGS/cocoon/lock/flock"
	"github.com/CMGS/cocoon/oci"
	"github.com/CMGS/cocoon/storage"
)

const conversionLockGCMaxAge = storage.OCIGCGracePeriod

type conversionLockGC interface {
	CollectStaleConversionLocks(maxAge time.Duration) ([]string, error)
}

type ociRuntimeCacheGC interface {
	CollectUnreferencedOCIRuntimeCaches() ([]string, error)
}

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

// gcPhase describes a single GC collection phase.
type gcPhase struct {
	name    string
	collect func() ([]string, error)
}

func gcAction(c *cli.Context) error {
	app, err := initApp(c)
	if err != nil {
		return err
	}

	if c.Bool("dry-run") {
		return gcDryRun(app)
	}

	phases := buildGCPhases(app)

	var errs error
	total := 0
	counts := make([]int, len(phases))

	for i, phase := range phases {
		items, phaseErr := phase.collect()
		if phaseErr != nil {
			errs = errors.Join(errs, fmt.Errorf("phase %d (%s): %w", i+1, phase.name, phaseErr))
			continue
		}
		for _, item := range items {
			fmt.Printf("collected %s: %s\n", phase.name, item)
		}
		counts[i] = len(items)
		total += len(items)
	}

	if total == 0 {
		fmt.Println("Nothing to collect.")
	} else {
		parts := make([]string, 0, len(phases))
		for i, phase := range phases {
			parts = append(parts, fmt.Sprintf("%d %s", counts[i], phase.name))
		}
		fmt.Printf("\nCollected %d item(s): %s.\n", total, strings.Join(parts, ", "))
	}

	if errs != nil {
		return fmt.Errorf("gc completed with errors: %w", errs)
	}
	return nil
}

func buildGCPhases(app *appContext) []gcPhase {
	phases := []gcPhase{
		{"images", app.gc.CollectUnreferencedImages},
		{"overlays", app.gc.CollectOrphanedOverlays},
		{"OCI layouts", app.gc.CollectOrphanedOCILayouts},
		{"stale tags", app.gc.CollectStaleOCITags},
		{"orphaned manifests", app.gc.CollectOrphanedOCIManifestRefs},
		{"OCI blobs", app.gc.CollectUnreferencedOCIBlobs},
	}
	if runtimeGC, ok := app.gc.(ociRuntimeCacheGC); ok {
		phases = append(phases, gcPhase{"OCI runtime caches", runtimeGC.CollectUnreferencedOCIRuntimeCaches})
	}
	if lockGC, ok := app.gc.(conversionLockGC); ok {
		phases = append(phases, gcPhase{"stale locks", func() ([]string, error) {
			return lockGC.CollectStaleConversionLocks(conversionLockGCMaxAge)
		}})
	}
	phases = append(phases, gcPhase{"temp files", func() ([]string, error) {
		return app.gc.CollectTempFiles(1 * time.Hour)
	}})
	return phases
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

	// Phase 7 preview: unreferenced OCI runtime cache dirs.
	runtimeCacheCandidates, err := previewUnreferencedOCIRuntimeCacheCandidates(app)
	if err != nil {
		return fmt.Errorf("preview unreferenced OCI runtime caches: %w", err)
	}
	if len(runtimeCacheCandidates) > 0 {
		fmt.Println("\nUnreferenced OCI runtime caches (candidates for collection):")
		for _, runtimeKey := range runtimeCacheCandidates {
			fmt.Printf("  %s\n", runtimeKey)
		}
	} else {
		fmt.Println("\nNo unreferenced OCI runtime caches found.")
	}

	// Phase 8 preview: stale conversion lock files.
	if _, ok := app.gc.(conversionLockGC); ok {
		lockCandidates, lockErr := previewStaleConversionLockCandidates(app, conversionLockGCMaxAge)
		if lockErr != nil {
			return fmt.Errorf("preview stale conversion locks: %w", lockErr)
		}
		if len(lockCandidates) > 0 {
			fmt.Println("\nStale conversion lock files (candidates for collection):")
			for _, name := range lockCandidates {
				fmt.Printf("  %s\n", name)
			}
		} else {
			fmt.Println("\nNo stale conversion lock files found.")
		}
	} else {
		fmt.Println("\nStale conversion lock collection is not supported by this backend.")
	}

	// Phase 9 preview: old temp files (>1h).
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

	cutoff := time.Now().Add(-storage.OCIGCGracePeriod)

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

	cutoff := time.Now().Add(-storage.OCIGCGracePeriod)

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

func previewUnreferencedOCIRuntimeCacheCandidates(app *appContext) ([]string, error) {
	entries, err := os.ReadDir(app.cfg.OCIRuntimeCacheDir())
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}

	runtimeRefs, err := oci.LoadRuntimeRefsSnapshot(app.cfg)
	if err != nil {
		return nil, err
	}

	pinned := make(map[string]struct{}, len(runtimeRefs.Runtimes))
	for runtimeKey, entry := range runtimeRefs.Runtimes {
		if len(entry.Refs) == 0 {
			continue
		}
		pinned[runtimeKey] = struct{}{}
	}

	cutoff := time.Now().Add(-storage.OCIGCGracePeriod)
	candidates := make([]string, 0)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		runtimeKey := entry.Name()
		if _, ok := pinned[runtimeKey]; ok {
			continue
		}
		// Mirror real GC grace period.
		if info, statErr := entry.Info(); statErr == nil && info.ModTime().After(cutoff) {
			continue
		}
		candidates = append(candidates, runtimeKey)
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

func previewStaleConversionLockCandidates(app *appContext, maxAge time.Duration) ([]string, error) {
	lockDir := app.cfg.ConversionLockDir()
	entries, err := os.ReadDir(lockDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}

	cutoff := time.Now().Add(-maxAge)
	candidates := make([]string, 0)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".lock") {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}

		baseKey := strings.TrimSuffix(name, ".lock")
		if _, statErr := os.Stat(app.cfg.BaseImagePath(baseKey)); statErr == nil {
			continue
		} else if !os.IsNotExist(statErr) {
			continue
		}

		lockPath := filepath.Join(lockDir, name)
		fl := flock.New(lockPath)
		ok, tryErr := fl.TryLock()
		if tryErr != nil || !ok {
			continue
		}
		_ = fl.Unlock()
		candidates = append(candidates, name)
	}
	sort.Strings(candidates)
	return candidates, nil
}

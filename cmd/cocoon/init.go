package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	cli "github.com/urfave/cli/v2"

	"github.com/CMGS/cocoon/config"
)

func initCommand() *cli.Command {
	return &cli.Command{
		Name:  "init",
		Usage: "Initialize cocoon directories and default config",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "force",
				Usage: "overwrite existing config file and re-download firmware",
			},
			&cli.StringFlag{
				Name:  "with-pvh-firmware",
				Usage: "download PVH firmware from `URL` (e.g. https://github.com/cloud-hypervisor/rust-hypervisor-firmware/releases/download/0.4.2/hypervisor-fw)",
			},
			&cli.StringFlag{
				Name:  "with-uefi-firmware",
				Usage: "download UEFI firmware from `URL` (e.g. https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/v41.0/CLOUDHV.fd)",
			},
		},
		Action: initAction,
	}
}

func initAction(c *cli.Context) error {
	cfgPath := configPath // package-level var from main.go
	force := c.Bool("force")
	pvhURL := c.String("with-pvh-firmware")
	uefiURL := c.String("with-uefi-firmware")

	// Build config from defaults, then apply CLI overrides.
	cfg := config.DefaultConfig()
	if rootDir != "" {
		cfg.RootDir = rootDir
	}
	if runtimeDir != "" {
		cfg.RuntimeDir = runtimeDir
	}
	if logDir != "" {
		cfg.LogDir = logDir
	}

	// Create directory tree.
	fmt.Println("Creating directories...")
	if err := cfg.EnsureDirs(); err != nil {
		return fmt.Errorf("create directories: %w", err)
	}

	dirs := []string{
		cfg.DBDir(),
		cfg.CacheDir(),
		cfg.ImageCacheDir(),
		cfg.ManifestCacheDir(),
		cfg.ConversionLockDir(),
		cfg.BuildahRoot,
		cfg.VMDir(),
		cfg.TempDir(),
		cfg.TrashDir(),
		cfg.FirmwareDir(),
		cfg.RuntimeDir,
		cfg.LogDir,
	}
	for _, d := range dirs {
		fmt.Printf("  %s\n", d)
	}

	// Download firmware if requested.
	if pvhURL != "" {
		if err := downloadFirmware(pvhURL, cfg.PVHFirmwarePath, 0o755, force); err != nil {
			return fmt.Errorf("download PVH firmware: %w", err)
		}
	}
	if uefiURL != "" {
		if err := downloadFirmware(uefiURL, cfg.UEFIFirmwarePath, 0o644, force); err != nil {
			return fmt.Errorf("download UEFI firmware: %w", err)
		}
	}

	// Write default config file.
	if cfgPath == "" {
		cfgPath = "/etc/cocoon/config.json"
	}

	if _, err := os.Stat(cfgPath); err == nil && !force {
		fmt.Printf("\nConfig file already exists: %s (use --force to overwrite)\n", cfgPath)
	} else {
		if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil { //nolint:gosec // G301: config dir needs to be readable
			return fmt.Errorf("create config directory: %w", err)
		}

		data, marshalErr := json.MarshalIndent(cfg, "", "  ")
		if marshalErr != nil {
			return fmt.Errorf("marshal config: %w", marshalErr)
		}

		if err := os.WriteFile(cfgPath, data, 0o644); err != nil { //nolint:gosec // G306: config file should be readable
			return fmt.Errorf("write config: %w", err)
		}

		fmt.Printf("\nConfig written to %s\n", cfgPath)
	}

	fmt.Println("Done. Run 'cocoon doctor' to verify system dependencies.")
	return nil
}

// downloadFirmware downloads a file from url to destPath with the given permissions.
// If the file already exists and force is false, the download is skipped.
func downloadFirmware(url, destPath string, perm os.FileMode, force bool) error {
	if _, err := os.Stat(destPath); err == nil && !force {
		fmt.Printf("\nFirmware already exists: %s (use --force to re-download)\n", destPath)
		return nil
	}

	// Ensure parent directory exists.
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil { //nolint:gosec // G301: firmware dir needs to be accessible
		return fmt.Errorf("create firmware directory: %w", err)
	}

	fmt.Printf("\nDownloading %s\n  -> %s\n", url, destPath)

	resp, err := http.Get(url) //nolint:gosec,noctx // G107: URL comes from user-provided CLI flag
	if err != nil {
		return fmt.Errorf("HTTP GET: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected HTTP status: %s", resp.Status)
	}

	// Create a temp file in the same directory to allow atomic rename.
	tmpFile, err := os.CreateTemp(filepath.Dir(destPath), ".fw-download-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() {
		tmpFile.Close()
		os.Remove(tmpPath) //nolint:errcheck // best-effort cleanup
	}()

	// Copy with progress.
	written, err := io.Copy(tmpFile, &progressReader{
		reader: resp.Body,
		total:  resp.ContentLength,
	})
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	if err := os.Chmod(tmpPath, perm); err != nil {
		return fmt.Errorf("set permissions: %w", err)
	}

	if err := os.Rename(tmpPath, destPath); err != nil {
		return fmt.Errorf("move firmware into place: %w", err)
	}

	fmt.Printf("  Downloaded %d bytes -> %s\n", written, destPath)
	return nil
}

// progressReader wraps an io.Reader and prints download progress.
type progressReader struct {
	reader  io.Reader
	total   int64 // -1 if unknown
	current int64
	lastPct int
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.reader.Read(p)
	pr.current += int64(n)

	if pr.total > 0 {
		pct := int(pr.current * 100 / pr.total)
		// Print progress every 10%.
		if pct/10 > pr.lastPct/10 {
			fmt.Printf("  Progress: %d%%\n", pct)
			pr.lastPct = pct
		}
	}

	return n, err
}

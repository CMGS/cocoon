package main

import (
	"fmt"
	"log"
	"os"

	cli "github.com/urfave/cli/v2"
)

const defaultFirmwareURL = "https://github.com/cloud-hypervisor/edk2/releases/latest/download/CLOUDHV.fd"

func firmwareCommand() *cli.Command {
	return &cli.Command{
		Name:  "firmware",
		Usage: "Manage firmware files",
		Subcommands: []*cli.Command{
			firmwareListCommand(),
			firmwareVerifyCommand(),
			firmwareInstallCommand(),
			firmwareUpdateCommand(),
		},
	}
}

func firmwareListCommand() *cli.Command {
	return &cli.Command{
		Name:    "list",
		Aliases: []string{"ls"},
		Usage:   "List installed firmware files with paths and sizes",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "format",
				Value: "table",
				Usage: "output format (table, json)",
			},
		},
		Action: firmwareListAction,
	}
}

// firmwareEntry describes a single firmware file for display.
type firmwareEntry struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Exists bool   `json:"exists"`
}

func firmwareListAction(c *cli.Context) error {
	app, err := initApp(c)
	if err != nil {
		return err
	}

	format := c.String("format")
	entries := collectFirmwareEntries(app)

	// JSON output.
	if format == formatJSON {
		return printJSON(entries)
	}

	// Table output (default).
	headers := []string{"NAME", "TYPE", "PATH", "SIZE", "EXISTS"}
	rows := make([][]string, 0, len(entries))
	for _, e := range entries {
		sizeStr := "-"
		if e.Exists {
			sizeStr = humanBytes(e.Size)
		}
		rows = append(rows, []string{
			e.Name,
			e.Type,
			e.Path,
			sizeStr,
			fmt.Sprintf("%v", e.Exists),
		})
	}
	printTable(headers, rows)
	return nil
}

func firmwareVerifyCommand() *cli.Command {
	return &cli.Command{
		Name:   "verify",
		Usage:  "Check firmware files exist and are accessible",
		Action: firmwareVerifyAction,
	}
}

func firmwareVerifyAction(c *cli.Context) error {
	app, err := initApp(c)
	if err != nil {
		return err
	}

	entries := collectFirmwareEntries(app)

	allOK := true
	for _, e := range entries {
		if e.Path == "" {
			fmt.Printf("SKIP  %s (%s): not configured\n", e.Name, e.Type)
			continue
		}
		if e.Exists {
			fmt.Printf("OK    %s (%s): %s [%s]\n", e.Name, e.Type, e.Path, humanBytes(e.Size))
		} else {
			fmt.Printf("FAIL  %s (%s): %s (not found)\n", e.Name, e.Type, e.Path)
			allOK = false
		}
	}

	if !allOK {
		return fmt.Errorf("one or more firmware files are missing")
	}

	fmt.Println("\nAll firmware files verified.")
	return nil
}

func firmwareInstallCommand() *cli.Command {
	return &cli.Command{
		Name:  "install",
		Usage: "Download and install firmware files",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "uefi-url",
				Usage: "download UEFI firmware (CLOUDHV.fd) from `URL` (default: latest edk2 release)",
			},
			&cli.BoolFlag{
				Name:  "force",
				Usage: "re-download even if firmware files already exist",
			},
		},
		Action: firmwareInstallAction,
	}
}

func firmwareUpdateCommand() *cli.Command {
	return &cli.Command{
		Name:  "update",
		Usage: "Update firmware files (alias for install)",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "uefi-url",
				Usage: "download UEFI firmware (CLOUDHV.fd) from `URL` (default: latest edk2 release)",
			},
			&cli.BoolFlag{
				Name:  "force",
				Usage: "re-download even if firmware files already exist",
			},
		},
		Action: firmwareInstallAction,
	}
}

func firmwareInstallAction(c *cli.Context) error {
	uefiURL := c.String("uefi-url")
	force := c.Bool("force")

	if uefiURL == "" {
		uefiURL = defaultFirmwareURL
		log.Printf("No --uefi-url specified, using default: %s", uefiURL)
	}

	app, err := initApp(c)
	if err != nil {
		return err
	}

	if err := downloadFirmware(uefiURL, app.cfg.UEFIFirmwarePath, 0o644, force); err != nil {
		return fmt.Errorf("download UEFI firmware: %w", err)
	}

	fmt.Println("Firmware install complete.")
	return nil
}

// collectFirmwareEntries reads firmware paths from config and checks their existence.
func collectFirmwareEntries(app *appContext) []firmwareEntry {
	entries := []firmwareEntry{
		{
			Name: "CLOUDHV.fd",
			Type: "UEFI",
			Path: app.cfg.UEFIFirmwarePath,
		},
	}

	for i := range entries {
		if entries[i].Path == "" {
			continue
		}
		info, err := os.Stat(entries[i].Path)
		if err == nil {
			entries[i].Exists = true
			entries[i].Size = info.Size()
		}
	}

	return entries
}

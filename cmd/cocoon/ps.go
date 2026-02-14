package main

import (
	"fmt"
	"strings"

	cli "github.com/urfave/cli/v2"

	"github.com/CMGS/cocoon/types"
)

func psCommand() *cli.Command {
	return &cli.Command{
		Name:    "list",
		Aliases: []string{"ps", "ls"},
		Usage:   "List VMs",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:    "all",
				Aliases: []string{"a"},
				Usage:   "show all VMs (including stopped)",
			},
			&cli.StringFlag{
				Name:  "format",
				Value: "table",
				Usage: "output format (table, json)",
			},
			&cli.BoolFlag{
				Name:    "quiet",
				Aliases: []string{"q"},
				Usage:   "only display VM IDs",
			},
			&cli.StringFlag{
				Name:  "filter",
				Usage: "filter by field (e.g., state=running)",
			},
		},
		Action: psAction,
	}
}

func psAction(c *cli.Context) error {
	app, err := initApp(c)
	if err != nil {
		return err
	}

	vms, err := app.vmMgr.List(c.Context)
	if err != nil {
		return fmt.Errorf("list VMs: %w", err)
	}

	all := c.Bool("all")
	quiet := c.Bool("quiet")
	format := c.String("format")
	filter := c.String("filter")

	// Default behavior: show only RUNNING VMs.
	// Use --all to include non-running states.
	if !all {
		filtered := make([]*types.VMInspect, 0, len(vms))
		for _, v := range vms {
			if v.State == types.VMStateRunning {
				filtered = append(filtered, v)
			}
		}
		vms = filtered
	}

	// Apply --filter if specified. Supports "state=<value>" format.
	if filter != "" {
		vms, err = applyFilter(vms, filter)
		if err != nil {
			return err
		}
	}

	// Quiet mode: print only VM IDs, one per line.
	if quiet {
		for _, v := range vms {
			fmt.Println(v.VMID)
		}
		return nil
	}

	// JSON output.
	if format == formatJSON {
		return printJSON(vms)
	}

	// Table output (default).
	headers := []string{"VM ID", "NAME", "STATE", "CPUS", "MEMORY", "CREATED"}
	rows := make([][]string, 0, len(vms))
	for _, v := range vms {
		rows = append(rows, []string{
			v.VMID,
			v.Name,
			string(v.State),
			fmt.Sprintf("%d", v.BootConfig.CPUs),
			fmt.Sprintf("%dMB", v.BootConfig.MemoryMB),
			v.Timestamps.CreatedAt,
		})
	}
	printTable(headers, rows)
	return nil
}

// applyFilter filters the VM list based on a "key=value" expression.
// Supported keys: state, name.
func applyFilter(vms []*types.VMInspect, filter string) ([]*types.VMInspect, error) {
	parts := strings.SplitN(filter, "=", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid filter format %q; expected key=value (e.g., state=running)", filter)
	}

	key := strings.ToLower(strings.TrimSpace(parts[0]))
	value := strings.ToLower(strings.TrimSpace(parts[1]))

	filtered := make([]*types.VMInspect, 0, len(vms))
	for _, v := range vms {
		switch key {
		case "state":
			if strings.EqualFold(string(v.State), value) {
				filtered = append(filtered, v)
			}
		case "name":
			if strings.EqualFold(v.Name, value) {
				filtered = append(filtered, v)
			}
		default:
			return nil, fmt.Errorf("unsupported filter key %q; supported keys: state, name", key)
		}
	}

	return filtered, nil
}

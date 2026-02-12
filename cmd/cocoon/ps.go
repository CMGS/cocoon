package main

import (
	"fmt"

	cli "github.com/urfave/cli/v2"

	"github.com/CMGS/cocoon/types"
)

func psCommand() *cli.Command {
	return &cli.Command{
		Name:    "ps",
		Aliases: []string{"list", "ls"},
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

	// Filter out DELETED and ERROR states unless --all is specified.
	if !all {
		filtered := make([]*types.VMInspect, 0, len(vms))
		for _, v := range vms {
			if v.State != types.VMStateDeleted && v.State != types.VMStateError {
				filtered = append(filtered, v)
			}
		}
		vms = filtered
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

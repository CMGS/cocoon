package main

import (
	"fmt"

	cli "github.com/urfave/cli/v2"

	"github.com/CMGS/cocoon/types"
	"github.com/CMGS/cocoon/vm"
)

func runCommand() *cli.Command {
	return &cli.Command{
		Name:      "run",
		Aliases:   []string{"create"},
		Usage:     "Create and start a VM from an image",
		ArgsUsage: "IMAGE",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "name",
				Aliases: []string{"n"},
				Usage:   "VM name (globally unique; auto-generated if omitted)",
			},
			&cli.IntFlag{
				Name:    "cpus",
				Aliases: []string{"c"},
				Value:   types.DefaultCPUs,
				Usage:   "number of vCPUs",
			},
			&cli.Int64Flag{
				Name:    "memory",
				Aliases: []string{"m"},
				Value:   int64(types.DefaultMemoryMB),
				Usage:   "memory in MB",
			},
			&cli.StringFlag{
				Name:  "disk",
				Value: types.DefaultDiskSize,
				Usage: "root disk overlay size (e.g., 10G, 20G)",
			},
			&cli.StringFlag{
				Name:  "boot-strategy",
				Value: string(types.DefaultBootStrategy),
				Usage: "boot strategy: pvh_then_uefi, uefi_only, pvh_only",
			},
			&cli.BoolFlag{
				Name:    "detach",
				Aliases: []string{"d"},
				Usage:   "run VM in background",
			},
		},
		Action: runAction,
	}
}

func runAction(c *cli.Context) error {
	if c.NArg() < 1 {
		return fmt.Errorf("IMAGE argument required\n\nUsage: cocoon run IMAGE [flags]")
	}

	app, err := initApp(c)
	if err != nil {
		return err
	}

	opts := &vm.CreateOptions{
		Image:        c.Args().Get(0),
		Name:         c.String("name"),
		CPUs:         c.Int("cpus"),
		MemoryMB:     c.Int64("memory"),
		DiskSize:     c.String("disk"),
		BootStrategy: types.BootStrategy(c.String("boot-strategy")),
	}

	vmCfg, err := app.vmMgr.Create(c.Context, opts)
	if err != nil {
		return fmt.Errorf("create VM: %w", err)
	}

	fmt.Printf("%s\n", vmCfg.VMID)

	// If not --detach, also start the VM.
	if !c.Bool("detach") {
		if err := app.vmMgr.Start(c.Context, vmCfg.VMID); err != nil {
			return fmt.Errorf("start VM: %w", err)
		}
		// Full attach mode (follow serial log) is Phase 2.
	}

	return nil
}

package main

import (
	"fmt"

	cli "github.com/urfave/cli/v2"

	"github.com/CMGS/cocoon/types"
)

func runCommand() *cli.Command {
	flags := vmCreateFlags()
	flags = append(flags,
		&cli.BoolFlag{
			Name:    "detach",
			Aliases: []string{"d"},
			Usage:   "run VM in background",
		},
		&cli.BoolFlag{
			Name:  "rm",
			Usage: "automatically delete the VM when it stops",
		},
	)
	return &cli.Command{
		Name:      "run",
		Usage:     "Create and start a VM from an image",
		ArgsUsage: "IMAGE",
		Flags:     flags,
		Action:    runAction,
	}
}

func runAction(c *cli.Context) error {
	opts, err := parseCreateOptions(c, "run")
	if err != nil {
		return err
	}

	app, err := initApp(c)
	if err != nil {
		return err
	}

	vmCfg, err := app.vmMgr.Create(c.Context, opts)
	if err != nil {
		return fmt.Errorf("create VM: %w", err)
	}

	fmt.Printf("%s\n", vmCfg.VMID)

	autoRemove := c.Bool("rm")

	// Start the VM. In Phase 1, both detach and non-detach modes start
	// the VM as a background CH process. Phase 2 will add attach mode
	// (follow serial log) for non-detach runs.
	if err := app.vmMgr.Start(c.Context, vmCfg.VMID); err != nil {
		return fmt.Errorf("start VM: %w", err)
	}

	// If --rm is set, record it in metadata so the stop command (or a future
	// lifecycle hook) can auto-delete the VM after it stops.
	if autoRemove {
		if err := app.vmMgr.UpdateMetadata(vmCfg.VMID, func(md *types.VMMetadataFile) {
			md.AutoRemove = true
		}); err != nil {
			return fmt.Errorf("set auto-remove for %s: %w", vmCfg.VMID, err)
		}
	}

	return nil
}

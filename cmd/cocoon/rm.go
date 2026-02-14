package main

import (
	"errors"
	"fmt"

	cli "github.com/urfave/cli/v2"

	"github.com/CMGS/cocoon/types"
)

func rmCommand() *cli.Command {
	return &cli.Command{
		Name:      "delete",
		Aliases:   []string{"rm"},
		Usage:     "Remove a VM and cleanup storage",
		ArgsUsage: "VM_REF",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:    "force",
				Aliases: []string{"f"},
				Usage:   "force delete even if VM is running",
			},
		},
		Action: rmAction,
	}
}

func rmAction(c *cli.Context) error {
	if c.NArg() < 1 {
		return fmt.Errorf("VM_REF argument required\n\nUsage: cocoon delete VM_REF [flags]")
	}

	app, err := initApp(c)
	if err != nil {
		return err
	}

	ref := c.Args().Get(0)
	vmID, err := app.vmMgr.ResolveVMRef(ref)
	if err != nil {
		if errors.Is(err, types.ErrVMNotFound) {
			return nil // idempotent: VM already gone
		}
		return fmt.Errorf("resolve VM ref %q: %w", ref, err)
	}

	force := c.Bool("force")
	if err := app.vmMgr.Delete(c.Context, vmID, force); err != nil {
		if errors.Is(err, types.ErrVMNotFound) {
			return nil // idempotent: VM disappeared between resolve and delete
		}
		return fmt.Errorf("delete VM %s: %w", vmID, err)
	}

	fmt.Println(vmID)
	return nil
}

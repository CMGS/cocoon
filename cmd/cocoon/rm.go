package main

import (
	"errors"
	"fmt"
	"strings"

	cli "github.com/urfave/cli/v2"

	"github.com/CMGS/cocoon/types"
)

func rmCommand() *cli.Command {
	return &cli.Command{
		Name:      "delete",
		Aliases:   []string{"rm"},
		Usage:     "Remove one or more VMs and cleanup storage",
		ArgsUsage: "VM_REF [VM_REF...]",
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
		return fmt.Errorf("VM_REF argument required\n\nUsage: cocoon delete VM_REF [VM_REF...] [flags]")
	}

	app, err := initApp(c)
	if err != nil {
		return err
	}

	force := c.Bool("force")
	var errs []string

	for i := 0; i < c.NArg(); i++ {
		ref := c.Args().Get(i)
		vmID, err := app.vmMgr.ResolveVMRef(ref)
		if err != nil {
			if errors.Is(err, types.ErrVMNotFound) {
				continue // idempotent: VM already gone
			}
			errs = append(errs, fmt.Sprintf("%s: %v", ref, err))
			continue
		}

		if err := app.vmMgr.Delete(c.Context, vmID, force); err != nil {
			if errors.Is(err, types.ErrVMNotFound) {
				continue // idempotent: VM disappeared between resolve and delete
			}
			errs = append(errs, fmt.Sprintf("%s: %v", vmID, err))
			continue
		}

		fmt.Println(vmID)
	}

	if len(errs) > 0 {
		return fmt.Errorf("delete failed for %d VM(s):\n  %s", len(errs), strings.Join(errs, "\n  "))
	}
	return nil
}

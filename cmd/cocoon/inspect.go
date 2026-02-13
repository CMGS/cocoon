package main

import (
	"fmt"

	cli "github.com/urfave/cli/v2"
)

func inspectCommand() *cli.Command {
	return &cli.Command{
		Name:      "inspect",
		Usage:     "Display detailed VM information",
		ArgsUsage: "VM_REF",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "format",
				Value: "json",
				Usage: "output format (json)",
			},
		},
		Action: inspectAction,
	}
}

func inspectAction(c *cli.Context) error {
	if c.NArg() < 1 {
		return fmt.Errorf("VM_REF argument required\n\nUsage: cocoon inspect VM_REF [flags]")
	}

	app, err := initApp(c)
	if err != nil {
		return err
	}

	ref := c.Args().Get(0)
	vmID, err := app.vmMgr.ResolveVMRef(ref)
	if err != nil {
		return fmt.Errorf("resolve VM ref %q: %w", ref, err)
	}

	info, err := app.vmMgr.Inspect(c.Context, vmID)
	if err != nil {
		return fmt.Errorf("inspect VM: %w", err)
	}

	return printJSON(info)
}

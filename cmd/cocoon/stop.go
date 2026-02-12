package main

import (
	"fmt"
	"time"

	cli "github.com/urfave/cli/v2"
)

func stopCommand() *cli.Command {
	return &cli.Command{
		Name:      "stop",
		Usage:     "Stop a running VM",
		ArgsUsage: "VM_REF",
		Flags: []cli.Flag{
			&cli.DurationFlag{
				Name:  "timeout",
				Value: 30 * time.Second,
				Usage: "graceful shutdown timeout",
			},
		},
		Action: stopAction,
	}
}

func stopAction(c *cli.Context) error {
	if c.NArg() < 1 {
		return fmt.Errorf("VM_REF argument required\n\nUsage: cocoon stop VM_REF [flags]")
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

	timeout := c.Duration("timeout")
	if err := app.vmMgr.Stop(c.Context, vmID, timeout); err != nil {
		return fmt.Errorf("stop VM: %w", err)
	}

	fmt.Println(vmID)
	return nil
}

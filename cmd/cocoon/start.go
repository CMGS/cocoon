package main

import (
	"fmt"

	cli "github.com/urfave/cli/v2"
)

func startCommand() *cli.Command {
	return &cli.Command{
		Name:      "start",
		Usage:     "Start a stopped VM",
		ArgsUsage: "VM_REF",
		Flags: []cli.Flag{
			&cli.IntFlag{
				Name:  "boot-timeout",
				Usage: "seconds to wait for VM to boot (0 = use config default)",
			},
		},
		Action: startAction,
	}
}

func startAction(c *cli.Context) error {
	if c.NArg() < 1 {
		return fmt.Errorf("VM_REF argument required\n\nUsage: cocoon start VM_REF [flags]")
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

	// Override config boot timeout if --boot-timeout flag is specified.
	bootTimeout := c.Int("boot-timeout")
	if bootTimeout > 0 {
		app.cfg.BootTimeoutSeconds = bootTimeout
	}

	if err := app.vmMgr.Start(c.Context, vmID); err != nil {
		return fmt.Errorf("start VM: %w", err)
	}

	fmt.Println(vmID)
	return nil
}

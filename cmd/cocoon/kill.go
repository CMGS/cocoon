package main

import (
	"fmt"

	cli "github.com/urfave/cli/v2"
)

func killCommand() *cli.Command {
	return &cli.Command{
		Name:      "kill",
		Usage:     "Force-terminate a VM immediately (SIGKILL)",
		ArgsUsage: "VM_REF",
		Action:    killAction,
	}
}

func killAction(c *cli.Context) error {
	if c.NArg() < 1 {
		return fmt.Errorf("VM_REF argument required\n\nUsage: cocoon kill VM_REF")
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

	// Force kill via Manager.Kill which handles state transitions,
	// metadata cleanup (ProcessPID=0, StoppedAt), and the state machine.
	if err := app.vmMgr.Kill(c.Context, vmID); err != nil {
		return fmt.Errorf("kill VM %s: %w", vmID, err)
	}

	fmt.Println(vmID)
	return nil
}

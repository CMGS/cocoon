package main

import (
	"fmt"

	cli "github.com/urfave/cli/v2"

	"github.com/CMGS/cocoon/types"
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

	// Force kill the CH process immediately via SIGKILL.
	if err := app.hyper.ForceKill(vmID); err != nil {
		return fmt.Errorf("force kill VM %s: %w", vmID, err)
	}

	// Transition state to STOPPED following the valid state machine.
	// RUNNING  -> STOPPING -> STOPPED
	// STOPPING -> STOPPED
	// STARTING -> ERROR (cannot go to STOPPING)
	meta, metaErr := app.vmMgr.LoadMetadata(vmID)
	if metaErr == nil {
		state := types.VMState(meta.State)
		switch state {
		case types.VMStateRunning:
			_ = app.vmMgr.TransitionState(vmID, types.VMStateStopping, "force killed via cocoon kill")
			_ = app.vmMgr.TransitionState(vmID, types.VMStateStopped, "force killed via cocoon kill")
		case types.VMStateStopping:
			_ = app.vmMgr.TransitionState(vmID, types.VMStateStopped, "force killed via cocoon kill")
		case types.VMStateStarting:
			_ = app.vmMgr.TransitionState(vmID, types.VMStateError, "force killed via cocoon kill")
		}
	}

	fmt.Println(vmID)
	return nil
}

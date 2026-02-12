package main

import (
	"fmt"
	"strconv"
	"strings"

	cli "github.com/urfave/cli/v2"

	"github.com/CMGS/cocoon/types"
	"github.com/CMGS/cocoon/vm"
)

// vmCreateFlags returns the common flags shared by both create and run commands.
func vmCreateFlags() []cli.Flag {
	return []cli.Flag{
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
		&cli.StringFlag{
			Name:    "memory",
			Aliases: []string{"m"},
			Value:   fmt.Sprintf("%dM", types.DefaultMemoryMB),
			Usage:   "memory size (e.g., 512M, 1G, 2G, 2048)",
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
	}
}

func createCommand() *cli.Command {
	return &cli.Command{
		Name:      "create",
		Usage:     "Create a VM from an image without starting it",
		ArgsUsage: "IMAGE",
		Flags:     vmCreateFlags(),
		Action:    createAction,
	}
}

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

// parseCreateOptions builds CreateOptions from CLI flags, including
// human-readable memory string parsing.
func parseCreateOptions(c *cli.Context, cmdName string) (*vm.CreateOptions, error) {
	if c.NArg() < 1 {
		return nil, fmt.Errorf("IMAGE argument required\n\nUsage: cocoon %s IMAGE [flags]", cmdName)
	}

	memoryMB, err := parseMemory(c.String("memory"))
	if err != nil {
		return nil, fmt.Errorf("invalid --memory value: %w", err)
	}

	return &vm.CreateOptions{
		Image:        c.Args().Get(0),
		Name:         c.String("name"),
		CPUs:         c.Int("cpus"),
		MemoryMB:     memoryMB,
		DiskSize:     c.String("disk"),
		BootStrategy: types.BootStrategy(c.String("boot-strategy")),
	}, nil
}

func createAction(c *cli.Context) error {
	opts, err := parseCreateOptions(c, "create")
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
	return nil
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

	// If not --detach, also start the VM.
	if !c.Bool("detach") {
		if err := app.vmMgr.Start(c.Context, vmCfg.VMID); err != nil {
			return fmt.Errorf("start VM: %w", err)
		}
		// Full attach mode (follow serial log) is Phase 2.
	}

	// If --rm is set, record it in metadata so the stop command (or a future
	// lifecycle hook) can auto-delete the VM after it stops.
	if autoRemove {
		meta, metaErr := app.vmMgr.LoadMetadata(vmCfg.VMID)
		if metaErr == nil {
			meta.AutoRemove = true
			_ = app.vmMgr.SaveMetadata(meta)
		}
	}

	return nil
}

// parseMemory parses a human-readable memory string and returns the value in MB.
// Accepted formats:
//   - "512M" or "512m" -> 512
//   - "1G" or "1g"     -> 1024
//   - "2048"           -> 2048 (plain number = MB)
func parseMemory(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty memory value")
	}

	// Check for suffix.
	last := s[len(s)-1]
	switch last {
	case 'M', 'm':
		n, err := strconv.ParseInt(s[:len(s)-1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid memory value %q: %w", s, err)
		}
		if n <= 0 {
			return 0, fmt.Errorf("memory must be positive, got %d", n)
		}
		return n, nil
	case 'G', 'g':
		n, err := strconv.ParseInt(s[:len(s)-1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid memory value %q: %w", s, err)
		}
		if n <= 0 {
			return 0, fmt.Errorf("memory must be positive, got %d", n)
		}
		return n * 1024, nil
	default:
		// Plain number: interpret as MB.
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid memory value %q: expected a number with optional M/G suffix", s)
		}
		if n <= 0 {
			return 0, fmt.Errorf("memory must be positive, got %d", n)
		}
		return n, nil
	}
}

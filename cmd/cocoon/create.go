package main

import (
	"fmt"

	"github.com/docker/go-units"
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
		&cli.BoolFlag{
			Name:  "skip-verify",
			Usage: "skip bootability verification of the image",
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
		SkipVerify:   c.Bool("skip-verify"),
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

// parseMemory parses a human-readable memory string and returns the value in MB.
// Delegates to docker/go-units for robust parsing (supports K, M, G, T, Ki, Mi, Gi, etc.).
// Plain numbers without suffix are interpreted as MB for backwards compatibility.
func parseMemory(s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("empty memory value")
	}

	bytes, err := units.RAMInBytes(s)
	if err != nil {
		return 0, fmt.Errorf("invalid memory value %q: %w", s, err)
	}
	if bytes <= 0 {
		return 0, fmt.Errorf("memory must be positive, got %d bytes", bytes)
	}

	mb := bytes / (1024 * 1024)
	if mb == 0 {
		return 0, fmt.Errorf("memory value %q is less than 1MB", s)
	}
	return mb, nil
}

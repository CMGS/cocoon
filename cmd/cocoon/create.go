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
			Usage: "boot strategy: pvh (default), uefi",
		},
		&cli.BoolFlag{
			Name:  "skip-verify",
			Usage: "skip bootability verification of the image",
		},
		&cli.IntFlag{
			Name:  "boot-timeout",
			Usage: "boot detection timeout in seconds (0 = skip boot detection)",
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

	bootStrategy, err := types.ParseBootStrategy(c.String("boot-strategy"))
	if err != nil {
		return nil, fmt.Errorf("invalid --boot-strategy value: %w", err)
	}

	cpus := c.Int("cpus")
	if cpus < 1 || cpus > 256 {
		return nil, fmt.Errorf("invalid --cpus value %d: must be between 1 and 256", cpus)
	}

	memoryMB, err := parseMemory(c.String("memory"))
	if err != nil {
		return nil, fmt.Errorf("invalid --memory value: %w", err)
	}

	diskSize := c.String("disk")
	if diskSize != "" {
		diskBytes, diskErr := units.RAMInBytes(diskSize)
		if diskErr != nil {
			return nil, fmt.Errorf("invalid --disk value %q: %w", diskSize, diskErr)
		}
		if diskBytes < 1*1024*1024*1024 { // minimum 1GB
			return nil, fmt.Errorf("invalid --disk value %q: minimum disk size is 1G", diskSize)
		}
	}

	return &vm.CreateOptions{
		Image:        c.Args().Get(0),
		Name:         c.String("name"),
		CPUs:         cpus,
		MemoryMB:     memoryMB,
		DiskSize:     diskSize,
		BootStrategy: bootStrategy,
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
	// Plain numbers without suffix are interpreted as MB (docs/09 spec).
	if isAllDigits(s) {
		s += "M"
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

func isAllDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}

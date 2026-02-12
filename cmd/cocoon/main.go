package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	cli "github.com/urfave/cli/v2"

	"github.com/CMGS/cocoon/version"
)

var (
	configPath string
	rootDir    string
	runtimeDir string
	logDir     string
	logLevel   string
)

func main() {
	cli.VersionPrinter = func(_ *cli.Context) {
		fmt.Print(version.String())
	}

	app := cli.NewApp()
	app.Name = version.NAME
	app.Usage = "Lightweight VM manager built on Cloud Hypervisor"
	app.Version = version.VERSION
	app.Flags = []cli.Flag{
		&cli.StringFlag{
			Name:        "config",
			Value:       "/etc/cocoon/config.json",
			Usage:       "config file path for cocoon",
			Destination: &configPath,
			EnvVars:     []string{"COCOON_CONFIG_PATH"},
		},
		&cli.StringFlag{
			Name:        "root-dir",
			Usage:       "root directory for cocoon persistent data (overrides config)",
			Destination: &rootDir,
			EnvVars:     []string{"COCOON_ROOT_DIR"},
		},
		&cli.StringFlag{
			Name:        "runtime-dir",
			Usage:       "runtime directory for sockets and PIDs (overrides config)",
			Destination: &runtimeDir,
			EnvVars:     []string{"COCOON_RUNTIME_DIR"},
		},
		&cli.StringFlag{
			Name:        "log-dir",
			Usage:       "log directory for VM serial logs (overrides config)",
			Destination: &logDir,
			EnvVars:     []string{"COCOON_LOG_DIR"},
		},
		&cli.StringFlag{
			Name:        "log-level",
			Value:       "info",
			Usage:       "log level (debug, info, warn, error)",
			Destination: &logLevel,
			EnvVars:     []string{"COCOON_LOG_LEVEL"},
		},
	}

	app.Commands = []*cli.Command{
		initCommand(),
		createCommand(),
		runCommand(),
		startCommand(),
		stopCommand(),
		killCommand(),
		rmCommand(),
		psCommand(),
		inspectCommand(),
		logsCommand(),
		imagesCommand(),
		gcCommand(),
		firmwareCommand(),
		doctorCommand(),
		versionCommand(),
	}

	os.Exit(run(app))
}

func run(app *cli.App) int {
	// Set up signal handling for graceful shutdown.
	ctx, stop := signal.NotifyContext(context.TODO(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer stop()

	if err := app.RunContext(ctx, os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	return 0
}

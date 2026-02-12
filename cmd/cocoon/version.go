package main

import (
	"fmt"

	cli "github.com/urfave/cli/v2"

	"github.com/CMGS/cocoon/version"
)

func versionCommand() *cli.Command {
	return &cli.Command{
		Name:  "version",
		Usage: "Show version information",
		Action: func(_ *cli.Context) error {
			fmt.Print(version.String())
			return nil
		},
	}
}

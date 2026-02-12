package main

import (
	"fmt"

	cli "github.com/urfave/cli/v2"
)

func doctorCommand() *cli.Command {
	return &cli.Command{
		Name:  "doctor",
		Usage: "Check system health and dependencies",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "fix",
				Usage: "attempt to fix issues automatically",
			},
			&cli.BoolFlag{
				Name:  "force",
				Usage: "force re-check even if cached results exist",
			},
			&cli.StringFlag{
				Name:  "format",
				Value: "table",
				Usage: "output format (table, json)",
			},
		},
		Action: doctorAction,
	}
}

func doctorAction(c *cli.Context) error {
	app, err := initApp(c)
	if err != nil {
		return err
	}

	fix := c.Bool("fix")
	force := c.Bool("force")
	format := c.String("format")

	issues, err := app.vmMgr.Reconcile(c.Context, fix, force)
	if err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}

	if len(issues) == 0 {
		fmt.Println("No issues found.")
		return nil
	}

	// JSON output.
	if format == formatJSON {
		return printJSON(issues)
	}

	// Table output (default).
	headers := []string{"VM ID", "TYPE", "SEVERITY", "DETAILS"}
	rows := make([][]string, 0, len(issues))
	for _, issue := range issues {
		rows = append(rows, []string{
			truncateID(issue.VMID, 16),
			string(issue.Type),
			string(issue.Severity),
			issue.Details,
		})
	}
	printTable(headers, rows)

	if fix {
		fmt.Printf("\nAttempted to fix %d issue(s).\n", len(issues))
	} else {
		fmt.Printf("\nFound %d issue(s). Run with --fix to attempt automatic repair.\n", len(issues))
	}

	return nil
}

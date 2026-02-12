package main

import (
	"fmt"

	cli "github.com/urfave/cli/v2"
)

func imagesCommand() *cli.Command {
	return &cli.Command{
		Name:    "images",
		Aliases: []string{"image"},
		Usage:   "List cached VM images",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "format",
				Value: "table",
				Usage: "output format (table, json)",
			},
		},
		Action: imagesAction,
	}
}

func imagesAction(c *cli.Context) error {
	app, err := initApp(c)
	if err != nil {
		return err
	}

	images, err := app.imgMgr.ListCached(c.Context)
	if err != nil {
		return fmt.Errorf("list cached images: %w", err)
	}

	format := c.String("format")

	// JSON output.
	if format == formatJSON {
		return printJSON(images)
	}

	// Table output (default).
	headers := []string{"BASE KEY", "SIZE", "REF COUNT", "CACHED AT"}
	rows := make([][]string, 0, len(images))
	for _, img := range images {
		rows = append(rows, []string{
			img.BaseKey,
			humanBytes(img.Size),
			fmt.Sprintf("%d", img.RefCount),
			img.CreatedAt.Format("2006-01-02T15:04:05Z"),
		})
	}
	printTable(headers, rows)
	return nil
}

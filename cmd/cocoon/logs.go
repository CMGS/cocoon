package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"time"

	cli "github.com/urfave/cli/v2"
)

func logsCommand() *cli.Command {
	return &cli.Command{
		Name:      "logs",
		Usage:     "View VM serial console logs",
		ArgsUsage: "VM_REF",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:    "follow",
				Aliases: []string{"f"},
				Usage:   "follow log output",
			},
			&cli.IntFlag{
				Name:  "tail",
				Value: 100,
				Usage: "number of lines to show from the end",
			},
			&cli.BoolFlag{
				Name:    "timestamps",
				Aliases: []string{"t"},
				Usage:   "prefix each line with a timestamp",
			},
		},
		Action: logsAction,
	}
}

func logsAction(c *cli.Context) error {
	if c.NArg() < 1 {
		return fmt.Errorf("VM_REF argument required\n\nUsage: cocoon logs VM_REF [flags]")
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

	// Determine serial log path from config.
	logPath := app.cfg.VMSerialLogPath(vmID)

	follow := c.Bool("follow")
	tailN := c.Int("tail")
	timestamps := c.Bool("timestamps")

	// Print the last N lines.
	if err := printTailLines(logPath, tailN, timestamps); err != nil {
		return fmt.Errorf("read log file: %w", err)
	}

	// If --follow, poll for new data.
	if follow {
		return followLog(c, logPath, timestamps)
	}

	return nil
}

// printTailLines reads the last n lines from a file and prints them.
// If timestamps is true, each line is prefixed with the current time.
func printTailLines(path string, n int, timestamps bool) error {
	f, err := os.Open(path) //nolint:gosec // G304: path is a derived serial log path, not direct user input
	if err != nil {
		if os.IsNotExist(err) {
			// No log file yet -- not an error, just nothing to show.
			return nil
		}
		return err
	}
	defer func() { _ = f.Close() }()

	// Read all lines into a ring buffer of size n.
	scanner := bufio.NewScanner(f)
	// Increase buffer size for potentially long serial log lines.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	lines := make([]string, 0, n)
	for scanner.Scan() {
		if len(lines) >= n {
			// Shift: drop the oldest line.
			lines = append(lines[1:], scanner.Text())
		} else {
			lines = append(lines, scanner.Text())
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	for _, line := range lines {
		printLogLine(line, timestamps)
	}
	return nil
}

// followLog opens the log file and polls for new data every 500ms.
// It respects context cancellation for graceful shutdown.
func followLog(c *cli.Context, path string, timestamps bool) error {
	f, err := os.Open(path) //nolint:gosec // G304: path is a derived serial log path
	if err != nil {
		if os.IsNotExist(err) {
			// Wait for the file to appear.
			f, err = waitForFile(c, path)
			if err != nil {
				return err
			}
		} else {
			return err
		}
	}
	defer func() { _ = f.Close() }()

	// Seek to end of file -- tail lines were already printed.
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("seek to end: %w", err)
	}

	reader := bufio.NewReader(f)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-c.Done():
			return nil
		case <-ticker.C:
			for {
				line, err := reader.ReadString('\n')
				if len(line) > 0 {
					if timestamps {
						fmt.Printf("%s %s", time.Now().UTC().Format(time.RFC3339), line)
					} else {
						fmt.Print(line)
					}
				}
				if err != nil {
					break
				}
			}
		}
	}
}

// printLogLine prints a single log line, optionally prefixed with a timestamp.
func printLogLine(line string, timestamps bool) {
	if timestamps {
		fmt.Printf("%s %s\n", time.Now().UTC().Format(time.RFC3339), line)
	} else {
		fmt.Println(line)
	}
}

// waitForFile polls until the file at path exists, checking every 500ms.
// Returns the opened file or an error if the context is canceled.
func waitForFile(c *cli.Context, path string) (*os.File, error) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-c.Done():
			return nil, c.Err()
		case <-ticker.C:
			f, err := os.Open(path) //nolint:gosec // G304: path is a derived serial log path
			if err == nil {
				return f, nil
			}
			if !os.IsNotExist(err) {
				return nil, err
			}
		}
	}
}

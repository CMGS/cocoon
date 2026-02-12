package vm

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"time"
)

// bootPollInterval is the interval between serial log file polls.
const bootPollInterval = 250 * time.Millisecond

// waitForBoot tails the serial log file, scanning for boot success or failure
// patterns. Returns nil when a success pattern matches, or an error if a failure
// pattern matches or the timeout expires.
//
// The function:
//   - Compiles success and failure patterns as regexp.Regexp (returns error if any
//     pattern is invalid).
//   - Waits for the serial log file to appear (it is created by Cloud Hypervisor).
//   - Reads the file in a polling loop, tracking the read offset to avoid
//     re-scanning already-read content.
//   - Returns nil on success pattern match, or an error on failure pattern match
//     or timeout.
func waitForBoot(ctx context.Context, serialLogPath string, timeout time.Duration,
	successPatterns, failurePatterns []string,
) error {
	// Compile all patterns upfront.
	successREs, err := compilePatterns(successPatterns)
	if err != nil {
		return fmt.Errorf("compile success patterns: %w", err)
	}
	failureREs, err := compilePatterns(failurePatterns)
	if err != nil {
		return fmt.Errorf("compile failure patterns: %w", err)
	}

	// Create a timeout context derived from the parent.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Wait for the serial log file to appear.
	if waitErr := waitForFile(ctx, serialLogPath); waitErr != nil {
		return waitErr
	}

	// Open the file for tailing.
	f, err := os.Open(serialLogPath) //nolint:gosec // G304: serial log path is derived from config, not user input
	if err != nil {
		return fmt.Errorf("open serial log %s: %w", serialLogPath, err)
	}
	defer func() { _ = f.Close() }()

	// Poll the file for new content.
	reader := bufio.NewReader(f)
	// partial holds an incomplete line from the previous read (no trailing newline yet).
	var partial string

	ticker := time.NewTicker(bootPollInterval)
	defer ticker.Stop()

	for {
		// Try to read available lines.
		for {
			line, readErr := reader.ReadString('\n')
			if len(line) > 0 {
				if readErr == io.EOF && line[len(line)-1] != '\n' {
					// Incomplete line (no trailing newline): buffer it for
					// the next poll iteration so we don't match partial output.
					partial += line
					break
				}

				// Complete line: prepend any buffered partial data.
				fullLine := partial + line
				partial = ""

				// Check against failure patterns first (fail-fast).
				if pat, matched := matchesAny(fullLine, failureREs, failurePatterns); matched {
					return fmt.Errorf("boot failure detected: %q matched pattern %q", trimLine(fullLine), pat)
				}
				// Check against success patterns.
				if _, matched := matchesAny(fullLine, successREs, successPatterns); matched {
					return nil
				}
			}
			if readErr != nil {
				if readErr == io.EOF {
					break // No more data available; wait for next poll.
				}
				return fmt.Errorf("read serial log: %w", readErr)
			}
		}

		// Wait for new data or context cancellation.
		select {
		case <-ctx.Done():
			return fmt.Errorf("boot timeout: no boot completion detected within %s", timeout)
		case <-ticker.C:
			// Continue reading.
		}
	}
}

// waitForFile polls until the given path exists or the context expires.
func waitForFile(ctx context.Context, path string) error {
	// Fast path: check immediately.
	if _, err := os.Stat(path); err == nil {
		return nil
	}

	ticker := time.NewTicker(bootPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("boot timeout: serial log file %s did not appear", path)
		case <-ticker.C:
			if _, err := os.Stat(path); err == nil {
				return nil
			}
		}
	}
}

// compilePatterns compiles a slice of regex pattern strings into compiled
// regexp.Regexp objects. Returns an error if any pattern is invalid.
func compilePatterns(patterns []string) ([]*regexp.Regexp, error) {
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("invalid pattern %q: %w", p, err)
		}
		compiled = append(compiled, re)
	}
	return compiled, nil
}

// matchesAny checks a line against compiled patterns and returns the original
// pattern string and true if any match is found.
func matchesAny(line string, compiled []*regexp.Regexp, originals []string) (string, bool) {
	for i, re := range compiled {
		if re.MatchString(line) {
			return originals[i], true
		}
	}
	return "", false
}

// trimLine removes trailing newline/carriage-return characters for display.
func trimLine(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

package oci

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Cocoonfile represents a parsed Cocoonfile.
type Cocoonfile struct {
	// From is the base image reference (registry ref, URL, or local path).
	From string
	// Steps are the ordered RUN/COPY operations.
	Steps []Step
	// Labels are key=value metadata entries.
	Labels map[string]string
	// BaseDir is the directory containing the Cocoonfile.
	// Used to resolve relative COPY source paths. Set by the caller
	// after parsing (not populated by the parser itself).
	BaseDir string
}

// Step represents a single RUN or COPY instruction.
type Step struct {
	// Type is "run" or "copy".
	Type string
	// Args is the command (for RUN) or "src dest" (for COPY).
	Args string
}

// ParseCocoonfile reads and parses a Cocoonfile from the given path.
func ParseCocoonfile(path string) (*Cocoonfile, error) {
	f, err := os.Open(path) //nolint:gosec // G304: path from CLI flag
	if err != nil {
		return nil, fmt.Errorf("open Cocoonfile: %w", err)
	}
	defer f.Close() //nolint:errcheck

	return parseCocoonfile(bufio.NewScanner(f))
}

// ParseCocoonfileFromString parses a Cocoonfile from a string (for testing).
func ParseCocoonfileFromString(content string) (*Cocoonfile, error) {
	return parseCocoonfile(bufio.NewScanner(strings.NewReader(content)))
}

func parseCocoonfile(scanner *bufio.Scanner) (*Cocoonfile, error) {
	cf := &Cocoonfile{
		Labels: make(map[string]string),
	}

	// Collect logical lines (handling line continuations with \).
	var lines []string
	var current strings.Builder
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()

		// Handle line continuation.
		if trimmed, ok := strings.CutSuffix(line, "\\"); ok {
			current.WriteString(trimmed)
			continue
		}
		current.WriteString(line)
		lines = append(lines, current.String())
		current.Reset()
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read Cocoonfile: %w", err)
	}
	// Flush any remaining continuation.
	if current.Len() > 0 {
		lines = append(lines, current.String())
	}

	for i, line := range lines {
		line = strings.TrimSpace(line)
		// Skip empty lines and comments.
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Split directive and args.
		directive, args := splitDirective(line)
		directive = strings.ToUpper(directive)

		switch directive {
		case "FROM":
			if args == "" {
				return nil, fmt.Errorf("line %d: FROM requires an argument", i+1)
			}
			if cf.From != "" {
				return nil, fmt.Errorf("line %d: multiple FROM directives not supported", i+1)
			}
			cf.From = args

		case "RUN":
			if args == "" {
				return nil, fmt.Errorf("line %d: RUN requires a command", i+1)
			}
			cf.Steps = append(cf.Steps, Step{Type: "run", Args: args})

		case "COPY":
			if args == "" {
				return nil, fmt.Errorf("line %d: COPY requires src and dest arguments", i+1)
			}
			// Validate we have at least two tokens.
			parts := strings.Fields(args)
			if len(parts) < 2 {
				return nil, fmt.Errorf("line %d: COPY requires src and dest (got: %q)", i+1, args)
			}
			cf.Steps = append(cf.Steps, Step{Type: "copy", Args: args})

		case "LABEL":
			if args == "" {
				return nil, fmt.Errorf("line %d: LABEL requires key=value", i+1)
			}
			if err := parseLabels(cf, args); err != nil {
				return nil, fmt.Errorf("line %d: %w", i+1, err)
			}

		default:
			return nil, fmt.Errorf("line %d: unknown directive %q", i+1, directive)
		}
	}

	if cf.From == "" {
		return nil, fmt.Errorf("Cocoonfile must contain a FROM directive")
	}

	return cf, nil
}

// splitDirective splits a line into its directive keyword and remaining arguments.
func splitDirective(line string) (string, string) {
	directive, args, ok := strings.Cut(line, " ")
	if !ok {
		return line, ""
	}
	return directive, strings.TrimSpace(args)
}

// parseLabels parses one or more key=value or key="value" pairs from a LABEL line.
func parseLabels(cf *Cocoonfile, args string) error {
	// Simple parser: split on spaces, each token is key=value or key="value with spaces".
	remaining := args
	for remaining != "" {
		remaining = strings.TrimSpace(remaining)
		if remaining == "" {
			break
		}

		// Find the key (up to '=').
		eqIdx := strings.IndexByte(remaining, '=')
		if eqIdx < 0 {
			return fmt.Errorf("invalid LABEL format: missing '=' in %q", remaining)
		}
		key := remaining[:eqIdx]
		remaining = remaining[eqIdx+1:]

		if key == "" {
			return fmt.Errorf("invalid LABEL format: empty key")
		}

		// Parse value: quoted or unquoted.
		var value string
		if len(remaining) > 0 && remaining[0] == '"' {
			// Quoted value: find closing quote.
			closeIdx := strings.IndexByte(remaining[1:], '"')
			if closeIdx < 0 {
				return fmt.Errorf("unterminated quoted value for key %q", key)
			}
			value = remaining[1 : closeIdx+1]
			remaining = remaining[closeIdx+2:]
		} else {
			// Unquoted value: up to next space.
			spIdx := strings.IndexByte(remaining, ' ')
			if spIdx < 0 {
				value = remaining
				remaining = ""
			} else {
				value = remaining[:spIdx]
				remaining = remaining[spIdx+1:]
			}
		}

		cf.Labels[key] = value
	}
	return nil
}

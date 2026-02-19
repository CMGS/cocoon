package main

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/CMGS/cocoon/utils"
)

// formatJSON is the constant for "json" output format, used across CLI commands.
const formatJSON = "json"

// formatTable is the constant for "table" output format.
const formatTable = "table"

// validOutputFormats lists the allowed values for --format flags.
var validOutputFormats = []string{formatTable, formatJSON}

// validateOutputFormat returns an error if format is not a recognized value.
func validateOutputFormat(format string) error {
	if slices.Contains(validOutputFormats, format) {
		return nil
	}
	return fmt.Errorf("invalid --format value %q: must be one of %v", format, validOutputFormats)
}

// printTable prints a formatted table with the given headers and rows.
// Uses tabwriter for aligned columns.
func printTable(headers []string, rows [][]string) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	// Print header row.
	_, _ = fmt.Fprintln(w, strings.Join(headers, "\t"))
	// Print each data row.
	for _, row := range rows {
		_, _ = fmt.Fprintln(w, strings.Join(row, "\t"))
	}
	_ = w.Flush()
}

// printJSON marshals v as indented JSON and prints to stdout.
func printJSON(v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal JSON: %w", err)
	}
	fmt.Println(string(data))
	return nil
}

// humanBytes delegates to utils.HumanBytes for backward compatibility.
func humanBytes(b int64) string {
	return utils.HumanBytes(b)
}

// truncateID shortens a VM ID or similar identifier for table display.
// Returns the first maxLen characters followed by "..." if truncated.
func truncateID(id string, maxLen int) string {
	if len(id) <= maxLen {
		return id
	}
	return id[:maxLen] + "..."
}

// truncateDigest shortens a digest string for table display.
// Input can be "sha256:abcdef..." or just "abcdef...".
// Returns "sha256:abcdef01..." (first 12 hex chars after sha256:).
func truncateDigest(digest string) string {
	if len(digest) > 7 && digest[:7] == "sha256:" {
		hex := digest[7:]
		if len(hex) > 12 {
			return "sha256:" + hex[:12]
		}
		return digest
	}
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}

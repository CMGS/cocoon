package engine

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Helper: write string to file, creating it if needed.
// ---------------------------------------------------------------------------

func writeToFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open file %s: %v", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("write to %s: %v", path, err)
	}
}

// ---------------------------------------------------------------------------
// Tests for compilePatterns
// ---------------------------------------------------------------------------

func TestCompilePatterns_Valid(t *testing.T) {
	t.Parallel()
	patterns := []string{`login:`, `Reached target.*Login`, `^boot\s+ok$`}
	compiled, err := compilePatterns(patterns)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(compiled) != len(patterns) {
		t.Fatalf("expected %d compiled patterns, got %d", len(patterns), len(compiled))
	}
}

func TestCompilePatterns_Invalid(t *testing.T) {
	t.Parallel()
	patterns := []string{`valid`, `[invalid`}
	_, err := compilePatterns(patterns)
	if err == nil {
		t.Fatal("expected error for invalid regex, got nil")
	}
}

func TestCompilePatterns_Empty(t *testing.T) {
	t.Parallel()
	compiled, err := compilePatterns(nil)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(compiled) != 0 {
		t.Fatalf("expected 0 compiled patterns, got %d", len(compiled))
	}
}

// ---------------------------------------------------------------------------
// Tests for matchesAny
// ---------------------------------------------------------------------------

func TestMatchesAny_Match(t *testing.T) {
	t.Parallel()
	patterns := []string{`login:`, `Kernel panic`}
	compiled, err := compilePatterns(patterns)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	pat, matched := matchesAny("ubuntu login: ", compiled, patterns)
	if !matched {
		t.Fatal("expected match, got false")
	}
	if pat != "login:" {
		t.Fatalf("expected pattern %q, got %q", "login:", pat)
	}
}

func TestMatchesAny_NoMatch(t *testing.T) {
	t.Parallel()
	patterns := []string{`login:`, `Kernel panic`}
	compiled, err := compilePatterns(patterns)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	_, matched := matchesAny("booting normally...", compiled, patterns)
	if matched {
		t.Fatal("expected no match, got true")
	}
}

func TestMatchesAny_EmptyPatterns(t *testing.T) {
	t.Parallel()
	_, matched := matchesAny("anything", []*regexp.Regexp{}, []string{})
	if matched {
		t.Fatal("expected no match with empty patterns")
	}
}

// ---------------------------------------------------------------------------
// Tests for trimLine
// ---------------------------------------------------------------------------

func TestTrimLine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{name: "trailing newline", input: "hello\n", expected: "hello"},
		{name: "trailing CR+LF", input: "hello\r\n", expected: "hello"},
		{name: "trailing CR", input: "hello\r", expected: "hello"},
		{name: "multiple trailing", input: "hello\n\r\n", expected: "hello"},
		{name: "no trailing", input: "hello", expected: "hello"},
		{name: "empty string", input: "", expected: ""},
		{name: "only newline", input: "\n", expected: ""},
		{name: "only CRLF", input: "\r\n", expected: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := trimLine(tc.input)
			if got != tc.expected {
				t.Errorf("trimLine(%q) = %q, want %q", tc.input, got, tc.expected)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Tests for waitForBoot
// ---------------------------------------------------------------------------

func TestWaitForBoot_SuccessPatternMatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "serial.log")

	// Write log content that includes a success pattern.
	writeToFile(t, logPath, "Booting kernel...\nsystemd started\nubuntu login: \n")

	ctx := t.Context()
	err := waitForBoot(ctx, logPath, 2*time.Second,
		[]string{`login:`},
		[]string{`Kernel panic`},
	)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

func TestWaitForBoot_FailurePatternMatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "serial.log")

	// Write log content that includes a failure pattern.
	writeToFile(t, logPath, "Booting kernel...\nKernel panic - not syncing: VFS\n")

	ctx := t.Context()
	err := waitForBoot(ctx, logPath, 2*time.Second,
		[]string{`login:`},
		[]string{`Kernel panic`},
	)
	if err == nil {
		t.Fatal("expected error for failure pattern, got nil")
	}
	if got := err.Error(); !contains(got, "boot failure detected") {
		t.Fatalf("expected error containing 'boot failure detected', got %q", got)
	}
	if got := err.Error(); !contains(got, "Kernel panic") {
		t.Fatalf("expected error mentioning 'Kernel panic', got %q", got)
	}
}

func TestWaitForBoot_Timeout(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "serial.log")

	// Write some content that does NOT match any pattern.
	writeToFile(t, logPath, "loading modules...\ninitialized hardware\n")

	ctx := t.Context()
	err := waitForBoot(ctx, logPath, 100*time.Millisecond,
		[]string{`login:`},
		[]string{`Kernel panic`},
	)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if got := err.Error(); !contains(got, "boot timeout") {
		t.Fatalf("expected error containing 'boot timeout', got %q", got)
	}
}

func TestWaitForBoot_FileAppearsLate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "serial.log")

	// Start waitForBoot before the file exists. Create the file after a delay.
	go func() {
		time.Sleep(300 * time.Millisecond)
		writeToFile(t, logPath, "starting boot...\nubuntu login: \n")
	}()

	ctx := t.Context()
	err := waitForBoot(ctx, logPath, 2*time.Second,
		[]string{`login:`},
		[]string{`Kernel panic`},
	)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

func TestWaitForBoot_InvalidSuccessRegex(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "serial.log")

	// Create the file so we can reach the pattern compilation step.
	writeToFile(t, logPath, "anything\n")

	ctx := t.Context()
	err := waitForBoot(ctx, logPath, 1*time.Second,
		[]string{`[invalid`}, // bad regex
		[]string{`Kernel panic`},
	)
	if err == nil {
		t.Fatal("expected error for invalid regex, got nil")
	}
	if got := err.Error(); !contains(got, "compile success patterns") {
		t.Fatalf("expected error mentioning 'compile success patterns', got %q", got)
	}
}

func TestWaitForBoot_InvalidFailureRegex(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "serial.log")

	writeToFile(t, logPath, "anything\n")

	ctx := t.Context()
	err := waitForBoot(ctx, logPath, 1*time.Second,
		[]string{`login:`},
		[]string{`[invalid`}, // bad regex
	)
	if err == nil {
		t.Fatal("expected error for invalid failure regex, got nil")
	}
	if got := err.Error(); !contains(got, "compile failure patterns") {
		t.Fatalf("expected error mentioning 'compile failure patterns', got %q", got)
	}
}

func TestWaitForBoot_PartialLineMatchesSuccess(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "serial.log")

	// Write a partial line (no newline) that contains the success pattern.
	// This simulates the getty "login: " prompt which never ends with \n
	// (getty waits for user input). The partial line should match success
	// patterns immediately.
	writeToFile(t, logPath, "ubuntu login: ")

	ctx := t.Context()
	err := waitForBoot(ctx, logPath, 2*time.Second,
		[]string{`login:`},
		[]string{`Kernel panic`},
	)
	if err != nil {
		t.Fatalf("expected nil error for partial line with success pattern, got %v", err)
	}
}

func TestWaitForBoot_PartialLineDoesNotMatchFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "serial.log")

	// Write a partial line that contains a failure pattern substring.
	// Failure patterns should NOT match partial lines (wait for the full
	// line to avoid false positives from partial kernel output).
	writeToFile(t, logPath, "Kernel pani")

	ctx := t.Context()
	err := waitForBoot(ctx, logPath, 500*time.Millisecond,
		[]string{`login:`},
		[]string{`Kernel panic`},
	)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if got := err.Error(); !contains(got, "boot timeout") {
		t.Fatalf("expected 'boot timeout' (not a false failure match), got %q", got)
	}
}

func TestWaitForBoot_ContextCancelled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "serial.log")

	writeToFile(t, logPath, "booting...\n")

	ctx, cancel := context.WithCancel(t.Context())
	// Cancel immediately.
	cancel()

	err := waitForBoot(ctx, logPath, 5*time.Second,
		[]string{`login:`},
		[]string{`Kernel panic`},
	)
	if err == nil {
		t.Fatal("expected error on cancelled context, got nil")
	}
	if got := err.Error(); !contains(got, "boot timeout") {
		t.Fatalf("expected error containing 'boot timeout', got %q", got)
	}
}

func TestWaitForBoot_FailureBeforeSuccess(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "serial.log")

	// Failure pattern appears before success pattern; should fail.
	writeToFile(t, logPath, "Kernel panic\nlogin:\n")

	ctx := t.Context()
	err := waitForBoot(ctx, logPath, 2*time.Second,
		[]string{`login:`},
		[]string{`Kernel panic`},
	)
	if err == nil {
		t.Fatal("expected error when failure appears before success, got nil")
	}
	if got := err.Error(); !contains(got, "boot failure detected") {
		t.Fatalf("expected 'boot failure detected', got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Tests for waitForFile
// ---------------------------------------------------------------------------

func TestWaitForFile_AlreadyExists(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "existing.log")
	writeToFile(t, path, "data")

	ctx, cancel := context.WithTimeout(t.Context(), 1*time.Second)
	defer cancel()

	if err := waitForFile(ctx, path); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestWaitForFile_AppearsLater(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "late.log")

	go func() {
		time.Sleep(200 * time.Millisecond)
		writeToFile(t, path, "data")
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	if err := waitForFile(ctx, path); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestWaitForFile_Timeout(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "never.log")

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	err := waitForFile(ctx, path)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if got := err.Error(); !contains(got, "did not appear") {
		t.Fatalf("expected 'did not appear' in error, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Helper: string contains check
// ---------------------------------------------------------------------------

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && containsStr(s, substr)))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

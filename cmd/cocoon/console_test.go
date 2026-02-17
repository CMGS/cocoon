package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"
	"time"
)

func TestRelayStdinToPTY(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		input      []byte
		escapeChar byte
		wantPTY    []byte
		wantExit   bool
	}{
		{
			name:       "normal text passthrough",
			input:      []byte("hello world"),
			escapeChar: '~',
			wantPTY:    []byte("hello world"),
		},
		{
			name:       "escape at line start disconnects",
			input:      []byte("\r~."),
			escapeChar: '~',
			wantPTY:    []byte("\r"),
			wantExit:   true,
		},
		{
			name:       "escape at session start disconnects",
			input:      []byte("~."),
			escapeChar: '~',
			wantPTY:    nil,
			wantExit:   true,
		},
		{
			name:       "tilde in middle of line passes through",
			input:      []byte("hello~.world"),
			escapeChar: '~',
			wantPTY:    []byte("hello~.world"),
		},
		{
			name:       "double tilde sends literal tilde",
			input:      []byte("\r~~"),
			escapeChar: '~',
			wantPTY:    []byte("\r~"),
		},
		{
			name:       "unrecognized escape forwards both chars",
			input:      []byte("\r~x"),
			escapeChar: '~',
			wantPTY:    []byte("\r~x"),
		},
		{
			name:       "escape after newline disconnects",
			input:      []byte("hello\n~."),
			escapeChar: '~',
			wantPTY:    []byte("hello\n"),
			wantExit:   true,
		},
		{
			name:       "consecutive newlines then escape",
			input:      []byte("\r\n\r\n"),
			escapeChar: '~',
			wantPTY:    []byte("\r\n\r\n"),
		},
		{
			name:       "newline then escape disconnects",
			input:      []byte("\n~."),
			escapeChar: '~',
			wantPTY:    []byte("\n"),
			wantExit:   true,
		},
		{
			name:       "escape help prints to stdout not pty",
			input:      []byte("\r~?rest"),
			escapeChar: '~',
			wantPTY:    []byte("\rrest"),
		},
		{
			name:       "escape disconnect immediately after help",
			input:      []byte("\r~?~."),
			escapeChar: '~',
			wantPTY:    []byte("\r"),
			wantExit:   true,
		},
		{
			name:       "empty input",
			input:      []byte{},
			escapeChar: '~',
			wantPTY:    nil,
		},
		// Custom escape character tests.
		{
			name:       "custom escape char disconnects",
			input:      []byte("\r^."),
			escapeChar: '^',
			wantPTY:    []byte("\r"),
			wantExit:   true,
		},
		{
			name:       "custom escape char tilde passes through",
			input:      []byte("\r~."),
			escapeChar: '^',
			wantPTY:    []byte("\r~."),
		},
		{
			name:       "custom escape char double sends literal",
			input:      []byte("\r^^"),
			escapeChar: '^',
			wantPTY:    []byte("\r^"),
		},
		// Escape state machine edge case: unrecognized escape followed by
		// CR/LF must transition to stateLineStart so the next escape works.
		{
			name:       "unrecognized escape then CR preserves line start",
			input:      []byte("\r~\r~."),
			escapeChar: '~',
			wantPTY:    []byte("\r~\r"),
			wantExit:   true,
		},
		{
			name:       "unrecognized escape then LF preserves line start",
			input:      []byte("\n~\n~."),
			escapeChar: '~',
			wantPTY:    []byte("\n~\n"),
			wantExit:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stdin := bytes.NewReader(tt.input)
			ptyBuf := &bytes.Buffer{}

			err := relayStdinToPTY(context.Background(), stdin, ptyBuf, tt.escapeChar)

			if tt.wantExit {
				if err != nil {
					t.Errorf("expected clean exit (nil error), got: %v", err)
				}
			} else {
				// Non-exit: we expect EOF from the reader.
				if err != nil && err != io.EOF {
					t.Errorf("unexpected error: %v", err)
				}
			}

			got := ptyBuf.Bytes()
			if !bytes.Equal(got, tt.wantPTY) {
				t.Errorf("PTY output mismatch:\n  got:  %q\n  want: %q", got, tt.wantPTY)
			}
		})
	}
}

// Integration tests for consoleAction. These require a running Cloud Hypervisor
// instance and are skipped in unit test mode. Run with: go test -run TestConsole -tags integration
//
// See docs/12-console.md Section 9.2 for the full integration test specification.

func TestConsoleAttachDetach(t *testing.T) {
	t.Skip("integration test: requires running Cloud Hypervisor instance with a VM")

	// TODO: When integration test infrastructure is available:
	// 1. Create a VM with console mode Pty
	// 2. Start the VM and wait for boot
	// 3. Attach console, verify PTY path from vm.info
	// 4. Send input, verify output appears on guest
	// 5. Send ~. escape sequence, verify clean disconnect
	// 6. Verify VM continues running after detach
	// 7. Verify terminal is restored to original state
}

func TestConsoleLegacyVMError(t *testing.T) {
	t.Skip("integration test: requires running Cloud Hypervisor instance with a legacy VM")

	// TODO: When integration test infrastructure is available:
	// 1. Create a VM with console mode Off (simulating pre-Pty VM)
	// 2. Attempt cocoon console
	// 3. Verify error: "no console PTY available... recreate it to enable interactive console"
	// 4. Verify error mentions 'cocoon doctor' for CH version check
}

func TestRelayStdinToPTY_ContextCancellation(t *testing.T) {
	t.Parallel()

	// Use an io.Pipe so the reader blocks until data arrives.
	pr, pw := io.Pipe()
	defer pw.Close() //nolint:errcheck

	ptyBuf := &bytes.Buffer{}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- relayStdinToPTY(ctx, pr, ptyBuf, '~')
	}()

	// Write a byte so the goroutine enters the read loop.
	_, _ = pw.Write([]byte("a"))
	// Give the goroutine time to process 'a' and block on the next Read.
	time.Sleep(50 * time.Millisecond)

	// Cancel context, then send another byte to unblock the pipe Read.
	// The goroutine will process 'b', loop back to the select, and find
	// ctx.Done() — returning nil.
	cancel()
	_, _ = pw.Write([]byte("b"))

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("expected nil on context cancellation, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("relayStdinToPTY did not return after context cancellation")
	}
}

func TestRelayStdinToPTY_ControlCharEscape(t *testing.T) {
	t.Parallel()

	// Ctrl-] (0x1D) as escape char, matching telnet default.
	tests := []struct {
		name       string
		input      []byte
		escapeChar byte
		wantPTY    []byte
		wantExit   bool
	}{
		{
			name:       "ctrl-] at session start disconnects",
			input:      []byte{0x1D, '.'},
			escapeChar: 0x1D,
			wantPTY:    nil,
			wantExit:   true,
		},
		{
			name:       "ctrl-] after CR disconnects",
			input:      []byte{'\r', 0x1D, '.'},
			escapeChar: 0x1D,
			wantPTY:    []byte("\r"),
			wantExit:   true,
		},
		{
			name:       "ctrl-] mid-line passes through",
			input:      []byte{'h', 'i', 0x1D, '.'},
			escapeChar: 0x1D,
			wantPTY:    []byte{'h', 'i', 0x1D, '.'},
		},
		{
			name:       "double ctrl-] sends literal",
			input:      []byte{'\r', 0x1D, 0x1D},
			escapeChar: 0x1D,
			wantPTY:    []byte{'\r', 0x1D},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stdin := bytes.NewReader(tt.input)
			ptyBuf := &bytes.Buffer{}
			err := relayStdinToPTY(context.Background(), stdin, ptyBuf, tt.escapeChar)
			if tt.wantExit {
				if err != nil {
					t.Errorf("expected clean exit, got: %v", err)
				}
			} else {
				if err != nil && err != io.EOF {
					t.Errorf("unexpected error: %v", err)
				}
			}
			got := ptyBuf.Bytes()
			if !bytes.Equal(got, tt.wantPTY) {
				t.Errorf("PTY output mismatch:\n  got:  %q\n  want: %q", got, tt.wantPTY)
			}
		})
	}
}

func TestParseEscapeChar(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input   string
		want    byte
		wantErr bool
	}{
		{"^]", 0x1D, false},  // Ctrl-]
		{"^A", 0x01, false},  // Ctrl-A
		{"^C", 0x03, false},  // Ctrl-C
		{"^_", 0x1F, false},  // Ctrl-_
		{"^a", 0x01, false},  // lowercase caret notation
		{"^z", 0x1A, false},  // Ctrl-Z
		{"~", '~', false},    // printable char
		{"#", '#', false},    // printable char
		{" ", ' ', false},    // space (0x20)
		{"^@", 0, true},      // NUL rejected
		{"\x00", 0, true},    // raw NUL rejected
		{"\x7F", 0, true},    // DEL rejected
		{"^!", 0, true},      // invalid caret notation
		{"ab", 0, true},      // too long
		{"", 0, true},        // empty
		{"abc", 0, true},     // way too long
		{"\x1D", 0x1D, false}, // raw Ctrl-] byte
		{"\x01", 0x01, false}, // raw Ctrl-A byte
		// CR/LF rejected (conflict with line-start detection).
		{"^M", 0, true},      // CR via caret notation
		{"^J", 0, true},      // LF via caret notation
		{"\r", 0, true},      // raw CR
		{"\n", 0, true},      // raw LF
		// '.' and '?' rejected (conflict with escape commands).
		{".", 0, true},
		{"?", 0, true},
		// High bytes rejected (non-ASCII).
		{"\x80", 0, true},
		{"\xFF", 0, true},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("input=%q", tt.input), func(t *testing.T) {
			t.Parallel()
			got, err := parseEscapeChar(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error for input %q, got byte %d", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error for input %q: %v", tt.input, err)
				return
			}
			if got != tt.want {
				t.Errorf("parseEscapeChar(%q) = 0x%02X, want 0x%02X", tt.input, got, tt.want)
			}
		})
	}
}

func TestFormatEscapeChar(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input byte
		want  string
	}{
		{0x1D, "^]"},
		{0x01, "^A"},
		{0x03, "^C"},
		{0x1F, "^_"},
		{'~', "~"},
		{'#', "#"},
		{' ', " "},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("byte=0x%02X", tt.input), func(t *testing.T) {
			t.Parallel()
			got := formatEscapeChar(tt.input)
			if got != tt.want {
				t.Errorf("formatEscapeChar(0x%02X) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// errWriter is an io.Writer that always returns an error.
type errWriter struct{ err error }

func (w *errWriter) Write([]byte) (int, error) { return 0, w.err }

func TestRelayStdinToPTY_WriteError(t *testing.T) {
	t.Parallel()

	wantErr := fmt.Errorf("broken pipe")
	stdin := bytes.NewReader([]byte("hello"))
	pty := &errWriter{err: wantErr}

	err := relayStdinToPTY(context.Background(), stdin, pty, '~')
	if err == nil {
		t.Fatal("expected write error, got nil")
	}
	if err.Error() != wantErr.Error() {
		t.Errorf("expected error %q, got %q", wantErr, err)
	}
}

package main

import (
	"bytes"
	"context"
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

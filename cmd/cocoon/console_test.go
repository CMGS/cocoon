package main

import (
	"bytes"
	"context"
	"io"
	"testing"
)

func TestRelayStdinToPTY(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    []byte
		wantPTY  []byte
		wantExit bool
	}{
		{
			name:    "normal text passthrough",
			input:   []byte("hello world"),
			wantPTY: []byte("hello world"),
		},
		{
			name:     "escape at line start disconnects",
			input:    []byte("\r~."),
			wantPTY:  []byte("\r"),
			wantExit: true,
		},
		{
			name:     "escape at session start disconnects",
			input:    []byte("~."),
			wantPTY:  nil,
			wantExit: true,
		},
		{
			name:    "tilde in middle of line passes through",
			input:   []byte("hello~.world"),
			wantPTY: []byte("hello~.world"),
		},
		{
			name:    "double tilde sends literal tilde",
			input:   []byte("\r~~"),
			wantPTY: []byte("\r~"),
		},
		{
			name:    "unrecognized escape forwards both chars",
			input:   []byte("\r~x"),
			wantPTY: []byte("\r~x"),
		},
		{
			name:     "escape after newline disconnects",
			input:    []byte("hello\n~."),
			wantPTY:  []byte("hello\n"),
			wantExit: true,
		},
		{
			name:    "consecutive newlines then escape",
			input:   []byte("\r\n\r\n"),
			wantPTY: []byte("\r\n\r\n"),
		},
		{
			name:     "newline then escape disconnects",
			input:    []byte("\n~."),
			wantPTY:  []byte("\n"),
			wantExit: true,
		},
		{
			name:    "escape help prints to stdout not pty",
			input:   []byte("\r~?rest"),
			wantPTY: []byte("\rrest"),
		},
		{
			name:     "escape disconnect immediately after help",
			input:    []byte("\r~?~."),
			wantPTY:  []byte("\r"),
			wantExit: true,
		},
		{
			name:    "empty input",
			input:   []byte{},
			wantPTY: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stdin := bytes.NewReader(tt.input)
			ptyBuf := &bytes.Buffer{}

			err := relayStdinToPTY(context.Background(), stdin, ptyBuf, '~')

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

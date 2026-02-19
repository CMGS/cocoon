package oci

import (
	"fmt"
	"net"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/CMGS/cocoon/types"
	"github.com/CMGS/cocoon/utils"
)

func TestClassifyPushError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		err           error
		wantTransient bool
	}{
		{
			name:          "unauthorized is permanent",
			err:           fmt.Errorf("UNAUTHORIZED: authentication required"),
			wantTransient: false,
		},
		{
			name:          "denied is permanent",
			err:           fmt.Errorf("DENIED: access forbidden"),
			wantTransient: false,
		},
		{
			name:          "not found is permanent",
			err:           fmt.Errorf("NAME_UNKNOWN: repository not found"),
			wantTransient: false,
		},
		{
			name:          "timeout is transient",
			err:           fmt.Errorf("connection timeout"),
			wantTransient: true,
		},
		{
			name:          "connection refused is transient",
			err:           fmt.Errorf("dial tcp: connection refused"),
			wantTransient: true,
		},
		{
			name:          "503 is transient",
			err:           fmt.Errorf("503 Service Unavailable"),
			wantTransient: true,
		},
		{
			name:          "429 is transient",
			err:           fmt.Errorf("429 Too Many Requests"),
			wantTransient: true,
		},
		{
			name: "net.OpError is transient",
			err: &net.OpError{
				Op:  "dial",
				Net: "tcp",
				Err: fmt.Errorf("connection refused"),
			},
			wantTransient: true,
		},
		{
			name:          "unknown error is permanent",
			err:           fmt.Errorf("something unexpected"),
			wantTransient: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			classified := classifyPushError(tt.err)
			if types.IsTransient(classified) != tt.wantTransient {
				t.Errorf("IsTransient = %v, want %v", types.IsTransient(classified), tt.wantTransient)
			}
		})
	}
}

func TestFormatPushProgressLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   v1.Update
		want string
	}{
		{
			name: "known total with percent",
			in: v1.Update{
				Complete: 512,
				Total:    1024,
			},
			want: "Pushing image:  50% (512B / 1.0KB)",
		},
		{
			name: "unknown total",
			in: v1.Update{
				Complete: 2048,
			},
			want: "Pushing image: 2.0KB",
		},
		{
			name: "clamp over 100 percent",
			in: v1.Update{
				Complete: 2048,
				Total:    1024,
			},
			want: "Pushing image: 100% (2.0KB / 1.0KB)",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := formatPushProgressLine(tt.in); got != tt.want {
				t.Fatalf("formatPushProgressLine() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHumanBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   int64
		want string
	}{
		{name: "bytes", in: 999, want: "999B"},
		{name: "kb", in: 1024, want: "1.0KB"},
		{name: "mb", in: 1024 * 1024, want: "1.0MB"},
		{name: "negative", in: -1, want: "0B"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := utils.HumanBytes(tt.in); got != tt.want {
				t.Fatalf("HumanBytes(%d) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

package oci

import (
	"fmt"
	"net"
	"testing"

	"github.com/CMGS/cocoon/types"
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

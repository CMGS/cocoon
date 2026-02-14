package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/CMGS/cocoon/image/refcache"
)

func TestShouldFallbackToPrepare(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "local-cache-miss",
			err:  fmt.Errorf("resolve: %w", errImageNotFoundInLocalCache),
			want: true,
		},
		{
			name: "ambiguous-alias",
			err:  fmt.Errorf("resolve: %w", refcache.ErrAmbiguousImageRef),
			want: false,
		},
		{
			name: "generic-error",
			err:  errors.New("lock acquisition failed"),
			want: false,
		},
		{
			name: "nil-error",
			err:  nil,
			want: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldFallbackToPrepare(tt.err); got != tt.want {
				t.Fatalf("shouldFallbackToPrepare(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

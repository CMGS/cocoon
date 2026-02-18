package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/CMGS/cocoon/image/refcache"
	"github.com/CMGS/cocoon/oci"
	"github.com/CMGS/cocoon/types"
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

func TestEvaluateOCILayoutBootability_DirectModeDetected(t *testing.T) {
	t.Parallel()

	info := &oci.LayoutInfo{
		Layers: []oci.LayerInfo{
			{MediaType: oci.MediaTypeKernelLayer},
			{MediaType: oci.MediaTypeRootfsLayer},
		},
		Config: &oci.VMImageConfig{
			KernelPath:    "/vmlinuz",
			InitrdPath:    "/initrd.img",
			KernelCmdline: "console=hvc0",
		},
	}

	result := evaluateOCILayoutBootability(info)
	if !result.Bootable {
		t.Fatalf("Bootable=false, want true (errors=%v)", result.Errors)
	}
	if len(result.BootModes) != 1 || result.BootModes[0] != string(types.BootModeDirect) {
		t.Fatalf("BootModes=%v, want [%s]", result.BootModes, types.BootModeDirect)
	}
	if !result.KernelChecked || !result.KernelFound {
		t.Fatalf("kernel flags checked=%v found=%v, want true/true", result.KernelChecked, result.KernelFound)
	}
}

func TestEvaluateOCILayoutBootability_MissingKernelLayer(t *testing.T) {
	t.Parallel()

	info := &oci.LayoutInfo{
		Layers: []oci.LayerInfo{
			{MediaType: oci.MediaTypeRootfsLayer},
		},
		Config: &oci.VMImageConfig{
			KernelPath:    "/vmlinuz",
			InitrdPath:    "/initrd.img",
			KernelCmdline: "console=hvc0",
		},
	}

	result := evaluateOCILayoutBootability(info)
	if result.Bootable {
		t.Fatal("Bootable=true, want false when kernel layer is missing")
	}
	for _, mode := range result.BootModes {
		if mode == string(types.BootModeDirect) {
			t.Fatalf("BootModes=%v should not contain direct when kernel layer is missing", result.BootModes)
		}
	}
	foundKernelErr := false
	for _, msg := range result.Errors {
		if strings.Contains(msg, "kernel layer not found") {
			foundKernelErr = true
			break
		}
	}
	if !foundKernelErr {
		t.Fatalf("Errors=%v, expected kernel layer missing error", result.Errors)
	}
}

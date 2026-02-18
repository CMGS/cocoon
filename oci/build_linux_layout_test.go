//go:build linux

package oci

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateLayoutWorkDir_UsesTempRootOutsideLayouts(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)
	layoutWorkDir, err := createLayoutWorkDir(cfg)
	if err != nil {
		t.Fatalf("createLayoutWorkDir: %v", err)
	}

	if _, err := os.Stat(layoutWorkDir); err != nil {
		t.Fatalf("stat work dir: %v", err)
	}

	workDirWithSep := filepath.Clean(layoutWorkDir) + string(os.PathSeparator)
	expectedTempRoot := filepath.Join(cfg.TempDir(), "oci-layout-builds")
	expectedTempRootWithSep := filepath.Clean(expectedTempRoot) + string(os.PathSeparator)
	if !strings.HasPrefix(workDirWithSep, expectedTempRootWithSep) {
		t.Fatalf("work dir %q not under %q", layoutWorkDir, expectedTempRoot)
	}

	layoutRootWithSep := filepath.Clean(cfg.OCILayoutDir()) + string(os.PathSeparator)
	if strings.HasPrefix(workDirWithSep, layoutRootWithSep) {
		t.Fatalf("work dir %q must not be under OCI layout root %q", layoutWorkDir, cfg.OCILayoutDir())
	}
}

func TestCustomizationStepProgressLabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		stepType  string
		stepIndex int
		total     int
		want      string
	}{
		{name: "run", stepType: "run", stepIndex: 1, total: 3, want: "RUN [1/3]"},
		{name: "copy", stepType: "copy", stepIndex: 2, total: 3, want: "COPY [2/3]"},
		{name: "unknown", stepType: "label", stepIndex: 1, total: 1, want: "LABEL [1/1]"},
		{name: "empty-type", stepType: "", stepIndex: 1, total: 2, want: "STEP [1/2]"},
		{name: "no-total", stepType: "run", stepIndex: 1, total: 0, want: "RUN"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := customizationStepProgressLabel(tt.stepType, tt.stepIndex, tt.total); got != tt.want {
				t.Fatalf("customizationStepProgressLabel(%q,%d,%d)=%q, want %q", tt.stepType, tt.stepIndex, tt.total, got, tt.want)
			}
		})
	}
}

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

package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CMGS/cocoon/config"
)

func TestCheckOCIRuntimePreflight_RequiresLinux(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultConfig()
	err := checkOCIRuntimePreflight(context.Background(), cfg, ociRuntimePreflightDeps{
		goos: "darwin",
		readFileFn: func(string) ([]byte, error) {
			return []byte(""), nil
		},
		lookPathFn: func(string) (string, error) {
			return "", fmt.Errorf("not used")
		},
		runFn: func(context.Context, string, ...string) ([]byte, error) {
			return nil, fmt.Errorf("not used")
		},
	})
	if err == nil {
		t.Fatal("expected linux host preflight error")
	}
	if !strings.Contains(err.Error(), "requires Linux host") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckOCIRuntimePreflight_RequiresOverlayFS(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultConfig()
	err := checkOCIRuntimePreflight(context.Background(), cfg, ociRuntimePreflightDeps{
		goos: "linux",
		readFileFn: func(string) ([]byte, error) {
			return []byte("nodev\tsquashfs\nnodev\tfuse\n"), nil
		},
		lookPathFn: func(file string) (string, error) {
			if file == "modinfo" {
				return "", os.ErrNotExist
			}
			return "/usr/bin/fake", nil
		},
		runFn: func(context.Context, string, ...string) ([]byte, error) {
			return []byte("1.8.0"), nil
		},
	})
	if err == nil {
		t.Fatal("expected overlayfs preflight error")
	}
	if !strings.Contains(err.Error(), "OverlayFS") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckOCIRuntimePreflight_RequiresVirtiofsdBinary(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultConfig()
	err := checkOCIRuntimePreflight(context.Background(), cfg, ociRuntimePreflightDeps{
		goos: "linux",
		readFileFn: func(string) ([]byte, error) {
			return []byte("nodev\toverlay\n"), nil
		},
		lookPathFn: func(file string) (string, error) {
			if file == "virtiofsd" {
				return "", os.ErrNotExist
			}
			return "/usr/bin/cloud-hypervisor", nil
		},
		runFn: func(context.Context, string, ...string) ([]byte, error) {
			return []byte("38.0.0"), nil
		},
	})
	if err == nil {
		t.Fatal("expected virtiofsd preflight error")
	}
	if !strings.Contains(err.Error(), "virtiofsd") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckOCIRuntimePreflight_RequiresCloudHypervisorVersion(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultConfig()
	err := checkOCIRuntimePreflight(context.Background(), cfg, ociRuntimePreflightDeps{
		goos: "linux",
		readFileFn: func(string) ([]byte, error) {
			return []byte("nodev\toverlay\n"), nil
		},
		lookPathFn: func(file string) (string, error) {
			return filepath.Join("/usr/bin", file), nil
		},
		runFn: func(_ context.Context, binary string, _ ...string) ([]byte, error) {
			if strings.Contains(binary, "virtiofsd") {
				return []byte("virtiofsd 1.8.0"), nil
			}
			return []byte("cloud-hypervisor 37.2.0"), nil
		},
	})
	if err == nil {
		t.Fatal("expected cloud-hypervisor version preflight error")
	}
	if !strings.Contains(err.Error(), "cloud-hypervisor") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckOCIRuntimePreflight_Success(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultConfig()
	err := checkOCIRuntimePreflight(context.Background(), cfg, ociRuntimePreflightDeps{
		goos: "linux",
		readFileFn: func(string) ([]byte, error) {
			return []byte("nodev\toverlay\n"), nil
		},
		lookPathFn: func(file string) (string, error) {
			return filepath.Join("/usr/bin", file), nil
		},
		runFn: func(_ context.Context, binary string, _ ...string) ([]byte, error) {
			if strings.Contains(binary, "virtiofsd") {
				return []byte("virtiofsd 1.9.1"), nil
			}
			return []byte("cloud-hypervisor 38.0.1"), nil
		},
	})
	if err != nil {
		t.Fatalf("expected preflight success, got %v", err)
	}
}

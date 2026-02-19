//go:build linux

package engine

import (
	"os"
	"strings"
	"testing"

	"github.com/CMGS/cocoon/config"
)

func TestOverlayRuntimeEnsureWorkspace(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultConfig()
	cfg.RebaseRootDir(t.TempDir())
	om := newOverlayRuntimeManager(cfg)

	vmID := "vm-test-overlay-workspace"
	if err := om.EnsureWorkspace(vmID); err != nil {
		t.Fatalf("EnsureWorkspace: %v", err)
	}

	paths := []string{
		cfg.VMOCIUpperDir(vmID),
		cfg.VMOCIWorkDir(vmID),
		cfg.VMOCIMergedDir(vmID),
	}
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", p)
		}
	}
}

func TestBuildOverlayMountData(t *testing.T) {
	t.Parallel()

	got, err := buildOverlayMountData(
		[]string{"/cache/rootfs", "/cache/custom"},
		"/var/lib/cocoon/vms/vm-1/upper",
		"/var/lib/cocoon/vms/vm-1/work",
	)
	if err != nil {
		t.Fatalf("buildOverlayMountData: %v", err)
	}

	wantParts := []string{
		"lowerdir=/cache/rootfs:/cache/custom",
		"upperdir=/var/lib/cocoon/vms/vm-1/upper",
		"workdir=/var/lib/cocoon/vms/vm-1/work",
	}
	for _, part := range wantParts {
		if !strings.Contains(got, part) {
			t.Fatalf("mount data %q missing part %q", got, part)
		}
	}
}

func TestBuildOverlayMountDataRejectsInvalidPaths(t *testing.T) {
	t.Parallel()

	if _, err := buildOverlayMountData(nil, "/upper", "/work"); err == nil {
		t.Fatal("expected error for empty lower dirs")
	}
	if _, err := buildOverlayMountData([]string{"/cache:bad"}, "/upper", "/work"); err == nil {
		t.Fatal("expected error for colon in lowerdir path")
	}
	if _, err := buildOverlayMountData([]string{"/cache,bad"}, "/upper", "/work"); err == nil {
		t.Fatal("expected error for comma in lowerdir path")
	}
}

func TestOverlayRuntimeMountVMInvokesMount(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultConfig()
	cfg.RebaseRootDir(t.TempDir())
	vmID := "vm-test-overlay-mount"

	mountedCalled := false
	var gotTarget, gotData string
	var gotFlags uintptr
	om := &overlayRuntimeManager{
		cfg: cfg,
		mountFn: func(source, target, fstype string, flags uintptr, data string) error {
			mountedCalled = true
			gotTarget = target
			gotData = data
			gotFlags = flags
			return nil
		},
		unmountFn: overlayUnmount,
		readMountInfoFn: func() ([]byte, error) {
			return []byte(""), nil
		},
	}

	lowerDirs := []string{"/cache/rootfs", "/cache/custom"}
	if err := om.MountVM(vmID, lowerDirs); err != nil {
		t.Fatalf("MountVM: %v", err)
	}
	if !mountedCalled {
		t.Fatal("expected mount to be invoked")
	}
	if gotTarget != cfg.VMOCIMergedDir(vmID) {
		t.Fatalf("mount target = %q, want %q", gotTarget, cfg.VMOCIMergedDir(vmID))
	}
	if !strings.Contains(gotData, "lowerdir=/cache/rootfs:/cache/custom") {
		t.Fatalf("mount data = %q, expected lowerdir list", gotData)
	}
	if gotFlags != overlayMountFlags() {
		t.Fatalf("mount flags = %d, want %d", gotFlags, overlayMountFlags())
	}
}

func TestOverlayRuntimeMountVMNoopWhenAlreadyMounted(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultConfig()
	cfg.RebaseRootDir(t.TempDir())
	vmID := "vm-test-overlay-mounted"
	merged := cfg.VMOCIMergedDir(vmID)

	om := &overlayRuntimeManager{
		cfg: cfg,
		mountFn: func(source, target, fstype string, flags uintptr, data string) error {
			t.Fatal("mount should not be called when already mounted")
			return nil
		},
		unmountFn: overlayUnmount,
		readMountInfoFn: func() ([]byte, error) {
			line := "24 21 0:22 / " + merged + " rw,relatime - overlay overlay rw"
			return []byte(line + "\n"), nil
		},
	}

	if err := om.MountVM(vmID, []string{"/cache/rootfs"}); err != nil {
		t.Fatalf("MountVM: %v", err)
	}
}

func TestOverlayRuntimeUnmountVM(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultConfig()
	cfg.RebaseRootDir(t.TempDir())
	vmID := "vm-test-overlay-unmount"
	merged := cfg.VMOCIMergedDir(vmID)

	unmountCalled := false
	om := &overlayRuntimeManager{
		cfg:     cfg,
		mountFn: overlayMount,
		unmountFn: func(target string, flags int) error {
			unmountCalled = true
			if target != merged {
				t.Fatalf("unmount target = %q, want %q", target, merged)
			}
			return nil
		},
		readMountInfoFn: func() ([]byte, error) {
			line := "24 21 0:22 / " + merged + " rw,relatime - overlay overlay rw"
			return []byte(line + "\n"), nil
		},
	}

	if err := om.UnmountVM(vmID); err != nil {
		t.Fatalf("UnmountVM: %v", err)
	}
	if !unmountCalled {
		t.Fatal("expected unmount to be invoked")
	}
}

func TestOverlayRuntimeUnmountVMNoopWhenNotMounted(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultConfig()
	cfg.RebaseRootDir(t.TempDir())
	vmID := "vm-test-overlay-unmounted"

	om := &overlayRuntimeManager{
		cfg:     cfg,
		mountFn: overlayMount,
		unmountFn: func(target string, flags int) error {
			t.Fatal("unmount should not be called when not mounted")
			return nil
		},
		readMountInfoFn: func() ([]byte, error) {
			return []byte(""), nil
		},
	}

	if err := om.UnmountVM(vmID); err != nil {
		t.Fatalf("UnmountVM: %v", err)
	}
}

func TestUnescapeMountInfoPath(t *testing.T) {
	t.Parallel()

	got := unescapeMountInfoPath(`/path\040with\040space/\134data`)
	want := `/path with space/\data`
	if got != want {
		t.Fatalf("unescapeMountInfoPath = %q, want %q", got, want)
	}
}

package engine

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/CMGS/cocoon/config"
)

type (
	overlayMountFunc       func(source, target, fstype string, flags uintptr, data string) error
	overlayUnmountFunc     func(target string, flags int) error
	overlayMountInfoReader func() ([]byte, error)
)

type overlayRuntimeManager struct {
	cfg             *config.CocoonConfig
	mountFn         overlayMountFunc
	unmountFn       overlayUnmountFunc
	readMountInfoFn overlayMountInfoReader
	unmountTimeout  time.Duration
	unmountInterval time.Duration
}

func newOverlayRuntimeManager(cfg *config.CocoonConfig) *overlayRuntimeManager {
	return &overlayRuntimeManager{
		cfg:             cfg,
		mountFn:         overlayMount,
		unmountFn:       overlayUnmount,
		readMountInfoFn: overlayReadMountInfo,
		unmountTimeout:  2 * time.Second,
		unmountInterval: 100 * time.Millisecond,
	}
}

func (m *overlayRuntimeManager) EnsureWorkspace(vmID string) error {
	dirs := []string{
		m.cfg.VMOCIUpperDir(vmID),
		m.cfg.VMOCIWorkDir(vmID),
		m.cfg.VMOCIMergedDir(vmID),
	}
	for _, dir := range dirs {
		if err := ensureOverlayDir(dir); err != nil {
			return err
		}
	}
	return nil
}

func (m *overlayRuntimeManager) MountVM(vmID string, lowerDirs []string) error {
	if !overlayRuntimeSupported() {
		return fmt.Errorf("overlay runtime mounts are only supported on linux")
	}

	if err := m.EnsureWorkspace(vmID); err != nil {
		return err
	}

	data, err := buildOverlayMountData(lowerDirs, m.cfg.VMOCIUpperDir(vmID), m.cfg.VMOCIWorkDir(vmID))
	if err != nil {
		return err
	}

	mounted, err := m.IsMounted(vmID)
	if err != nil {
		return err
	}
	if mounted {
		return nil
	}

	if err := m.mountFn("overlay", m.cfg.VMOCIMergedDir(vmID), "overlay", overlayMountFlags(), data); err != nil {
		return fmt.Errorf("mount overlay for %s: %w", vmID, err)
	}
	return nil
}

func (m *overlayRuntimeManager) UnmountVM(vmID string) error {
	if !overlayRuntimeSupported() {
		return nil
	}

	mounted, err := m.IsMounted(vmID)
	if err != nil {
		return err
	}
	if !mounted {
		return nil
	}

	if err := m.unmountFn(m.cfg.VMOCIMergedDir(vmID), overlayUnmountFlags()); err != nil {
		return fmt.Errorf("unmount overlay for %s: %w", vmID, err)
	}
	if err := m.waitForUnmount(m.cfg.VMOCIMergedDir(vmID)); err != nil {
		log.Printf("warning: overlay mount for %s still present after lazy unmount: %v", vmID, err)
	}
	return nil
}

func (m *overlayRuntimeManager) IsMounted(vmID string) (bool, error) {
	if !overlayRuntimeSupported() {
		return false, nil
	}
	return m.isMountPoint(m.cfg.VMOCIMergedDir(vmID))
}

func (m *overlayRuntimeManager) isMountPoint(path string) (bool, error) {
	data, err := m.readMountInfoFn()
	if err != nil {
		return false, fmt.Errorf("read mountinfo: %w", err)
	}

	target := filepath.Clean(path)
	for line := range strings.SplitSeq(string(data), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		mountPoint := unescapeMountInfoPath(fields[4])
		if filepath.Clean(mountPoint) == target {
			return true, nil
		}
	}
	return false, nil
}

func (m *overlayRuntimeManager) waitForUnmount(path string) error {
	timeout := m.unmountTimeout
	if timeout <= 0 {
		return nil
	}
	interval := m.unmountInterval
	if interval <= 0 {
		interval = 20 * time.Millisecond
	}

	deadline := time.Now().Add(timeout)
	for {
		mounted, err := m.isMountPoint(path)
		if err != nil {
			return err
		}
		if !mounted {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("mount point %s still mounted after %s", path, timeout)
		}

		remaining := time.Until(deadline)
		sleepFor := interval
		sleepFor = min(remaining, sleepFor)
		time.Sleep(sleepFor)
	}
}

func buildOverlayMountData(lowerDirs []string, upperDir, workDir string) (string, error) {
	if len(lowerDirs) == 0 {
		return "", fmt.Errorf("lowerDirs must not be empty")
	}
	for _, path := range append(append([]string{}, lowerDirs...), upperDir, workDir) {
		if strings.TrimSpace(path) == "" {
			return "", fmt.Errorf("overlay path must not be empty")
		}
		if strings.ContainsAny(path, ":,") {
			return "", fmt.Errorf("overlay path must not contain colon or comma: %s", path)
		}
	}

	lower := strings.Join(lowerDirs, ":")
	return fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lower, upperDir, workDir), nil
}

func ensureOverlayDir(path string) error {
	if err := os.MkdirAll(path, 0o755); err != nil { //nolint:gosec // directories are VM runtime state
		return fmt.Errorf("create overlay directory %s: %w", path, err)
	}
	return nil
}

func unescapeMountInfoPath(path string) string {
	replacer := strings.NewReplacer(
		`\040`, " ",
		`\011`, "\t",
		`\012`, "\n",
		`\134`, "\\",
	)
	return replacer.Replace(path)
}

package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/types"
	"github.com/CMGS/cocoon/utils"
)

type ociRuntimePreflightDeps struct {
	goos       string
	readFileFn func(path string) ([]byte, error)
	lookPathFn func(file string) (string, error)
	runFn      func(ctx context.Context, binary string, args ...string) ([]byte, error)
}

func (m *manager) ensureOCIRuntimePreflight(ctx context.Context) error {
	return checkOCIRuntimePreflight(ctx, m.cfg, ociRuntimePreflightDeps{
		goos:       runtime.GOOS,
		readFileFn: os.ReadFile,
		lookPathFn: exec.LookPath,
		runFn: func(ctx context.Context, binary string, args ...string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, binary, args...) //nolint:gosec // binary path is trusted from config/PATH
			return cmd.CombinedOutput()
		},
	})
}

func checkOCIRuntimePreflight(ctx context.Context, cfg *config.CocoonConfig, deps ociRuntimePreflightDeps) error {
	if deps.goos != "linux" {
		return fmt.Errorf("OCI VM runtime requires Linux host (detected %s)", deps.goos)
	}

	filesystems, err := deps.readFileFn("/proc/filesystems")
	if err != nil {
		return fmt.Errorf("OCI VM runtime requires OverlayFS support (read /proc/filesystems): %w", err)
	}
	if !overlayListedInProcFilesystems(filesystems) {
		// Fallback: overlay may be available as an unloaded kernel module.
		if !overlayModuleAvailable(ctx, deps) {
			return fmt.Errorf("OCI VM runtime requires OverlayFS support: /proc/filesystems does not list overlay and modinfo overlay probe failed")
		}
	}

	virtioMin := utils.SemVersion{Major: 1, Minor: 7, Patch: 0}
	if err := ensureBinaryMinVersion(ctx, deps, cfg.VirtiofsdBinary, types.DefaultVirtiofsdProcess, [][]string{{"--version"}, {"-V"}}, virtioMin); err != nil {
		return fmt.Errorf("OCI VM runtime preflight failed for virtiofsd: %w", err)
	}

	chMin := utils.SemVersion{Major: 38, Minor: 0, Patch: 0}
	if err := ensureBinaryMinVersion(ctx, deps, cfg.CHBinary, types.DefaultHypervisorProcess, [][]string{{"--version"}}, chMin); err != nil {
		return fmt.Errorf("OCI VM runtime preflight failed for cloud-hypervisor: %w", err)
	}

	return nil
}

// overlayModuleAvailable probes for the overlay kernel module using modinfo.
// Returns true if the module is available (even if not currently loaded).
func overlayModuleAvailable(ctx context.Context, deps ociRuntimePreflightDeps) bool {
	modinfoBin, err := deps.lookPathFn("modinfo")
	if err != nil {
		return false
	}
	_, err = deps.runFn(ctx, modinfoBin, "overlay")
	return err == nil
}

func overlayListedInProcFilesystems(data []byte) bool {
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 {
			continue
		}
		if fields[len(fields)-1] == "overlay" {
			return true
		}
	}
	return false
}

func ensureBinaryMinVersion(
	ctx context.Context,
	deps ociRuntimePreflightDeps,
	configuredBinary string,
	defaultBinary string,
	argVariants [][]string,
	min utils.SemVersion,
) error {
	binary := strings.TrimSpace(configuredBinary)
	if binary == "" {
		binary = defaultBinary
	}

	resolved, err := deps.lookPathFn(binary)
	if err != nil {
		return fmt.Errorf("binary %q not found in PATH: %w", binary, err)
	}

	versionOutput, err := probeBinaryVersion(ctx, deps, resolved, argVariants)
	if err != nil {
		return fmt.Errorf("query version for %s: %w", resolved, err)
	}

	version, err := utils.ParseSemVersion(string(versionOutput))
	if err != nil {
		return fmt.Errorf("parse version from %s output %q: %w", resolved, strings.TrimSpace(string(versionOutput)), err)
	}

	if utils.CompareSemVersion(version, min) < 0 {
		return fmt.Errorf("detected %s, minimum required %s", version.String(), min.String())
	}

	return nil
}

func probeBinaryVersion(ctx context.Context, deps ociRuntimePreflightDeps, binary string, argVariants [][]string) ([]byte, error) {
	if len(argVariants) == 0 {
		return nil, fmt.Errorf("no version args configured")
	}

	var (
		output   []byte
		attempts = make([]string, 0, len(argVariants))
	)
	for _, args := range argVariants {
		out, err := deps.runFn(ctx, binary, args...)
		if err == nil {
			output = out
			return output, nil
		}
		attempts = append(attempts, fmt.Sprintf("%q: %v", strings.Join(args, " "), err))
	}
	return nil, fmt.Errorf("all version probes failed for %s (%d attempt(s)): %s", binary, len(argVariants), strings.Join(attempts, "; "))
}

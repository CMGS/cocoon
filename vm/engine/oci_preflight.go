package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/types"
)

var ociRuntimeVersionRe = regexp.MustCompile(`(\d+)\.(\d+)(?:\.(\d+))?`)

type preflightSemVersion struct {
	Major int
	Minor int
	Patch int
}

type ociRuntimePreflightDeps struct {
	goos       string
	readFileFn func(path string) ([]byte, error)
	lookPathFn func(file string) (string, error)
	runFn      func(ctx context.Context, binary string, args ...string) ([]byte, error)
}

func defaultOCIRuntimePreflightDeps() ociRuntimePreflightDeps {
	return ociRuntimePreflightDeps{
		goos:       runtime.GOOS,
		readFileFn: os.ReadFile,
		lookPathFn: exec.LookPath,
		runFn: func(ctx context.Context, binary string, args ...string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, binary, args...) //nolint:gosec // binary path is trusted from config/PATH
			return cmd.CombinedOutput()
		},
	}
}

func (m *manager) ensureOCIRuntimePreflight(ctx context.Context) error {
	return checkOCIRuntimePreflight(ctx, m.cfg, defaultOCIRuntimePreflightDeps())
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
		return fmt.Errorf("OCI VM runtime requires OverlayFS support: /proc/filesystems does not list overlay")
	}

	virtioMin := preflightSemVersion{Major: 1, Minor: 7, Patch: 0}
	if err := ensureBinaryMinVersion(ctx, deps, cfg.VirtiofsdBinary, types.DefaultVirtiofsdProcess, [][]string{{"--version"}, {"-V"}}, virtioMin); err != nil {
		return fmt.Errorf("OCI VM runtime preflight failed for virtiofsd: %w", err)
	}

	chMin := preflightSemVersion{Major: 38, Minor: 0, Patch: 0}
	if err := ensureBinaryMinVersion(ctx, deps, cfg.CHBinary, types.DefaultHypervisorProcess, [][]string{{"--version"}}, chMin); err != nil {
		return fmt.Errorf("OCI VM runtime preflight failed for cloud-hypervisor: %w", err)
	}

	return nil
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
	min preflightSemVersion,
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

	version, err := parsePreflightSemVersion(string(versionOutput))
	if err != nil {
		return fmt.Errorf("parse version from %s output %q: %w", resolved, strings.TrimSpace(string(versionOutput)), err)
	}

	if comparePreflightSemVersion(version, min) < 0 {
		return fmt.Errorf("detected %s, minimum required %s", version.String(), min.String())
	}

	return nil
}

func probeBinaryVersion(ctx context.Context, deps ociRuntimePreflightDeps, binary string, argVariants [][]string) ([]byte, error) {
	if len(argVariants) == 0 {
		return nil, fmt.Errorf("no version args configured")
	}

	var (
		lastErr error
		output  []byte
	)
	for _, args := range argVariants {
		output, lastErr = deps.runFn(ctx, binary, args...)
		if lastErr == nil {
			return output, nil
		}
	}
	return nil, lastErr
}

func parsePreflightSemVersion(out string) (preflightSemVersion, error) {
	matches := ociRuntimeVersionRe.FindStringSubmatch(out)
	if len(matches) < 3 {
		return preflightSemVersion{}, fmt.Errorf("no semantic version found")
	}

	major, err := strconv.Atoi(matches[1])
	if err != nil {
		return preflightSemVersion{}, fmt.Errorf("parse major: %w", err)
	}
	minor, err := strconv.Atoi(matches[2])
	if err != nil {
		return preflightSemVersion{}, fmt.Errorf("parse minor: %w", err)
	}

	patch := 0
	if len(matches) >= 4 && strings.TrimSpace(matches[3]) != "" {
		patch, err = strconv.Atoi(matches[3])
		if err != nil {
			return preflightSemVersion{}, fmt.Errorf("parse patch: %w", err)
		}
	}

	return preflightSemVersion{Major: major, Minor: minor, Patch: patch}, nil
}

func comparePreflightSemVersion(a, b preflightSemVersion) int {
	switch {
	case a.Major != b.Major:
		if a.Major < b.Major {
			return -1
		}
		return 1
	case a.Minor != b.Minor:
		if a.Minor < b.Minor {
			return -1
		}
		return 1
	case a.Patch != b.Patch:
		if a.Patch < b.Patch {
			return -1
		}
		return 1
	default:
		return 0
	}
}

func (v preflightSemVersion) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

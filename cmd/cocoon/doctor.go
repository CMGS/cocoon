package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	cli "github.com/urfave/cli/v2"

	"github.com/CMGS/cocoon/config"
	"github.com/CMGS/cocoon/oci"
	"github.com/CMGS/cocoon/utils"
)

func doctorCommand() *cli.Command {
	return &cli.Command{
		Name:  "doctor",
		Usage: "Check system health and dependencies",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "fix",
				Usage: "attempt to fix issues automatically",
			},
			&cli.BoolFlag{
				Name:  "force",
				Usage: "kill zombie processes and force-fix stuck states (requires --fix)",
			},
			&cli.StringFlag{
				Name:  "format",
				Value: "table",
				Usage: "output format (table, json)",
			},
		},
		Action: doctorAction,
	}
}

// checkResult holds the outcome of a single dependency check.
type checkResult struct {
	Name   string `json:"name"`
	Status string `json:"status"` // "pass", "fail", or "warn"
	Detail string `json:"detail"`
}

var (
	doctorLookPath   = exec.LookPath
	doctorRunCommand = func(ctx context.Context, binary string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, binary, args...) //nolint:gosec // binary path is resolved from PATH or fixed command; args are fixed literals
		return cmd.CombinedOutput()
	}
	doctorStat     = os.Stat
	doctorLstat    = os.Lstat
	doctorReadlink = os.Readlink
	doctorRemove   = os.Remove
	doctorSymlink  = os.Symlink
	doctorMkdirAll = os.MkdirAll
)

var doctorVirtiofsdFallbackPaths = []string{
	"/usr/libexec/virtiofsd",
	"/usr/lib/qemu/virtiofsd",
	"/usr/lib/virtiofsd",
	"/usr/lib64/virtiofsd",
}

var doctorVirtiofsdLinkDir = "/usr/local/bin"

const (
	checkStatusPass = "pass"
	checkStatusFail = "fail"
	checkStatusWarn = "warn"

	packageManagerAPT = "apt"
	packageManagerDNF = "dnf"
)

func checkBinaryWithMinVersion(name, binary string, args []string, min utils.SemVersion, purpose string) checkResult {
	path, err := exec.LookPath(binary)
	if err != nil {
		return checkResult{
			Name:   name,
			Status: "fail",
			Detail: fmt.Sprintf("binary not found in PATH (%s)", purpose),
		}
	}

	cmd := exec.Command(path, args...) //nolint:gosec // binary path is resolved from PATH; args are fixed literals
	out, err := cmd.CombinedOutput()
	if err != nil {
		return checkResult{
			Name:   name,
			Status: "fail",
			Detail: fmt.Sprintf("failed to query version: %v", err),
		}
	}

	ver, err := utils.ParseSemVersion(string(out))
	if err != nil {
		return checkResult{
			Name:   name,
			Status: "fail",
			Detail: fmt.Sprintf("failed to parse version from output: %s", strings.TrimSpace(string(out))),
		}
	}

	if utils.CompareSemVersion(ver, min) < 0 {
		return checkResult{
			Name:   name,
			Status: "fail",
			Detail: fmt.Sprintf("detected %s, minimum required %s", ver.String(), min.String()),
		}
	}

	return checkResult{
		Name:   name,
		Status: "pass",
		Detail: fmt.Sprintf("%s (version %s)", path, ver.String()),
	}
}

// runDependencyChecks verifies system dependencies required by Cocoon.
func runDependencyChecks(app *appContext) []checkResult {
	var results []checkResult

	// 1. Check cloud-hypervisor binary.
	results = append(results, checkBinaryWithMinVersion(
		"cloud-hypervisor",
		app.cfg.CHBinary,
		[]string{"--version"},
		utils.SemVersion{Major: 38, Minor: 0, Patch: 0},
		"required hypervisor runtime",
	))

	// 1b. Check Linux OverlayFS availability for OCI runtime path.
	results = append(results, checkOverlayFSSupport())

	// 1c. Check virtiofsd binary + minimum version for OCI runtime path.
	results = append(results, checkVirtiofsdBinary(app.cfg.VirtiofsdBinary))

	// 2. Check UEFI firmware file (with fallback probing).
	if app.cfg.UEFIFirmwarePath != "" {
		results = append(results, checkUEFIFirmware(app.cfg.UEFIFirmwarePath))
	}

	// 4. Check qemu-img binary + minimum version.
	results = append(results, checkBinaryWithMinVersion(
		"qemu-img",
		"qemu-img",
		[]string{"--version"},
		utils.SemVersion{Major: 8, Minor: 0, Patch: 0},
		"required for qcow2 operations",
	))

	// 4b. Check ch-remote binary + minimum version.
	results = append(results, checkBinaryWithMinVersion(
		"ch-remote",
		"ch-remote",
		[]string{"--version"},
		utils.SemVersion{Major: 38, Minor: 0, Patch: 0},
		"required for Cloud Hypervisor API interactions",
	))

	// 4c. Check guestfish binary + minimum version.
	results = append(results, checkBinaryWithMinVersion(
		"guestfish",
		"guestfish",
		[]string{"--version"},
		utils.SemVersion{Major: 1, Minor: 50, Patch: 0},
		"required for OCI-to-qcow2 conversion",
	))

	// 4d. Check swtpm binary (TPM emulator).
	results = append(results, checkSwtpm())

	// 4e. Check virt-customize binary (optional — required for Cocoonfile builds).
	results = append(results, checkOptionalBinary(
		"virt-customize",
		"virt-customize",
		[]string{"--version"},
		"optional: required for Cocoonfile RUN/COPY steps",
	))

	// 5. Check KVM device.
	if _, err := os.Stat("/dev/kvm"); err != nil {
		results = append(results, checkResult{
			Name:   "kvm",
			Status: "fail",
			Detail: "/dev/kvm not available",
		})
	} else {
		results = append(results, checkResult{
			Name:   "kvm",
			Status: "pass",
			Detail: "/dev/kvm",
		})
	}

	// 6. Check directory structure.
	dirs := map[string]string{
		"root-dir":     app.cfg.RootDir,
		"runtime-dir":  app.cfg.RuntimeDir,
		"log-dir":      app.cfg.LogDir,
		"db-dir":       app.cfg.DBDir(),
		"vm-dir":       app.cfg.VMDir(),
		"cache-dir":    app.cfg.CacheDir(),
		"firmware-dir": app.cfg.FirmwareDir(),
	}
	for name, dir := range dirs {
		if info, err := os.Stat(dir); err != nil {
			results = append(results, checkResult{
				Name:   fmt.Sprintf("dir/%s", name),
				Status: "fail",
				Detail: fmt.Sprintf("%s does not exist", dir),
			})
		} else if !info.IsDir() {
			results = append(results, checkResult{
				Name:   fmt.Sprintf("dir/%s", name),
				Status: "fail",
				Detail: fmt.Sprintf("%s exists but is not a directory", dir),
			})
		} else {
			results = append(results, checkResult{
				Name:   fmt.Sprintf("dir/%s", name),
				Status: "pass",
				Detail: dir,
			})
		}
	}

	results = append(results, checkOCIBlobRefCleanupHealth())

	return results
}

func checkOCIBlobRefCleanupHealth() checkResult {
	failures := oci.BlobRefCleanupFailureCount()
	if failures == 0 {
		return checkResult{
			Name:   "oci/blob-ref-cleanup",
			Status: "pass",
			Detail: "no OCI blob-ref cleanup failures observed",
		}
	}
	return checkResult{
		Name:   "oci/blob-ref-cleanup",
		Status: "warn",
		Detail: fmt.Sprintf("%d OCI blob-ref cleanup failure(s) observed; run 'cocoon gc' and inspect logs for warnings", failures),
	}
}

// checkSwtpm checks that the swtpm binary is available and reports its version.
func checkSwtpm() checkResult {
	path, err := exec.LookPath("swtpm")
	if err != nil {
		return checkResult{
			Name:   "swtpm",
			Status: "fail",
			Detail: "binary not found in PATH (required for VM TPM 2.0 support)",
		}
	}

	// swtpm --version writes to stderr and exits 0.
	cmd := exec.Command(path, "--version") //nolint:gosec // binary path is resolved from PATH
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Binary found but version query failed — still usable.
		return checkResult{
			Name:   "swtpm",
			Status: "pass",
			Detail: fmt.Sprintf("%s (version unknown)", path),
		}
	}

	ver := strings.TrimSpace(string(out))
	if ver == "" {
		ver = "installed"
	}
	return checkResult{
		Name:   "swtpm",
		Status: "pass",
		Detail: fmt.Sprintf("%s (%s)", path, ver),
	}
}

func normalizeVirtiofsdBinary(configuredBinary string) string {
	binary := strings.TrimSpace(configuredBinary)
	if binary == "" {
		return "virtiofsd"
	}
	return binary
}

func findVirtiofsdFallbackBinary() (string, bool) {
	for _, candidate := range doctorVirtiofsdFallbackPaths {
		info, err := doctorStat(candidate)
		if err != nil || info.IsDir() {
			continue
		}
		if info.Mode()&0o111 == 0 {
			continue
		}
		return candidate, true
	}
	return "", false
}

func resolveVirtiofsdBinary(configuredBinary string) (string, bool, error) {
	binary := normalizeVirtiofsdBinary(configuredBinary)
	path, err := doctorLookPath(binary)
	if err == nil {
		return path, false, nil
	}

	fallback, ok := findVirtiofsdFallbackBinary()
	if ok {
		return fallback, true, nil
	}

	return "", false, fmt.Errorf("binary not found in PATH (required for OCI VM direct-boot runtime): %s", binary)
}

func queryVirtiofsdVersion(path string) (utils.SemVersion, error) {
	queries := [][]string{{"--version"}, {"-V"}}

	var (
		lastOutput string
		lastErr    error
	)
	for _, args := range queries {
		out, err := doctorRunCommand(context.TODO(), path, args...)
		if err != nil {
			lastErr = fmt.Errorf("failed to query version: %w", err)
			continue
		}

		lastOutput = strings.TrimSpace(string(out))
		ver, parseErr := utils.ParseSemVersion(lastOutput)
		if parseErr != nil {
			lastErr = fmt.Errorf("failed to parse version from output: %s", lastOutput)
			continue
		}
		return ver, nil
	}

	if lastErr != nil {
		return utils.SemVersion{}, lastErr
	}
	return utils.SemVersion{}, fmt.Errorf("failed to query version: no output")
}

func checkVirtiofsdBinary(configuredBinary string) checkResult {
	binary := normalizeVirtiofsdBinary(configuredBinary)
	path, fallbackUsed, err := resolveVirtiofsdBinary(binary)
	if err != nil {
		return checkResult{
			Name:   "virtiofsd",
			Status: "fail",
			Detail: err.Error(),
		}
	}

	if fallbackUsed {
		return checkResult{
			Name:   "virtiofsd",
			Status: "fail",
			Detail: fmt.Sprintf("configured command %q not found in PATH; discovered fallback at %s; run 'cocoon doctor --fix' to remediate", binary, path),
		}
	}

	ver, verErr := queryVirtiofsdVersion(path)
	if verErr != nil {
		return checkResult{
			Name:   "virtiofsd",
			Status: "fail",
			Detail: verErr.Error(),
		}
	}

	min := utils.SemVersion{Major: 1, Minor: 7, Patch: 0}
	if utils.CompareSemVersion(ver, min) < 0 {
		return checkResult{
			Name:   "virtiofsd",
			Status: "fail",
			Detail: fmt.Sprintf("detected %s, minimum required %s", ver.String(), min.String()),
		}
	}

	return checkResult{
		Name:   "virtiofsd",
		Status: "pass",
		Detail: fmt.Sprintf("%s (version %s)", path, ver.String()),
	}
}

func hasFailedCheck(checks []checkResult, name string) bool {
	for _, chk := range checks {
		if chk.Name == name && chk.Status == checkStatusFail {
			return true
		}
	}
	return false
}

func replaceCheckResult(checks []checkResult, replacement checkResult) []checkResult {
	for i, chk := range checks {
		if chk.Name == replacement.Name {
			checks[i] = replacement
			return checks
		}
	}
	return append(checks, replacement)
}

func detectDoctorPackageManager() (string, error) {
	if _, err := doctorLookPath("apt-get"); err == nil {
		return packageManagerAPT, nil
	}
	if _, err := doctorLookPath("dnf"); err == nil {
		return packageManagerDNF, nil
	}
	return "", fmt.Errorf("unsupported package manager; install virtiofsd manually or set config virtiofsd_binary to an absolute path")
}

func ensureVirtiofsdCommandLink(configuredBinary, target string) error {
	if strings.ContainsRune(configuredBinary, os.PathSeparator) {
		return fmt.Errorf("configured virtiofsd binary %q is path-like; cannot auto-link to %s (set virtiofsd_binary explicitly)", configuredBinary, target)
	}

	linkPath := filepath.Join(doctorVirtiofsdLinkDir, configuredBinary)
	if err := doctorMkdirAll(doctorVirtiofsdLinkDir, 0o755); err != nil {
		return fmt.Errorf("create link directory %s: %w", doctorVirtiofsdLinkDir, err)
	}

	if info, err := doctorLstat(linkPath); err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("refusing to replace existing non-symlink at %s; remove it manually or set virtiofsd_binary to an explicit path", linkPath)
		}
		current, readErr := doctorReadlink(linkPath)
		if readErr == nil && current == target {
			return nil
		}
		if rmErr := doctorRemove(linkPath); rmErr != nil {
			return fmt.Errorf("remove existing %s: %w", linkPath, rmErr)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect existing %s: %w", linkPath, err)
	}

	if err := doctorSymlink(target, linkPath); err != nil {
		return fmt.Errorf("create symlink %s -> %s: %w", linkPath, target, err)
	}
	return nil
}

func tryLinkVirtiofsdFallback(configuredBinary string) {
	fallback, ok := findVirtiofsdFallbackBinary()
	if !ok {
		return
	}
	_ = ensureVirtiofsdCommandLink(configuredBinary, fallback)
}

func remediateVirtiofsdDependency(ctx context.Context, configuredBinary string) error {
	binary := normalizeVirtiofsdBinary(configuredBinary)

	if _, err := doctorLookPath(binary); err != nil {
		tryLinkVirtiofsdFallback(binary)
		if _, checkErr := doctorLookPath(binary); checkErr == nil {
			return nil
		}
	}

	pkgManager, err := detectDoctorPackageManager()
	if err != nil {
		return err
	}

	switch pkgManager {
	case packageManagerAPT:
		if _, err := doctorRunCommand(ctx, "apt-get", "update", "-qq"); err != nil {
			return fmt.Errorf("apt-get update: %w", err)
		}
		if _, err := doctorRunCommand(ctx, "apt-get", "install", "-y", "-qq", "virtiofsd"); err != nil {
			return fmt.Errorf("apt-get install virtiofsd: %w", err)
		}
	case packageManagerDNF:
		if _, err := doctorRunCommand(ctx, "dnf", "install", "-y", "-q", "virtiofsd"); err != nil {
			return fmt.Errorf("dnf install virtiofsd: %w", err)
		}
	default:
		return fmt.Errorf("unsupported package manager: %s", pkgManager)
	}

	if _, err := doctorLookPath(binary); err == nil {
		return nil
	}

	if fallback, ok := findVirtiofsdFallbackBinary(); ok {
		if linkErr := ensureVirtiofsdCommandLink(binary, fallback); linkErr != nil {
			return fmt.Errorf("link fallback virtiofsd: %w", linkErr)
		}
		if _, err := doctorLookPath(binary); err == nil {
			return nil
		}
		return fmt.Errorf("configured command %q is not in PATH after remediation; fallback exists at %s", binary, fallback)
	}

	return fmt.Errorf("configured command %q still not found after remediation", binary)
}

func tryFixVirtiofsdDependency(ctx context.Context, configuredBinary string, checks []checkResult) []checkResult {
	if !hasFailedCheck(checks, "virtiofsd") {
		return checks
	}

	fixErr := remediateVirtiofsdDependency(ctx, configuredBinary)
	updated := checkVirtiofsdBinary(configuredBinary)
	if fixErr != nil {
		if updated.Status == checkStatusPass {
			updated.Status = checkStatusWarn
		}
		updated.Detail = fmt.Sprintf("%s; auto-fix error: %v", updated.Detail, fixErr)
	} else if updated.Status == checkStatusPass {
		updated.Detail = fmt.Sprintf("%s; auto-fix applied", updated.Detail)
	}
	return replaceCheckResult(checks, updated)
}

// checkOptionalBinary checks if a binary exists and reports its version.
// Unlike checkBinaryWithMinVersion, missing binaries are reported as "warn" not "fail".
func checkOptionalBinary(name, binary string, args []string, purpose string) checkResult {
	path, err := exec.LookPath(binary)
	if err != nil {
		return checkResult{
			Name:   name,
			Status: "warn",
			Detail: fmt.Sprintf("not found in PATH (%s)", purpose),
		}
	}

	cmd := exec.Command(path, args...) //nolint:gosec // binary path is resolved from PATH; args are fixed literals
	out, err := cmd.CombinedOutput()
	if err != nil {
		return checkResult{
			Name:   name,
			Status: "warn",
			Detail: fmt.Sprintf("%s (version unknown)", path),
		}
	}

	ver, err := utils.ParseSemVersion(string(out))
	if err != nil {
		return checkResult{
			Name:   name,
			Status: "warn",
			Detail: fmt.Sprintf("%s (version: %s)", path, strings.TrimSpace(string(out))),
		}
	}

	return checkResult{
		Name:   name,
		Status: "pass",
		Detail: fmt.Sprintf("%s (version %s)", path, ver.String()),
	}
}

// checkUEFIFirmware checks the primary UEFI firmware path and probes
// deprecated system OVMF fallback paths if the primary is missing.
func checkUEFIFirmware(primaryPath string) checkResult {
	if _, err := os.Stat(primaryPath); err == nil {
		return checkResult{Name: "uefi-firmware", Status: "pass", Detail: primaryPath}
	}
	for _, fb := range config.UEFIFallbackPaths {
		if _, err := os.Stat(fb); err == nil {
			return checkResult{
				Name:   "uefi-firmware",
				Status: "warn",
				Detail: fmt.Sprintf("primary %s missing; using deprecated fallback %s", primaryPath, fb),
			}
		}
	}
	return checkResult{
		Name:   "uefi-firmware",
		Status: "fail",
		Detail: fmt.Sprintf("not found at %s or system fallback paths", primaryPath),
	}
}

func checkOverlayFSSupport() checkResult {
	if runtime.GOOS != hostOSLinux {
		return checkResult{
			Name:   "overlayfs",
			Status: "warn",
			Detail: fmt.Sprintf("OverlayFS is Linux-only (not applicable on %s)", runtime.GOOS),
		}
	}

	data, err := os.ReadFile("/proc/filesystems") //nolint:gosec // fixed kernel pseudo-file on linux
	if err != nil {
		return checkResult{
			Name:   "overlayfs",
			Status: "fail",
			Detail: fmt.Sprintf("read /proc/filesystems: %v", err),
		}
	}

	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 {
			continue
		}
		if fields[len(fields)-1] == "overlay" {
			return checkResult{
				Name:   "overlayfs",
				Status: "pass",
				Detail: "/proc/filesystems lists overlay",
			}
		}
	}

	return checkResult{
		Name:   "overlayfs",
		Status: "fail",
		Detail: "/proc/filesystems does not list overlay",
	}
}

func doctorAction(c *cli.Context) error {
	app, err := initApp(c)
	if err != nil {
		return err
	}

	fix := c.Bool("fix")
	force := c.Bool("force")
	format := c.String("format")

	if force && !fix {
		return fmt.Errorf("--force requires --fix; run 'cocoon doctor --fix --force'")
	}

	// Phase 1: Dependency checks.
	checks := runDependencyChecks(app)
	if fix {
		checks = tryFixVirtiofsdDependency(c.Context, app.cfg.VirtiofsdBinary, checks)
	}

	if format == formatJSON {
		// In JSON mode, collect everything and print at the end.
		type doctorOutput struct {
			Dependencies []checkResult `json:"dependencies"`
			Issues       any           `json:"issues"`
		}

		issues, reconcileErr := app.vmMgr.Reconcile(c.Context, fix, force)
		if reconcileErr != nil {
			return fmt.Errorf("reconcile: %w", reconcileErr)
		}

		if err := printJSON(doctorOutput{
			Dependencies: checks,
			Issues:       issues,
		}); err != nil {
			return err
		}

		failCount := 0
		for _, chk := range checks {
			if chk.Status == checkStatusFail {
				failCount++
			}
		}
		if failCount > 0 || len(issues) > 0 {
			return cli.Exit("", 1)
		}
		return nil
	}

	// Table mode: print dependency check results.
	fmt.Println("=== Dependency Checks ===")
	depHeaders := []string{"CHECK", "STATUS", "DETAIL"}
	depRows := make([][]string, 0, len(checks))
	failCount := 0
	for _, chk := range checks {
		status := chk.Status
		if chk.Status == checkStatusFail {
			failCount++
		}
		depRows = append(depRows, []string{chk.Name, status, chk.Detail})
	}
	printTable(depHeaders, depRows)

	if failCount > 0 {
		fmt.Printf("\n%d dependency check(s) failed.\n", failCount)
	} else {
		fmt.Println("\nAll dependency checks passed.")
	}

	// Phase 2: VM reconciliation.
	fmt.Println("\n=== VM Reconciliation ===")
	issues, reconcileErr := app.vmMgr.Reconcile(c.Context, fix, force)
	if reconcileErr != nil {
		return fmt.Errorf("reconcile: %w", reconcileErr)
	}

	if len(issues) == 0 {
		fmt.Println("No VM issues found.")
		if failCount > 0 {
			return cli.Exit("", 1)
		}
		return nil
	}

	headers := []string{"VM ID", "TYPE", "SEVERITY", "DETAILS"}
	rows := make([][]string, 0, len(issues))
	for _, issue := range issues {
		rows = append(rows, []string{
			truncateID(issue.VMID, 16),
			string(issue.Type),
			string(issue.Severity),
			issue.Details,
		})
	}
	printTable(headers, rows)

	if fix {
		fmt.Printf("\nAttempted to fix %d issue(s).\n", len(issues))
	} else {
		fmt.Printf("\nFound %d issue(s). Run with --fix to attempt automatic repair.\n", len(issues))
	}

	if failCount > 0 || len(issues) > 0 {
		return cli.Exit("", 1)
	}
	return nil
}

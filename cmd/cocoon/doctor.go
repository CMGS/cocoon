package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	cli "github.com/urfave/cli/v2"

	"github.com/CMGS/cocoon/config"
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
	Status string `json:"status"` // "pass" or "fail"
	Detail string `json:"detail"`
}

type semVersion struct {
	Major int
	Minor int
	Patch int
}

var versionRe = regexp.MustCompile(`(\d+)\.(\d+)(?:\.(\d+))?`)

func parseSemVersion(out string) (semVersion, error) {
	matches := versionRe.FindStringSubmatch(out)
	if len(matches) < 3 {
		return semVersion{}, fmt.Errorf("no semantic version found in output")
	}

	major, err := strconv.Atoi(matches[1])
	if err != nil {
		return semVersion{}, fmt.Errorf("parse major version: %w", err)
	}
	minor, err := strconv.Atoi(matches[2])
	if err != nil {
		return semVersion{}, fmt.Errorf("parse minor version: %w", err)
	}
	patch := 0
	if len(matches) >= 4 && matches[3] != "" {
		patch, err = strconv.Atoi(matches[3])
		if err != nil {
			return semVersion{}, fmt.Errorf("parse patch version: %w", err)
		}
	}

	return semVersion{Major: major, Minor: minor, Patch: patch}, nil
}

func compareSemVersion(a, b semVersion) int {
	if a.Major != b.Major {
		if a.Major < b.Major {
			return -1
		}
		return 1
	}
	if a.Minor != b.Minor {
		if a.Minor < b.Minor {
			return -1
		}
		return 1
	}
	if a.Patch != b.Patch {
		if a.Patch < b.Patch {
			return -1
		}
		return 1
	}
	return 0
}

func (v semVersion) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

func checkBinaryWithMinVersion(name, binary string, args []string, min semVersion, purpose string) checkResult {
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

	ver, err := parseSemVersion(string(out))
	if err != nil {
		return checkResult{
			Name:   name,
			Status: "fail",
			Detail: fmt.Sprintf("failed to parse version from output: %s", strings.TrimSpace(string(out))),
		}
	}

	if compareSemVersion(ver, min) < 0 {
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
		semVersion{Major: 38, Minor: 0, Patch: 0},
		"required hypervisor runtime",
	))

	// 2. Check PVH firmware file.
	if app.cfg.PVHFirmwarePath != "" {
		if _, err := os.Stat(app.cfg.PVHFirmwarePath); err != nil {
			results = append(results, checkResult{
				Name:   "pvh-firmware",
				Status: "fail",
				Detail: fmt.Sprintf("not found at %s", app.cfg.PVHFirmwarePath),
			})
		} else {
			results = append(results, checkResult{
				Name:   "pvh-firmware",
				Status: "pass",
				Detail: app.cfg.PVHFirmwarePath,
			})
		}
	}

	// 3. Check UEFI firmware file (with fallback probing).
	if app.cfg.UEFIFirmwarePath != "" {
		results = append(results, checkUEFIFirmware(app.cfg.UEFIFirmwarePath))
	}

	// 4. Check qemu-img binary + minimum version.
	results = append(results, checkBinaryWithMinVersion(
		"qemu-img",
		"qemu-img",
		[]string{"--version"},
		semVersion{Major: 8, Minor: 0, Patch: 0},
		"required for qcow2 operations",
	))

	// 4b. Check ch-remote binary + minimum version.
	results = append(results, checkBinaryWithMinVersion(
		"ch-remote",
		"ch-remote",
		[]string{"--version"},
		semVersion{Major: 38, Minor: 0, Patch: 0},
		"required for Cloud Hypervisor API interactions",
	))

	// 4c. Check buildah binary + minimum version.
	results = append(results, checkBinaryWithMinVersion(
		"buildah",
		"buildah",
		[]string{"version"},
		semVersion{Major: 1, Minor: 35, Patch: 0},
		"required for OCI image operations",
	))

	// 4d. Check skopeo binary + minimum version.
	results = append(results, checkBinaryWithMinVersion(
		"skopeo",
		"skopeo",
		[]string{"--version"},
		semVersion{Major: 1, Minor: 14, Patch: 0},
		"required for OCI manifest inspection",
	))

	// 4e. Check guestfish binary + minimum version.
	results = append(results, checkBinaryWithMinVersion(
		"guestfish",
		"guestfish",
		[]string{"--version"},
		semVersion{Major: 1, Minor: 50, Patch: 0},
		"required for OCI-to-qcow2 conversion",
	))

	// 4f. Check swtpm binary (TPM emulator).
	results = append(results, checkSwtpm())

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
		"buildah-dir":  app.cfg.BuildahRoot,
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

	return results
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
			if chk.Status == "fail" {
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
		if chk.Status == "fail" {
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

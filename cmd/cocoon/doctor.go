package main

import (
	"fmt"
	"os"
	"os/exec"

	cli "github.com/urfave/cli/v2"
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
				Usage: "force re-check even if cached results exist",
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

// runDependencyChecks verifies system dependencies required by Cocoon.
func runDependencyChecks(app *appContext) []checkResult {
	var results []checkResult

	// 1. Check cloud-hypervisor binary.
	chBinary := app.cfg.CHBinary
	if chPath, err := exec.LookPath(chBinary); err != nil {
		results = append(results, checkResult{
			Name:   "cloud-hypervisor",
			Status: "fail",
			Detail: fmt.Sprintf("binary %q not found in PATH", chBinary),
		})
	} else {
		results = append(results, checkResult{
			Name:   "cloud-hypervisor",
			Status: "pass",
			Detail: chPath,
		})
	}

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

	// 3. Check UEFI firmware file.
	if app.cfg.UEFIFirmwarePath != "" {
		if _, err := os.Stat(app.cfg.UEFIFirmwarePath); err != nil {
			results = append(results, checkResult{
				Name:   "uefi-firmware",
				Status: "fail",
				Detail: fmt.Sprintf("not found at %s", app.cfg.UEFIFirmwarePath),
			})
		} else {
			results = append(results, checkResult{
				Name:   "uefi-firmware",
				Status: "pass",
				Detail: app.cfg.UEFIFirmwarePath,
			})
		}
	}

	// 4. Check qemu-img binary.
	if qemuPath, err := exec.LookPath("qemu-img"); err != nil {
		results = append(results, checkResult{
			Name:   "qemu-img",
			Status: "fail",
			Detail: "binary not found in PATH",
		})
	} else {
		results = append(results, checkResult{
			Name:   "qemu-img",
			Status: "pass",
			Detail: qemuPath,
		})
	}

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

	return results
}

func doctorAction(c *cli.Context) error {
	app, err := initApp(c)
	if err != nil {
		return err
	}

	fix := c.Bool("fix")
	force := c.Bool("force")
	format := c.String("format")

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

		return printJSON(doctorOutput{
			Dependencies: checks,
			Issues:       issues,
		})
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

	return nil
}

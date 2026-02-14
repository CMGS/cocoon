//go:build linux

package pipeline

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// validateSafePath checks that a path contains only safe characters
// (alphanumeric, '/', '-', '_', '.') to prevent injection when the path
// is interpolated into a guestfish script string.
func validateSafePath(path string) error {
	for _, c := range path {
		isAlpha := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		isDigit := c >= '0' && c <= '9'
		isSafe := c == '/' || c == '-' || c == '_' || c == '.'
		if !isAlpha && !isDigit && !isSafe {
			return fmt.Errorf("unsafe character %q in path %q", string(c), path)
		}
	}
	return nil
}

// convertOCI converts an OCI rootfs mount into a bootable qcow2 image
// using qemu-img and guestfish.
func convertOCI(ctx context.Context, mountPath, outputPath, diskSize string) error {
	// Validate paths before interpolation into guestfish script to prevent injection.
	if err := validateSafePath(mountPath); err != nil {
		return fmt.Errorf("invalid mount path: %w", err)
	}
	if err := validateSafePath(outputPath); err != nil {
		return fmt.Errorf("invalid output path: %w", err)
	}

	// 1. Create empty qcow2 image.
	createCmd := exec.CommandContext(ctx, "qemu-img", "create", "-f", "qcow2", outputPath, diskSize) //nolint:gosec // args are controlled internal paths
	if out, err := createCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img create: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// 2. Check for guestfish.
	if _, err := exec.LookPath("guestfish"); err != nil {
		return fmt.Errorf("guestfish not found in PATH: OCI conversion requires libguestfs: %w", err)
	}

	// 3. Pack rootfs into a tar archive to preserve dotfiles and all top-level entries.
	rootfsTarPath := filepath.Join(filepath.Dir(outputPath), fmt.Sprintf(".rootfs-%d.tar", time.Now().UnixNano()))
	if err := validateSafePath(rootfsTarPath); err != nil {
		return fmt.Errorf("invalid rootfs tar path: %w", err)
	}
	tarCmd := exec.CommandContext(ctx, "tar", "-C", mountPath, "-cf", rootfsTarPath, ".") //nolint:gosec // args are validated internal paths
	if out, err := tarCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("pack rootfs tar: %s: %w", strings.TrimSpace(string(out)), err)
	}
	defer os.Remove(rootfsTarPath) //nolint:errcheck,gosec // best-effort temp cleanup

	// 4. Build guestfish script for partitioning and copying rootfs.
	// Paths are validated above to contain only safe characters.
	script := fmt.Sprintf(`add %s
run
part-init /dev/sda gpt
part-add /dev/sda primary 2048 206847
part-set-gpt-type /dev/sda 1 C12A7328-F81F-11D2-BA4B-00A0C93EC93B
part-add /dev/sda primary 206848 -1
part-set-gpt-type /dev/sda 2 0FC63DAF-8483-4772-8E79-3D69D8477DE4
mkfs fat /dev/sda1
mkfs ext4 /dev/sda2
mount /dev/sda2 /
mkdir-p /boot/efi
mount /dev/sda1 /boot/efi
tar-in %s /
sync
umount-all
`, outputPath, rootfsTarPath)

	// 5. Run guestfish with the script via stdin.
	gfCmd := exec.CommandContext(ctx, "guestfish") //nolint:gosec // guestfish script uses validated internal paths
	gfCmd.Stdin = strings.NewReader(script)
	if out, err := gfCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("guestfish: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// 6. Validate GRUB config presence and best-effort regeneration for serial console.
	if err := ensureGRUBConfig(ctx, outputPath); err != nil {
		return err
	}

	return nil
}

func ensureGRUBConfig(ctx context.Context, imagePath string) error {
	grubPath, found, err := detectGRUBConfigPath(ctx, imagePath)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("grub config not found in image (checked /boot/grub/grub.cfg and /boot/grub2/grub.cfg)")
	}

	// Best effort: if virt-customize exists, inject serial console param and regenerate grub config.
	// We don't hard-require virt-customize here to keep Phase 1 dependency surface unchanged.
	if _, err := exec.LookPath("virt-customize"); err != nil {
		return nil
	}

	cmdlineUpdate := "[ -f /etc/default/grub ] && (grep -q 'console=ttyS0,115200n8' /etc/default/grub || echo 'GRUB_CMDLINE_LINUX=\"$GRUB_CMDLINE_LINUX console=ttyS0,115200n8\"' >> /etc/default/grub) || true"
	regenerate := fmt.Sprintf("if command -v grub-mkconfig >/dev/null 2>&1; then grub-mkconfig -o %s; elif command -v grub2-mkconfig >/dev/null 2>&1; then grub2-mkconfig -o /boot/grub2/grub.cfg; fi", grubPath)

	customizeCmd := exec.CommandContext(ctx, "virt-customize", "-a", imagePath, "--run-command", cmdlineUpdate, "--run-command", regenerate) //nolint:gosec // args are fixed literals and validated paths
	if out, err := customizeCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("virt-customize grub update: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func detectGRUBConfigPath(ctx context.Context, imagePath string) (string, bool, error) {
	candidates := []string{
		"/boot/grub/grub.cfg",
		"/boot/grub2/grub.cfg",
	}
	for _, guestPath := range candidates {
		cmd := exec.CommandContext(ctx, "guestfish", "--ro", "-a", imagePath, "-i", "is-file", guestPath) //nolint:gosec // guestPath is from fixed list
		out, err := cmd.Output()
		if err != nil {
			return "", false, fmt.Errorf("guestfish check %s: %w", guestPath, err)
		}
		if strings.TrimSpace(string(out)) == "true" {
			return guestPath, true, nil
		}
	}
	return "", false, nil
}

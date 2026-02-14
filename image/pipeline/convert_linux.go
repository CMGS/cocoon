//go:build linux

package pipeline

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
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

	// 3. Build guestfish script for partitioning and copying rootfs.
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
copy-in %s/* /
sync
umount-all
`, outputPath, mountPath)

	// 4. Run guestfish with the script via stdin.
	gfCmd := exec.CommandContext(ctx, "guestfish") //nolint:gosec // guestfish script uses validated internal paths
	gfCmd.Stdin = strings.NewReader(script)
	if out, err := gfCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("guestfish: %s: %w", strings.TrimSpace(string(out)), err)
	}

	return nil
}

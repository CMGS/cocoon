//go:build linux

package pipeline

import (
	"context"
	"log"
	"os/exec"
	"strings"

	"github.com/CMGS/cocoon/image"
)

// guestfishTrue is the string guestfish returns for boolean true results.
const guestfishTrue = "true"

// deepVerifyBoot performs deep boot verification on Linux using guestfish.
// It mounts the qcow2 image read-only and checks for required boot components:
//   - Kernel: /boot/vmlinuz*
//   - Initrd/initramfs: /boot/initrd* or /boot/initramfs*
//   - systemd init: /sbin/init -> systemd
//   - UEFI bootloader in ESP
//
// If guestfish is not installed, this is non-fatal: a warning is logged and
// the function returns nil.
func deepVerifyBoot(ctx context.Context, imagePath string, result *image.BootCheckResult) error {
	// Check if guestfish is available.
	gfPath, err := exec.LookPath("guestfish")
	if err != nil {
		log.Printf("image verify: guestfish not found, skipping deep boot verification")
		result.Warnings = append(result.Warnings, "deep boot verification requires guestfish (not installed)")
		return nil
	}
	log.Printf("image verify: using guestfish at %s for deep verification", gfPath)

	// Check for kernel in /boot/.
	// On failure, log a warning and continue checking other components
	// rather than returning early (which would skip all remaining checks
	// and leave the caller with an incomplete but optimistic result).
	kernelOut, err := exec.CommandContext(ctx, "guestfish", "--ro", "-a", imagePath, "-i", "glob-expand", "/boot/vmlinuz*").Output() //nolint:gosec
	if err != nil {
		log.Printf("image verify: guestfish kernel check failed: %v (non-fatal)", err)
		result.Warnings = append(result.Warnings, "guestfish kernel check failed; results may be incomplete")
	} else {
		kernelFiles := strings.TrimSpace(string(kernelOut))
		if kernelFiles != "" {
			result.KernelFound = true
		}
	}

	// Check for initrd/initramfs in /boot/.
	initrdOut, err := exec.CommandContext(ctx, "guestfish", "--ro", "-a", imagePath, "-i", "glob-expand", "/boot/initr*").Output() //nolint:gosec
	if err == nil {
		initrdFiles := strings.TrimSpace(string(initrdOut))
		if initrdFiles != "" {
			result.InitrdFound = true
		}
	}
	if !result.InitrdFound {
		initramfsOut, errInitramfs := exec.CommandContext(ctx, "guestfish", "--ro", "-a", imagePath, "-i", "glob-expand", "/boot/initramfs*").Output() //nolint:gosec
		if errInitramfs == nil {
			initramfsFiles := strings.TrimSpace(string(initramfsOut))
			if initramfsFiles != "" {
				result.InitrdFound = true
			}
		}
	}

	// Check for systemd init.
	initOut, err := exec.CommandContext(ctx, "guestfish", "--ro", "-a", imagePath, "-i", "readlink", "/sbin/init").Output() //nolint:gosec
	if err == nil {
		initTarget := strings.TrimSpace(string(initOut))
		if strings.Contains(initTarget, "systemd") {
			result.SystemdFound = true
		}
	}
	// Also check if /sbin/init is systemd itself (some distros).
	if !result.SystemdFound {
		isFileOut, errSystemd := exec.CommandContext(ctx, "guestfish", "--ro", "-a", imagePath, "-i", "is-file", "/lib/systemd/systemd").Output() //nolint:gosec
		if errSystemd == nil && strings.TrimSpace(string(isFileOut)) == guestfishTrue {
			result.SystemdFound = true
		}
	}

	// Check for UEFI bootloader in ESP.
	// Include standard UEFI boot binaries and common shim/GRUB paths.
	uefiPaths := []string{
		"/boot/efi/EFI/BOOT/BOOTX64.EFI",
		"/boot/efi/EFI/BOOT/BOOTAA64.EFI",
		"/boot/efi/EFI/BOOT/bootx64.efi",
		"/boot/efi/EFI/BOOT/bootaa64.efi",
		"/boot/efi/EFI/BOOT/shimx64.efi",
		"/boot/efi/EFI/BOOT/grubx64.efi",
		"/boot/efi/EFI/BOOT/shimaa64.efi",
	}
	for _, uefiPath := range uefiPaths {
		isFileOut, errUEFI := exec.CommandContext(ctx, "guestfish", "--ro", "-a", imagePath, "-i", "is-file", uefiPath).Output() //nolint:gosec // uefiPath is from a hardcoded list, not user input
		if errUEFI == nil && strings.TrimSpace(string(isFileOut)) == guestfishTrue {
			result.BootloaderFound = true
			break
		}
	}

	return nil
}

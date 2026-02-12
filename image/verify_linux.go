//go:build linux

package image

import (
	"log"
	"os/exec"
	"strings"
)

// deepVerifyBoot performs deep boot verification on Linux using guestfish.
// It mounts the qcow2 image read-only and checks for required boot components:
//   - Kernel: /boot/vmlinuz*
//   - Initrd/initramfs: /boot/initrd* or /boot/initramfs*
//   - systemd init: /sbin/init -> systemd
//   - UEFI bootloader in ESP
//
// If guestfish is not installed, this is non-fatal: a warning is logged and
// the function returns nil.
func deepVerifyBoot(imagePath string, result *BootCheckResult) error {
	// Check if guestfish is available.
	gfPath, err := exec.LookPath("guestfish")
	if err != nil {
		log.Printf("image verify: guestfish not found, skipping deep boot verification")
		result.Warnings = append(result.Warnings, "deep boot verification requires guestfish (not installed)")
		return nil
	}
	log.Printf("image verify: using guestfish at %s for deep verification", gfPath)

	// Check for kernel in /boot/.
	kernelOut, err := exec.Command("guestfish", "--ro", "-a", imagePath, "-i", "glob-expand", "/boot/vmlinuz*").Output()
	if err != nil {
		log.Printf("image verify: guestfish kernel check failed: %v (non-fatal)", err)
		result.Warnings = append(result.Warnings, "guestfish kernel check failed; results may be incomplete")
		return nil
	}
	kernelFiles := strings.TrimSpace(string(kernelOut))
	if kernelFiles != "" {
		result.KernelFound = true
	}

	// Check for initrd/initramfs in /boot/.
	initrdOut, err := exec.Command("guestfish", "--ro", "-a", imagePath, "-i", "glob-expand", "/boot/initr*").Output()
	if err == nil {
		initrdFiles := strings.TrimSpace(string(initrdOut))
		if initrdFiles != "" {
			result.InitrdFound = true
		}
	}
	if !result.InitrdFound {
		initramfsOut, err := exec.Command("guestfish", "--ro", "-a", imagePath, "-i", "glob-expand", "/boot/initramfs*").Output()
		if err == nil {
			initramfsFiles := strings.TrimSpace(string(initramfsOut))
			if initramfsFiles != "" {
				result.InitrdFound = true
			}
		}
	}

	// Check for systemd init.
	initOut, err := exec.Command("guestfish", "--ro", "-a", imagePath, "-i", "readlink", "/sbin/init").Output()
	if err == nil {
		initTarget := strings.TrimSpace(string(initOut))
		if strings.Contains(initTarget, "systemd") {
			result.SystemdFound = true
		}
	}
	// Also check if /sbin/init is systemd itself (some distros).
	if !result.SystemdFound {
		isFileOut, err := exec.Command("guestfish", "--ro", "-a", imagePath, "-i", "is-file", "/lib/systemd/systemd").Output()
		if err == nil && strings.TrimSpace(string(isFileOut)) == "true" {
			result.SystemdFound = true
		}
	}

	// Check for UEFI bootloader in ESP.
	uefiPaths := []string{
		"/boot/efi/EFI/BOOT/BOOTX64.EFI",
		"/boot/efi/EFI/BOOT/BOOTAA64.EFI",
		"/boot/efi/EFI/BOOT/bootx64.efi",
		"/boot/efi/EFI/BOOT/bootaa64.efi",
	}
	for _, uefiPath := range uefiPaths {
		isFileOut, err := exec.Command("guestfish", "--ro", "-a", imagePath, "-i", "is-file", uefiPath).Output()
		if err == nil && strings.TrimSpace(string(isFileOut)) == "true" {
			result.BootloaderFound = true
			break
		}
	}

	// Check for cloud-init.
	cloudInitOut, err := exec.Command("guestfish", "--ro", "-a", imagePath, "-i", "is-file", "/usr/bin/cloud-init").Output()
	if err == nil && strings.TrimSpace(string(cloudInitOut)) == "true" {
		result.CloudInitFound = true
	}

	return nil
}

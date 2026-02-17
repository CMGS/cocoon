package oci

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// DetectKernel finds the highest-versioned kernel and its matching initrd
// from a list of /boot files. Returns an error if no kernel is found.
//
// Kernel detection per docs/04.1-oci-vm-images.md Section 3.3:
//   - Filters for vmlinuz-* files
//   - Sorts by version (numeric segment comparison)
//   - Picks highest version
//   - Matches initrd by version suffix (Debian: initrd.img-{ver}, RHEL: initramfs-{ver}.img)
func DetectKernel(bootFiles []string) (*KernelInfo, error) {
	type kernelCandidate struct {
		path    string
		version string
	}

	var candidates []kernelCandidate
	for _, f := range bootFiles {
		base := filepath.Base(f)
		if !strings.HasPrefix(base, "vmlinuz-") {
			continue
		}
		ver := strings.TrimPrefix(base, "vmlinuz-")
		if ver == "" {
			continue
		}
		candidates = append(candidates, kernelCandidate{path: f, version: ver})
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("no vmlinuz-* kernel found in /boot")
	}

	// Sort by version descending (highest first).
	sort.Slice(candidates, func(i, j int) bool {
		return compareVersions(candidates[i].version, candidates[j].version) > 0
	})

	best := candidates[0]

	// Build a set of boot files for initrd lookup.
	fileSet := make(map[string]struct{}, len(bootFiles))
	for _, f := range bootFiles {
		fileSet[filepath.Base(f)] = struct{}{}
	}

	// Try Debian-style first, then RHEL-style.
	initrdCandidates := []string{
		"initrd.img-" + best.version,
		"initramfs-" + best.version + ".img",
	}

	var initrdPath string
	for _, name := range initrdCandidates {
		if _, ok := fileSet[name]; ok {
			initrdPath = filepath.Join(filepath.Dir(best.path), name)
			break
		}
	}

	if initrdPath == "" {
		return nil, fmt.Errorf("no initrd found for kernel version %s (tried: %s)",
			best.version, strings.Join(initrdCandidates, ", "))
	}

	return &KernelInfo{
		Version:    best.version,
		KernelPath: best.path,
		InitrdPath: initrdPath,
	}, nil
}

// compareVersions compares two version strings by splitting on "." and "-",
// comparing segments numerically when possible, else lexicographically.
// Returns >0 if a > b, <0 if a < b, 0 if equal.
func compareVersions(a, b string) int {
	aParts := splitVersion(a)
	bParts := splitVersion(b)

	maxLen := max(len(aParts), len(bParts))

	for i := range maxLen {
		var ap, bp string
		if i < len(aParts) {
			ap = aParts[i]
		}
		if i < len(bParts) {
			bp = bParts[i]
		}

		aNum, aErr := strconv.Atoi(ap)
		bNum, bErr := strconv.Atoi(bp)

		if aErr == nil && bErr == nil {
			if aNum != bNum {
				return aNum - bNum
			}
			continue
		}

		// Fall back to lexicographic comparison.
		if ap != bp {
			if ap < bp {
				return -1
			}
			return 1
		}
	}
	return 0
}

// splitVersion splits a version string on "." and "-" delimiters.
func splitVersion(v string) []string {
	// Replace "-" with "." then split.
	return strings.Split(strings.ReplaceAll(v, "-", "."), ".")
}

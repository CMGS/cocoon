package utils

import (
	"cmp"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var semVersionRe = regexp.MustCompile(`(\d+)\.(\d+)(?:\.(\d+))?`)

// SemVersion represents a semantic version as major.minor.patch.
type SemVersion struct {
	Major int
	Minor int
	Patch int
}

// ParseSemVersion extracts the first semantic version from arbitrary command
// output (for example "cloud-hypervisor 38.0.0").
func ParseSemVersion(out string) (SemVersion, error) {
	matches := semVersionRe.FindStringSubmatch(out)
	if len(matches) < 3 {
		return SemVersion{}, fmt.Errorf("no semantic version found")
	}

	major, err := strconv.Atoi(matches[1])
	if err != nil {
		return SemVersion{}, fmt.Errorf("parse major: %w", err)
	}
	minor, err := strconv.Atoi(matches[2])
	if err != nil {
		return SemVersion{}, fmt.Errorf("parse minor: %w", err)
	}

	patch := 0
	if len(matches) >= 4 && strings.TrimSpace(matches[3]) != "" {
		patch, err = strconv.Atoi(matches[3])
		if err != nil {
			return SemVersion{}, fmt.Errorf("parse patch: %w", err)
		}
	}

	return SemVersion{Major: major, Minor: minor, Patch: patch}, nil
}

// CompareSemVersion compares two semantic versions.
// Returns -1 if a<b, 0 if a==b, and 1 if a>b.
func CompareSemVersion(a, b SemVersion) int {
	if n := cmp.Compare(a.Major, b.Major); n != 0 {
		return n
	}
	if n := cmp.Compare(a.Minor, b.Minor); n != 0 {
		return n
	}
	return cmp.Compare(a.Patch, b.Patch)
}

func (v SemVersion) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

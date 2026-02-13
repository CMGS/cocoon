package types

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	hexPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)
	validArchs = map[string]bool{
		"amd64":   true,
		"arm64":   true,
		"aarch64": true,
	}
)

// ReferenceEntry tracks a base image and the VMs referencing it.
type ReferenceEntry struct {
	Path       string   `json:"path"`
	DigestFull string   `json:"digest_full"`
	SourceRef  string   `json:"source_ref"`
	Refs       []string `json:"refs"`
	CreatedAt  string   `json:"created_at"`
}

// ReferencesFile is the top-level structure of references.json.
// Keys are base_key ({checksum_16}_{arch}).
type ReferencesFile map[string]*ReferenceEntry

// NameIndex maps user-assigned VM names to vm_ids.
type NameIndex map[string]string

// ParseBaseKey splits a base_key into its checksum and architecture components.
// Format: {checksum_16hex}_{arch}, e.g. "a1b2c3d4e5f6a7b8_amd64".
func ParseBaseKey(baseKey string) (checksum, arch string, err error) {
	if strings.ContainsAny(baseKey, "/\\") || strings.Contains(baseKey, "..") {
		return "", "", fmt.Errorf("invalid base_key: contains path traversal characters: %q", baseKey)
	}
	parts := strings.SplitN(baseKey, "_", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid base_key format: %q", baseKey)
	}
	if !hexPattern.MatchString(parts[0]) {
		return "", "", fmt.Errorf("invalid base_key checksum: must be 16 hex chars, got %q", parts[0])
	}
	if !validArchs[parts[1]] {
		return "", "", fmt.Errorf("invalid base_key arch %q: must be one of amd64, arm64, aarch64", parts[1])
	}
	return parts[0], parts[1], nil
}

// FormatBaseKey constructs a base_key from checksum and arch.
func FormatBaseKey(checksum, arch string) string {
	return checksum + "_" + arch
}

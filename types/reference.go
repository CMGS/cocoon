package types

import (
	"fmt"
	"strings"
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

// ParseBaseKey splits a base_key into (checksum, arch).
// Example: "a1b2c3d4e5f6a7b8_amd64" -> ("a1b2c3d4e5f6a7b8", "amd64")
func ParseBaseKey(baseKey string) (checksum, arch string, err error) {
	parts := strings.SplitN(baseKey, "_", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid base_key format: %q", baseKey)
	}
	return parts[0], parts[1], nil
}

// FormatBaseKey constructs a base_key from checksum and arch.
func FormatBaseKey(checksum, arch string) string {
	return checksum + "_" + arch
}

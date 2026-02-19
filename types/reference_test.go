package types

import "testing"

func TestParseBaseKey_NormalizesAArch64(t *testing.T) {
	t.Parallel()

	checksum, arch, err := ParseBaseKey("a1b2c3d4e5f6a7b8_aarch64")
	if err != nil {
		t.Fatalf("ParseBaseKey: %v", err)
	}
	if checksum != "a1b2c3d4e5f6a7b8" {
		t.Fatalf("checksum=%q, want a1b2c3d4e5f6a7b8", checksum)
	}
	if arch != "arm64" {
		t.Fatalf("arch=%q, want arm64", arch)
	}
}

func TestFormatBaseKey_NormalizesAArch64(t *testing.T) {
	t.Parallel()

	got := FormatBaseKey("a1b2c3d4e5f6a7b8", "aarch64")
	if got != "a1b2c3d4e5f6a7b8_arm64" {
		t.Fatalf("FormatBaseKey=%q, want a1b2c3d4e5f6a7b8_arm64", got)
	}
}

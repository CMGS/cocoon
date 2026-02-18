//go:build darwin

package pipeline

import (
	"context"

	"github.com/CMGS/cocoon/image"
)

// deepVerifyBoot performs platform-specific deep boot verification.
// On darwin, guestfish/libguestfs is not available, so deep verification
// is not supported. This is a non-fatal stub that appends a warning.
func deepVerifyBoot(_ context.Context, _ string, result *image.BootCheckResult) error {
	result.Warnings = append(result.Warnings, "deep boot verification not available on darwin")
	return nil
}

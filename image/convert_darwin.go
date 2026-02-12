//go:build darwin

package image

import (
	"context"
	"fmt"
)

func convertOCI(_ context.Context, _, _, _ string) error {
	return fmt.Errorf("OCI image conversion requires libguestfs (Linux only)")
}

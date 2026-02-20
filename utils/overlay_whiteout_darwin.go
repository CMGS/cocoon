//go:build darwin

package utils

import "fmt"

// createOverlayWhiteoutDevice is a stub for darwin where overlayfs is not
// available. Layers containing OCI whiteout entries cannot be extracted in
// overlay mode on this platform.
func createOverlayWhiteoutDevice(_ string) error {
	return fmt.Errorf("overlayfs whiteout devices require Linux")
}

// setOverlayOpaqueXattr is a stub for darwin where the trusted.overlay.opaque
// extended attribute is not supported.
func setOverlayOpaqueXattr(_ string) error {
	return fmt.Errorf("overlayfs opaque xattr requires Linux")
}

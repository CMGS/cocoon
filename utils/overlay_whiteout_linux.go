//go:build linux

package utils

import "golang.org/x/sys/unix"

// createOverlayWhiteoutDevice creates a character device with major:minor 0:0
// at the given path, which overlayfs interprets as a whiteout entry that hides
// the corresponding file in lower layers.
func createOverlayWhiteoutDevice(path string) error {
	return unix.Mknod(path, unix.S_IFCHR, 0)
}

// setOverlayOpaqueXattr sets the trusted.overlay.opaque=y extended attribute
// on a directory, which tells overlayfs to hide all entries from lower layers
// beneath this directory.
func setOverlayOpaqueXattr(dirPath string) error {
	return unix.Setxattr(dirPath, "trusted.overlay.opaque", []byte("y"), 0)
}

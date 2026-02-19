//go:build linux

package engine

import (
	"os"

	"golang.org/x/sys/unix"
)

func overlayRuntimeSupported() bool {
	return true
}

func overlayMount(source, target, fstype string, flags uintptr, data string) error {
	return unix.Mount(source, target, fstype, flags, data)
}

func overlayUnmount(target string, flags int) error {
	return unix.Unmount(target, flags)
}

func overlayReadMountInfo() ([]byte, error) {
	return os.ReadFile("/proc/self/mountinfo")
}

func overlayMountFlags() uintptr {
	return unix.MS_NOSUID | unix.MS_NODEV
}

func overlayUnmountFlags() int {
	return unix.MNT_DETACH
}

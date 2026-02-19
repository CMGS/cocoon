//go:build !linux

package engine

func overlayRuntimeSupported() bool {
	return false
}

func overlayMount(source, target, fstype string, flags uintptr, data string) error {
	return nil
}

func overlayUnmount(target string, flags int) error {
	return nil
}

func overlayReadMountInfo() ([]byte, error) {
	return nil, nil
}

func overlayMountFlags() uintptr {
	return 0
}

func overlayUnmountFlags() int {
	return 0
}

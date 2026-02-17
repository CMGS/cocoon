package main

import (
	"os"
	"os/signal"
	"syscall"
	"unsafe"

	"golang.org/x/term"
)

// handleSIGWINCH propagates the initial terminal size and listens for
// SIGWINCH to relay resize events to the PTY. It returns a cleanup function
// that stops the signal listener.
func handleSIGWINCH(local *os.File, remote *os.File) func() {
	// Propagate initial terminal size.
	propagateTerminalSize(local, remote)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)

	go func() {
		for range sigCh {
			propagateTerminalSize(local, remote)
		}
	}()

	// NOTE: We intentionally do not close sigCh. After signal.Stop, the
	// goroutine will block on the channel read until process exit. This
	// avoids a race where an in-flight signal delivery panics on a closed
	// channel. The leak is benign — the process exits after console returns.
	return func() {
		signal.Stop(sigCh)
	}
}

// propagateTerminalSize reads the local terminal dimensions and sets them
// on the remote PTY file descriptor.
func propagateTerminalSize(local *os.File, remote *os.File) {
	width, height, err := term.GetSize(int(local.Fd()))
	if err != nil {
		return
	}
	_ = setWinSize(remote, width, height)
}

// winSize matches the kernel's struct winsize for the TIOCSWINSZ ioctl.
type winSize struct {
	Rows uint16
	Cols uint16
	X    uint16 // unused pixel width
	Y    uint16 // unused pixel height
}

// setWinSize sets the terminal size on the given file descriptor using
// the TIOCSWINSZ ioctl.
func setWinSize(f *os.File, cols, rows int) error {
	ws := winSize{Rows: uint16(rows), Cols: uint16(cols)} //nolint:gosec // G115: int->uint16 is safe for terminal dimensions
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		f.Fd(),
		syscall.TIOCSWINSZ,
		uintptr(unsafe.Pointer(&ws)), //nolint:gosec // G103: unsafe needed for ioctl
	)
	if errno != 0 {
		return errno
	}
	return nil
}

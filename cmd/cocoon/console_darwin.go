package main

import "os"

// handleSIGWINCH is a no-op on darwin. Console functionality requires Linux.
// The consoleAction function checks for Linux-only features at runtime.
func handleSIGWINCH(_ *os.File, _ *os.File) func() {
	return func() {}
}

//go:build unix

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// stdinIsTerminal reports whether stdin is an interactive terminal.
func stdinIsTerminal() bool {
	_, err := unix.IoctlGetTermios(int(os.Stdin.Fd()), unix.TCGETS)
	return err == nil
}

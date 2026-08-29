package main

import "os"

// stdinIsTerminal reports whether stdin is an interactive terminal rather than
// a pipe or a file.
//
// This uses the file mode rather than a TCGETS ioctl because the ioctl
// constant differs per platform — TCGETS on Linux, TIOCGETA on Darwin — and
// getting that wrong breaks the cross-build for a check this simple. The
// character-device test answers the only question that matters here: whether
// there is a human on the other end who can answer an approval prompt.
func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

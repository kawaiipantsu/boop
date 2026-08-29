// Command boop is the Boop CLI, TUI, WebUI and agent runtime entrypoint.
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "boop:", err)
		os.Exit(1)
	}
}

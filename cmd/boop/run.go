package main

import (
	"flag"
	"fmt"
	"io"

	"github.com/boop-dev/boop/internal/version"
)

// options holds the parsed command-line startup configuration.
type options struct {
	prompt   string
	provider string
	model    string
	mode     string
	noTUI    bool
	web      bool
	gui      bool
	listen   string
	port     int
	logLevel string
	// dangerouslyUnrestricted bypasses confirmation entirely. It is
	// deliberately verbose and must never be needed for normal local work.
	dangerouslyUnrestricted bool
	showVersion             bool
}

// run parses arguments and dispatches to a startup mode.
func run(args []string, stdout, stderr io.Writer) error {
	var opts options
	fs := flag.NewFlagSet("boop", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opts.prompt, "prompt", "", "submit a prompt at startup")
	fs.StringVar(&opts.provider, "provider", "", "provider to use")
	fs.StringVar(&opts.model, "model", "", "model to use")
	fs.StringVar(&opts.mode, "mode", "", "execution mode: confirm or auto")
	fs.BoolVar(&opts.noTUI, "no-tui", false, "plain CLI mode")
	fs.BoolVar(&opts.web, "web", false, "start the local WebUI")
	fs.BoolVar(&opts.gui, "gui", false, "launch the native GUI")
	fs.StringVar(&opts.listen, "listen", "", "WebUI bind address")
	fs.IntVar(&opts.port, "port", 0, "WebUI port")
	fs.StringVar(&opts.logLevel, "log-level", "", "trace, debug, info, warn, error")
	fs.BoolVar(&opts.dangerouslyUnrestricted, "dangerously-unrestricted", false,
		"skip all permission confirmation (not required for normal local development)")
	fs.BoolVar(&opts.showVersion, "version", false, "print version and exit")

	fs.Usage = func() {
		fmt.Fprintf(stderr, "boop — local AI client and agent runtime\n\nusage:\n  boop [flags] [prompt]\n  boop version\n\nflags:\n")
		fs.PrintDefaults()
	}

	// `boop version` is accepted as a subcommand alongside --version.
	if len(args) > 0 && args[0] == "version" {
		fmt.Fprintln(stdout, version.Get())
		return nil
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if opts.showVersion {
		fmt.Fprintln(stdout, version.Get())
		return nil
	}
	if opts.prompt == "" && fs.NArg() > 0 {
		opts.prompt = fs.Arg(0)
	}

	return dispatch(opts, stdout, stderr)
}

// dispatch selects the startup mode. Modes are wired up as their milestones land.
func dispatch(opts options, stdout, stderr io.Writer) error {
	switch {
	case opts.gui:
		return fmt.Errorf("native GUI is not implemented yet (milestone 13)")
	case opts.web:
		return fmt.Errorf("WebUI is not implemented yet (milestone 9)")
	case opts.noTUI:
		return fmt.Errorf("plain CLI mode is not implemented yet (milestone 2)")
	default:
		return fmt.Errorf("TUI is not implemented yet (milestone 3); try `boop version`")
	}
}

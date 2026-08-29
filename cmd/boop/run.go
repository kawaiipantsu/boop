package main

import (
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/kawaiipantsu/boop/internal/version"
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
	// `boop version` is accepted as a subcommand alongside --version.
	if len(args) > 0 && args[0] == "version" {
		fmt.Fprintln(stdout, version.Get())
		return nil
	}
	opts, err := parse(args, stderr)
	if err != nil {
		return err
	}
	if opts.showVersion {
		fmt.Fprintln(stdout, version.Get())
		return nil
	}
	return dispatch(opts, stdout, stderr)
}

// parse turns command-line arguments into options.
func parse(args []string, stderr io.Writer) (options, error) {
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
		fmt.Fprint(stderr, usageText)
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	// Remaining arguments form a bare prompt, so an unquoted request works:
	//   boop serve this folder up via http
	// Flags are still honoured because flag parsing stops at the first
	// non-flag argument:
	//   boop --mode auto build me a static site
	if rest := fs.Args(); len(rest) > 0 {
		bare := strings.Join(rest, " ")
		if opts.prompt != "" {
			return opts, fmt.Errorf("prompt given both with --prompt and as arguments: %q and %q", opts.prompt, bare)
		}
		opts.prompt = bare
	}
	return opts, nil
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

// usageText documents invocation, including the bare-prompt form that lets a
// request be typed without quoting.
const usageText = `boop — local AI client and agent runtime

usage:
  boop                              start the TUI
  boop <prompt...>                  submit a prompt, then continue interactively
  boop --prompt "<text>"            same, when the text would confuse flag parsing
  boop version                      print build metadata

examples:
  boop serve this folder up via http
  boop build me a simple website in html, css and js about hacking
  boop --mode auto fix the failing tests

flags:
`

package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/kawaiipantsu/boop/internal/app"
	"github.com/kawaiipantsu/boop/internal/config"
	"github.com/kawaiipantsu/boop/internal/permissions"
	"github.com/kawaiipantsu/boop/internal/provider"
	"github.com/kawaiipantsu/boop/internal/session"
)

// runPlainCLI implements --no-tui: one prompt in, the answer out (§52).
//
// It is the mode scripts and CI use, so it stays line-oriented and writes only
// the answer to stdout; progress and approvals go to stderr, which keeps
// `boop --no-tui "..." > out.txt` useful.
func runPlainCLI(ctx context.Context, opts options, stdout, stderr io.Writer) error {
	prompt, err := resolvePrompt(opts)
	if err != nil {
		return err
	}

	cfg, err := loadConfig(opts)
	if err != nil {
		return err
	}

	approver := newCLIApprover(stderr, cfg.Execution.Mode)
	application, err := app.New(ctx, app.Options{
		Config:   cfg,
		Approver: approver,
		Stderr:   stderr,
		Verbose:  opts.verbose,
	})
	if err != nil {
		return err
	}
	defer application.Close()

	sess, err := application.Sessions.Create(ctx, session.CreateOptions{
		ProjectPath: application.Workspace.Root(),
		Provider:    cfg.Provider,
		Model:       cfg.Model,
	})
	if err != nil {
		return fmt.Errorf("cannot start a session: %w", err)
	}

	var memory string
	if application.Memory != nil {
		memory = string(application.Memory.Render())
	}
	system := app.PromptContext{
		OS: runtime.GOOS, Arch: runtime.GOARCH, Shell: os.Getenv("SHELL"),
		WorkingDir: application.Workspace.Root(),
		Provider:   cfg.Provider, Model: cfg.Model,
		Mode:      string(cfg.Execution.Mode),
		Tools:     application.Tools.Names(),
		NetworkOn: cfg.Network.Enabled,
		// Boop.md is already a summary, so it is included whole or not at all.
		ProjectInfo: memory,
	}.Render(application.SystemPrompt())

	history := []provider.Message{
		{Role: provider.RoleSystem, Content: system},
		{Role: provider.RoleUser, Content: prompt},
	}
	if _, err := application.Sessions.AppendMessage(ctx, sess.ID, history[1]); err != nil {
		fmt.Fprintln(stderr, "boop: could not record the prompt:", err)
	}

	if opts.verbose {
		attachProgress(application, stderr)
	}

	loop := application.NewLoop(sess.ID)
	turn, err := loop.Run(ctx, history)
	if err != nil {
		return err
	}

	for _, msg := range turn.Messages {
		if _, err := application.Sessions.AppendMessage(ctx, sess.ID, msg); err != nil {
			fmt.Fprintln(stderr, "boop: could not record a message:", err)
			break
		}
	}

	answer := strings.TrimSpace(turn.Text)
	if answer == "" {
		answer = "(the model produced no text)"
	}
	fmt.Fprintln(stdout, answer)

	if turn.StoppedAtLimit {
		fmt.Fprintf(stderr, "boop: stopped after %d tool iterations; the answer may be incomplete "+
			"(raise execution.max_tool_iterations)\n", turn.Iterations)
	}
	if opts.verbose {
		fmt.Fprintf(stderr, "boop: %s, session %s\n", turn, sess.ID)
	}
	return nil
}

// resolvePrompt takes the prompt from the command line, or from stdin when it
// is piped, so Boop composes in a shell pipeline.
//
// A terminal stdin is not read: that would hang waiting for input the user did
// not know to type.
func resolvePrompt(opts options) (string, error) {
	if p := strings.TrimSpace(opts.prompt); p != "" {
		return p, nil
	}
	if !stdinIsTerminal() {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", fmt.Errorf("reading the prompt from stdin: %w", err)
		}
		if p := strings.TrimSpace(string(data)); p != "" {
			return p, nil
		}
	}
	return "", fmt.Errorf("no prompt given; pass one as arguments, with --prompt, or on stdin")
}

// attachProgress reports tool activity on stderr so a long run is not silent.
func attachProgress(a *app.App, stderr io.Writer) {
	a.Bus.Subscribe(func(ev app.Event) {
		switch ev.Type {
		case app.EventToolRequested:
			if action, ok := ev.Payload.(permissions.Action); ok {
				fmt.Fprintf(stderr, "  → %s\n", action.Summary)
			}
		case app.EventToolCompleted:
			if m, ok := ev.Payload.(map[string]any); ok {
				status := "ok"
				if failed, _ := m["error"].(bool); failed {
					status = "failed"
				}
				fmt.Fprintf(stderr, "  ← %v %s (%v)\n", m["tool"], status, m["duration"])
			}
		}
	}, app.EventToolRequested, app.EventToolCompleted)
}

// cliApprover asks for confirmation on the terminal.
type cliApprover struct {
	out  io.Writer
	in   *bufio.Reader
	mode permissions.Mode
}

func newCLIApprover(out io.Writer, mode permissions.Mode) *cliApprover {
	return &cliApprover{out: out, in: bufio.NewReader(os.Stdin), mode: mode}
}

// Approve implements permissions.Approver.
//
// When stdin is not a terminal the action is refused rather than assumed: a
// script piping input must not have its pipe silently consumed as consent.
func (c *cliApprover) Approve(action permissions.Action) (bool, error) {
	if !stdinIsTerminal() {
		fmt.Fprintf(c.out, "boop: refusing %q — it needs approval and stdin is not a terminal.\n",
			action.Summary)
		return false, nil
	}
	fmt.Fprintf(c.out, "\n  %s\n", action.Summary)
	if action.Detail != "" && action.Detail != action.Summary {
		fmt.Fprintf(c.out, "  %s\n", action.Detail)
	}
	fmt.Fprintf(c.out, "  risk: %s", action.Risk)
	if action.Production {
		fmt.Fprint(c.out, "  ⚠ may affect production")
	}
	fmt.Fprint(c.out, "\n  allow? [y/N] ")

	line, err := c.in.ReadString('\n')
	if err != nil {
		return false, nil
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

// loadConfig loads configuration and applies command-line overrides.
func loadConfig(opts options) (*config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("cannot load the configuration: %w", err)
	}
	if opts.provider != "" {
		cfg.Provider = opts.provider
	}
	if opts.model != "" {
		cfg.Model = opts.model
	}
	if opts.mode != "" {
		mode := permissions.Mode(opts.mode)
		if !mode.Valid() {
			return nil, fmt.Errorf("--mode %q is not valid (want confirm or auto)", opts.mode)
		}
		cfg.Execution.Mode = mode
	}
	if opts.logLevel != "" {
		cfg.Logging.Level = opts.logLevel
	}
	// The flag was previously parsed and then dropped on the floor, so it
	// silently did nothing. It skips confirmation for ordinary categories;
	// the production gate still holds above it (§15, §64.14).
	cfg.Execution.Unrestricted = opts.dangerouslyUnrestricted
	return cfg, nil
}

// Package tui is Boop's terminal frontend.
//
// It is a view over internal/app and nothing more. Provider selection,
// permission decisions, tool execution and persistence all belong to the core;
// this package subscribes to the core's event bus, draws what it reports, and
// turns keystrokes back into calls on it (§2.3, §19, invariant §64.1).
//
// Three goroutines meet here and the boundaries between them are the reason
// most of this package exists:
//
//   - Bubble Tea's update goroutine owns the Model and is the only thing that
//     mutates it.
//   - The tool loop runs inside a tea.Cmd, because Loop.Run blocks for as long
//     as a model and its commands take.
//   - The event bus publishes on whichever goroutine produced the event.
//
// Events cross into the UI through the pump, which coalesces token bursts so
// generation speed is not bound to render speed. Approvals cross the other way
// through permissions.Broker: the loop parks in the queue and the UI answers
// it. Cancellation is wired through both, so an interrupted turn releases a
// parked approval instead of deadlocking (§51).
package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kawaiipantsu/boop/internal/app"
	"github.com/kawaiipantsu/boop/internal/config"
	"github.com/kawaiipantsu/boop/internal/permissions"
	"github.com/kawaiipantsu/boop/internal/provider"
	"github.com/kawaiipantsu/boop/internal/session"
)

// Options configures a TUI session.
type Options struct {
	// Config is the loaded configuration with command-line overrides already
	// applied. Required.
	Config *config.Config
	// WorkingDir is the project root; empty uses the process directory.
	WorkingDir string
	// DatabasePath overrides the session store location. ":memory:" keeps the
	// session in RAM, which is what tests want.
	DatabasePath string
	// SystemPrompt overrides the built-in prompt.
	SystemPrompt string
	// InitialPrompt is submitted as soon as the UI is up, which is how
	// `boop <prompt>` continues interactively (§21).
	InitialPrompt string
	// ResumeSession resumes a stored session instead of starting a new one.
	ResumeSession string
	// Stderr receives startup warnings. Nil discards them.
	Stderr io.Writer
	// Verbose prints startup warnings that are otherwise only recorded.
	Verbose bool
}

// shutdownGrace bounds how long Run waits for an interrupted turn to unwind
// before closing the store underneath it (§58).
const shutdownGrace = 5 * time.Second

// Run assembles the runtime and drives the terminal UI until the user leaves.
//
// The runtime is built here rather than by the caller because app.New needs an
// Approver at construction time and the TUI is the thing that can serve one.
func Run(ctx context.Context, opts Options) error {
	if opts.Config == nil {
		return errors.New("tui: a configuration is required")
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The broker is bound to the session context so shutdown releases anything
	// parked on an approval, and left without a timeout because an
	// interactive user is allowed to think.
	broker := permissions.NewBroker(permissions.WithContext(ctx))
	approver := NewApprover(broker)

	application, err := app.New(ctx, app.Options{
		Config:       opts.Config,
		WorkingDir:   opts.WorkingDir,
		Approver:     approver,
		DatabasePath: opts.DatabasePath,
		SystemPrompt: opts.SystemPrompt,
		Stderr:       opts.Stderr,
		Verbose:      opts.Verbose,
	})
	if err != nil {
		return err
	}
	defer application.Close()
	approver.SetEvaluator(application.Evaluator)

	sess, history, resumed, err := openSession(ctx, application, opts.ResumeSession)
	if err != nil {
		return err
	}

	model := newModel(ctx, application, approver, sess.ID, sess.Title, opts.InitialPrompt, history)
	model.greet(application, resumed, opts.Verbose)

	program := tea.NewProgram(model,
		tea.WithContext(ctx),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)

	// Wire the two inbound streams now that there is a program to send to.
	model.pump.setSend(program.Send)
	detachBus := model.pump.attach(application.Bus)
	defer detachBus()
	detachApprovals := watchApprovals(broker, program.Send)
	defer detachApprovals()

	_, runErr := program.Run()

	// Order matters on the way out: stop accepting work, release anything the
	// loop is parked on, then wait briefly for it to unwind before the store
	// closes underneath it.
	cancel()
	broker.Close()
	waitForTurns(model.turns, shutdownGrace)

	// A killed or interrupted program is how a cancelled context and an
	// external SIGINT arrive; neither is a failure of the run.
	if runErr != nil && !errors.Is(runErr, tea.ErrProgramKilled) &&
		!errors.Is(runErr, tea.ErrInterrupted) && !errors.Is(runErr, context.Canceled) {
		return runErr
	}
	return nil
}

// waitForTurns waits for in-flight turns, giving up after grace so a wedged
// tool cannot stop Boop exiting.
func waitForTurns(wg *sync.WaitGroup, grace time.Duration) {
	if wg == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(grace):
	}
}

// openSession starts or resumes the session the UI works in, returning the
// conversation to seed the model with.
func openSession(ctx context.Context, application *app.App, resumeID string) (*session.Session, []provider.Message, bool, error) {
	system := provider.Message{Role: provider.RoleSystem, Content: buildSystemPrompt(application)}

	if resumeID != "" {
		sess, err := application.Sessions.Resume(ctx, resumeID, application.Workspace.Root())
		if err != nil {
			return nil, nil, false, fmt.Errorf("cannot resume session %s: %w", resumeID, err)
		}
		stored, err := application.Sessions.History().Messages(ctx, sess.ID, session.TranscriptOptions{})
		if err != nil {
			return nil, nil, false, fmt.Errorf("cannot read the stored transcript: %w", err)
		}
		return sess, append([]provider.Message{system}, stored...), true, nil
	}

	sess, err := application.Sessions.Create(ctx, session.CreateOptions{
		ProjectPath: application.Workspace.Root(),
		Provider:    application.Config.Provider,
		Model:       application.Config.Model,
	})
	if err != nil {
		return nil, nil, false, fmt.Errorf("cannot start a session: %w", err)
	}
	return sess, []provider.Message{system}, false, nil
}

// buildSystemPrompt assembles the prompt with this machine's context (§29).
func buildSystemPrompt(application *app.App) string {
	var memory string
	if application != nil {
		if mem := application.GetMemory(); mem != nil {
			memory = string(mem.Render())
		}
	}
	return app.PromptContext{
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		Shell:       os.Getenv("SHELL"),
		WorkingDir:  application.Workspace.Root(),
		Provider:    application.Config.Provider,
		Model:       application.Config.Model,
		Mode:        string(application.Config.Execution.Mode),
		Tools:       application.Tools.Names(),
		NetworkOn:   application.Config.Network.Enabled,
		ProjectInfo: memory,
	}.Render(application.SystemPrompt())
}

// greet writes the opening lines of the transcript: what Boop is pointed at,
// and anything that went wrong during startup but did not stop it.
func (m *Model) greet(application *app.App, resumed, verbose bool) {
	m.transcript.Append(Entry{Kind: EntrySystem, Text: fmt.Sprintf(
		"boop — %s in %s\ntype /help for commands, Ctrl+C to interrupt",
		m.providerModel(), application.Workspace.Root())})

	if resumed {
		m.transcript.Append(Entry{Kind: EntrySystem, Text: fmt.Sprintf(
			"resumed session %s with %d stored message(s)", m.sessionID, maxInt(0, len(m.history)-1))})
		for _, e := range entriesFromMessages(m.history[1:]) {
			m.transcript.Append(e)
		}
	}
	// Startup warnings are usually just "you have no OPENAI_API_KEY", which a
	// local-only user does not need three paragraphs about every launch. Show
	// a single line and keep the detail behind /status or --verbose.
	if n := len(application.Warnings); n > 0 {
		if verbose {
			for _, w := range application.Warnings {
				m.transcript.Append(Entry{Kind: EntrySystem, Text: "startup: " + w})
			}
		} else {
			m.transcript.Append(Entry{Kind: EntrySystem, Text: fmt.Sprintf(
				"%d provider(s) unavailable — /status for details", n)})
		}
	}
	if application.Config.Network.Enabled {
		m.transcript.Append(Entry{Kind: EntrySystem,
			Text: "outbound web access is ON: fetches and searches leave this machine"})
	}
	if application.Evaluator.Policy().Unrestricted {
		m.transcript.Append(Entry{Kind: EntryError,
			Text: "running UNRESTRICTED: permission confirmation is disabled for this run"})
	}
}

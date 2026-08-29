// Package app owns process lifecycle, the event bus, and the wiring that turns
// the individual subsystems into a running Boop.
//
// This is the only package that knows about all of them at once. Keeping the
// assembly here is what lets the TUI, the plain CLI and the WebUI be thin
// frontends over one runtime (§2.3).
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/kawaiipantsu/boop/internal/config"
	"github.com/kawaiipantsu/boop/internal/execution"
	"github.com/kawaiipantsu/boop/internal/permissions"
	"github.com/kawaiipantsu/boop/internal/project"
	"github.com/kawaiipantsu/boop/internal/provider"
	"github.com/kawaiipantsu/boop/internal/session"
	"github.com/kawaiipantsu/boop/internal/store"
	"github.com/kawaiipantsu/boop/internal/tools"
	"github.com/kawaiipantsu/boop/internal/webclient"
)

// Options configures how an App is assembled.
type Options struct {
	// Config is the loaded configuration. Required.
	Config *config.Config
	// WorkingDir is the project root. Empty means the process directory.
	WorkingDir string
	// Approver serves confirmation prompts. Without one, any action needing
	// confirmation fails rather than proceeding unapproved.
	Approver permissions.Approver
	// DatabasePath overrides the session store location. Empty uses the
	// configured data directory; ":memory:" keeps the session in RAM.
	DatabasePath string
	// SystemPrompt overrides the built-in prompt.
	SystemPrompt string
	// Stderr receives startup warnings. Nil discards them.
	Stderr io.Writer
	// Verbose prints startup warnings that are otherwise only recorded.
	Verbose bool
}

// App is an assembled Boop runtime.
type App struct {
	Config    *config.Config
	Bus       *Bus
	Router    *provider.Router
	Tools     *tools.Registry
	Evaluator *permissions.Evaluator
	Approver  permissions.Approver
	Sessions  *session.Manager
	Workspace *tools.Workspace
	Web       *webclient.Client
	Memory    *project.Memory
	// Warnings records providers that could not be built, so a frontend can
	// explain why a provider is missing without failing startup over it.
	Warnings []string

	store        store.Store
	systemPrompt string
}

// New assembles a runtime from configuration.
//
// Assembly is ordered so that a failure names the thing the user has to fix.
// Configuration problems surface before any file or socket is touched.
func New(ctx context.Context, opts Options) (*App, error) {
	if opts.Config == nil {
		return nil, errors.New("app: a configuration is required")
	}
	cfg := opts.Config

	if _, err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("configuration is invalid: %w", err)
	}

	dir := opts.WorkingDir
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("app: cannot determine the working directory: %w", err)
		}
		dir = cwd
	}
	if root, err := project.FindRoot(dir); err == nil && root != "" {
		dir = root
	}
	ws, err := tools.NewWorkspace(dir)
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}

	httpClient := defaultHTTPClient()
	router, warnings, err := BuildRouter(cfg, httpClient)
	if err != nil {
		return nil, err
	}
	// Warnings are informational, not errors: a local-only user has no
	// OPENAI_API_KEY and does not need to be told so on every single run.
	// They are surfaced through the returned App for a frontend to show on
	// demand, and printed only when the caller asks to see them.
	if opts.Verbose && opts.Stderr != nil {
		for _, w := range warnings {
			fmt.Fprintln(opts.Stderr, "boop:", w)
		}
	}

	var web *webclient.Client
	if cfg.Network.Enabled {
		web, err = newWebClient(cfg)
		if err != nil {
			return nil, fmt.Errorf("app: outbound web access: %w", err)
		}
	}

	registry, err := BuildTools(cfg, ToolDeps{Workspace: ws, Executor: newExecutor(cfg), Web: web})
	if err != nil {
		return nil, err
	}

	dbPath := opts.DatabasePath
	if dbPath == "" {
		dbPath, err = config.DatabasePath()
		if err != nil {
			return nil, fmt.Errorf("app: %w", err)
		}
	}
	st, err := store.OpenContext(ctx, dbPath)
	if err != nil {
		return nil, fmt.Errorf("app: cannot open the session store at %s: %w", dbPath, err)
	}

	prompt := opts.SystemPrompt
	if prompt == "" {
		prompt = DefaultSystemPrompt()
	}

	app := &App{
		Config:       cfg,
		Warnings:     warnings,
		Bus:          NewBus(),
		Router:       router,
		Tools:        registry,
		Evaluator:    permissions.NewEvaluator(cfg.Policy()),
		Approver:     opts.Approver,
		Sessions:     session.NewManager(st),
		Workspace:    ws,
		Web:          web,
		store:        st,
		systemPrompt: prompt,
	}

	// Project memory is best-effort: a project without a Boop.md is normal,
	// and failing to read one must not stop Boop starting.
	if mem, err := project.LoadOrCreate(filepath.Join(ws.Root(), "Boop.md")); err == nil {
		app.Memory = mem
	}
	return app, nil
}

// newExecutor builds the command executor with the configured bounds.
func newExecutor(cfg *config.Config) execution.Executor {
	return execution.NewLocalExecutor(
		execution.WithDefaultTimeout(cfg.Execution.CommandTimeout.Std()),
	)
}

// NewLoop returns a loop bound to this runtime for the given session.
func (a *App) NewLoop(sessionID string) *Loop {
	return &Loop{
		Router:        a.Router,
		Tools:         a.Tools,
		Evaluator:     a.Evaluator,
		Approver:      a.Approver,
		Bus:           a.Bus,
		MaxIterations: a.Config.Execution.MaxToolIterations,
		SessionID:     sessionID,
		Selection: provider.Selection{
			Provider: a.Config.Provider,
			Model:    a.Config.Model,
		},
	}
}

// SystemPrompt returns the prompt prefixed to every conversation.
func (a *App) SystemPrompt() string { return a.systemPrompt }

// Close releases the runtime's resources.
//
// Shutdown flushes and closes the database last, so anything written during
// teardown is still persisted (§58).
func (a *App) Close() error {
	if a.store != nil {
		return a.store.Close()
	}
	return nil
}

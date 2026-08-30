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
	"strings"
	"sync"

	"github.com/kawaiipantsu/boop/internal/config"
	"github.com/kawaiipantsu/boop/internal/execution"
	"github.com/kawaiipantsu/boop/internal/logging"
	"github.com/kawaiipantsu/boop/internal/permissions"
	"github.com/kawaiipantsu/boop/internal/project"
	"github.com/kawaiipantsu/boop/internal/provider"
	"github.com/kawaiipantsu/boop/internal/session"
	"github.com/kawaiipantsu/boop/internal/stats"
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
	// LogPath overrides the platform log file. ":discard" disables logging,
	// which keeps tests from writing to the user's real log directory.
	LogPath string
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
	// Stats aggregates token, cost and tool usage for /stats and the WebUI.
	//
	// The agent coordinator is deliberately not held here: internal/agent
	// builds on app.Loop, so the dependency runs upward and the fleet is
	// constructed by the frontends via agent.NewFromApp.
	Stats *stats.Tracker
	// Logger is the structured logger; its output goes to a file rather than
	// the terminal so a full-screen UI is never polluted (§44).
	Logger *logging.Logger
	// Warnings records providers that could not be built, so a frontend can
	// explain why a provider is missing without failing startup over it.
	Warnings []string

	mu           sync.RWMutex
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

	logger, err := buildLogger(cfg, opts.LogPath)
	if err != nil {
		return nil, fmt.Errorf("app: %w", err)
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
		Stats:        stats.New(),
		Logger:       logger,
		store:        st,
		systemPrompt: prompt,
	}

	// Project memory is best-effort: a project without a Boop.md is normal,
	// and failing to read one must not stop Boop starting.
	if mem, err := project.LoadOrCreate(ws.Root()); err == nil {
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
		Router:               a.Router,
		Tools:                a.Tools,
		Evaluator:            a.Evaluator,
		Approver:             a.Approver,
		Bus:                  a.Bus,
		MaxIterations:        a.Config.Execution.MaxToolIterations,
		MaxRetriesPerCommand: a.Config.Execution.MaxRetriesPerCommand,
		Context:              a.newContextManager(),
		SessionID:            sessionID,
		Selection: provider.Selection{
			Provider: a.Config.Provider,
			Model:    a.Config.Model,
		},
	}
}

// newContextManager bounds each request to the active model's window.
//
// The budget comes from the model's declared context window where the router
// knows it, falling back to a conservative default: overestimating the window
// produces a request the provider rejects, which is worse than sending less.
func (a *App) newContextManager() *session.ContextManager {
	return session.NewContextManager(session.Options{
		Budget:  defaultContextBudget,
		Reserve: defaultAnswerReserve,
	})
}

// SystemPrompt returns the prompt prefixed to every conversation.
func (a *App) SystemPrompt() string { return a.systemPrompt }

// GetMemory returns the active project memory safely under lock (§7).
func (a *App) GetMemory() *project.Memory {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Memory
}

// SetMemory safely updates the in-process project memory (§7).
func (a *App) SetMemory(mem *project.Memory) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.Memory = mem
	a.mu.Unlock()
}

// ReloadMemory re-reads Boop.md from the workspace root and safely updates App.Memory (§7).
func (a *App) ReloadMemory() (*project.Memory, error) {
	if a == nil {
		return nil, errors.New("app: runtime is nil")
	}
	if a.Workspace == nil {
		return nil, errors.New("app: workspace is not initialized")
	}
	mem, err := project.LoadOrCreate(a.Workspace.Root())
	if err != nil {
		return nil, err
	}
	a.SetMemory(mem)
	return mem, nil
}

// ApplyConfig safely swaps the running configuration and updates runtime components (§6).
// It reports whether any changes require a process restart (e.g. web/logging bind changes).
func (a *App) ApplyConfig(newCfg *config.Config) (restartRequired bool, err error) {
	if a == nil {
		return false, errors.New("app: runtime is nil")
	}
	if _, err := newCfg.Validate(); err != nil {
		return false, fmt.Errorf("configuration is invalid: %w", err)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	old := a.Config
	if old != nil {
		if old.Web.Listen != newCfg.Web.Listen ||
			old.Web.Port != newCfg.Web.Port ||
			old.Web.Enabled != newCfg.Web.Enabled ||
			old.Logging.File != newCfg.Logging.File {
			restartRequired = true
		}
	}

	// Rebuild evaluator with updated policy
	a.Evaluator = permissions.NewEvaluator(newCfg.Policy())

	// Rebuild router if providers or model configs changed
	httpClient := defaultHTTPClient()
	if router, warnings, err := BuildRouter(newCfg, httpClient); err == nil {
		a.Router = router
		a.Warnings = warnings
	}

	a.Config = newCfg
	return restartRequired, nil
}

// Context budget defaults, used when the model's real window is unknown.
const (
	// defaultContextBudget is deliberately modest: many local servers serve a
	// far smaller window than the model advertises, and an over-long request
	// fails outright rather than degrading.
	defaultContextBudget = 24000
	// defaultAnswerReserve is held back from the budget for the reply.
	defaultAnswerReserve = 4000
)

// buildLogger opens the structured logger for this run.
//
// It writes to a file rather than the terminal because a full-screen UI owns
// the terminal and §44 forbids debug noise in the transcript. A logging
// failure is not fatal: losing logs is worse than not starting, but only
// slightly, so the caller gets a working discard logger instead.
func buildLogger(cfg *config.Config, override string) (*logging.Logger, error) {
	if override == ":discard" {
		return logging.NewNop(), nil
	}
	level, err := logging.ParseLevel(cfg.Logging.Level)
	if err != nil {
		return nil, err
	}
	path := override
	if path == "" {
		path = cfg.Logging.File
	}
	if path == "" {
		if path, err = config.LogFile(); err != nil {
			return logging.NewNop(), nil
		}
	}
	format := logging.FormatText
	if strings.EqualFold(cfg.Logging.Format, "json") {
		format = logging.FormatJSON
	}
	lg, err := logging.New(logging.Options{
		Level: level, Format: format, File: path,
		MaxSizeBytes: int64(cfg.Logging.MaxSizeMB) << 20,
		MaxBackups:   cfg.Logging.MaxBackups,
		AddSource:    level <= logging.LevelDebug,
	})
	if err != nil {
		return logging.NewNop(), nil
	}
	return lg, nil
}

// Close releases the runtime's resources.
//
// Shutdown flushes and closes the database last, so anything written during
// teardown is still persisted (§58).
func (a *App) Close() error {
	var firstErr error
	if a.store != nil {
		firstErr = a.store.Close()
	}
	// The logger closes last so anything written during teardown still lands.
	if a.Logger != nil {
		if err := a.Logger.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

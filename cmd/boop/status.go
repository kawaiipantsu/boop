package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/kawaiipantsu/boop/internal/app"
	"github.com/kawaiipantsu/boop/internal/provider"
	"github.com/kawaiipantsu/boop/internal/version"
)

// statusProbeTimeout bounds the provider health and capability probes so
// `boop status` returns promptly even when a backend is a black hole rather
// than refusing the connection outright.
const statusProbeTimeout = 5 * time.Second

// errProviderUnhealthy is returned by runStatus after the report has been
// printed, so the process exits non-zero and the command composes in a health
// check (§54).
var errProviderUnhealthy = errors.New("the active provider is unhealthy")

// statusInfo is the command-line equivalent of the GET /api/status document
// (§54): everything needed to answer "can Boop reach its provider" without
// starting a session or a prompt.
type statusInfo struct {
	Version  version.Info    `json:"version"`
	Project  string          `json:"project_path"`
	Provider providerStatus  `json:"provider"`
	Model    modelStatus     `json:"model"`
	Mode     string          `json:"mode"`
	Agents   agentStatusInfo `json:"agents"`
	Network  bool            `json:"network_enabled"`
	Session  string          `json:"session"`
	Warnings []string        `json:"warnings,omitempty"`
}

// providerStatus is the active backend's reachability.
type providerStatus struct {
	Name      string `json:"name"`
	BaseURL   string `json:"base_url,omitempty"`
	Healthy   bool   `json:"healthy"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

// modelStatus is what the active model can do, as discovered from the provider
// (§8) rather than guessed from its name.
type modelStatus struct {
	Name          string `json:"name"`
	Explicit      bool   `json:"explicit"`
	CapsKnown     bool   `json:"capabilities_known"`
	Tools         bool   `json:"tools"`
	Vision        bool   `json:"vision"`
	Reasoning     bool   `json:"reasoning"`
	ContextWindow int    `json:"context_window,omitempty"`
}

// agentStatusInfo mirrors the scheduler bounds from config.
type agentStatusInfo struct {
	Enabled bool `json:"enabled"`
	Max     int  `json:"max"`
	Running int  `json:"running"`
}

// runStatusCommand implements `boop status [flags]`.
//
// It is matched before flag parsing, like `version` and `prep`, so its own
// flags are not mistaken for a bare prompt. It accepts the same provider,
// model and mode overrides as a normal run so a user can check a backend
// other than the configured default.
func runStatusCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	var opts options
	fs := flag.NewFlagSet("boop status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.BoolVar(&opts.statusJSON, "json", false, "print the report as JSON")
	fs.StringVar(&opts.provider, "provider", "", "provider to check")
	fs.StringVar(&opts.model, "model", "", "model to check")
	fs.StringVar(&opts.mode, "mode", "", "execution mode: confirm or auto")
	fs.Usage = func() {
		fmt.Fprint(stderr, "usage: boop status [--json] [--provider name] [--model id]\n\n")
		fmt.Fprintln(stderr, "Reports build metadata, the active provider's health and the model's")
		fmt.Fprintln(stderr, "capabilities. Exits non-zero when the active provider is unreachable.")
		fmt.Fprintln(stderr)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if rest := fs.Args(); len(rest) > 0 {
		return fmt.Errorf("boop status takes no positional arguments, got %q", rest[0])
	}
	return runStatus(ctx, opts, stdout, stderr)
}

// runStatus gathers and prints the report, then reports an unhealthy active
// provider as an error so the exit code is non-zero.
func runStatus(ctx context.Context, opts options, stdout, stderr io.Writer) error {
	cfg, err := loadConfig(opts)
	if err != nil {
		return err
	}

	// The session store and the log file are side effects a status check has
	// no business creating, so the runtime is built against an in-memory store
	// with logging discarded. Everything the report needs — the router, the
	// tool registry, the workspace and the provider warnings — is assembled
	// regardless.
	application, err := app.New(ctx, app.Options{
		Config:       cfg,
		DatabasePath: ":memory:",
		LogPath:      ":discard",
	})
	if err != nil {
		return err
	}
	defer func() { _ = application.Close() }()

	info := gatherStatus(ctx, application, cfg.Provider, cfg.Model)

	if err := renderStatus(stdout, info, opts.statusJSON); err != nil {
		return err
	}
	if !info.Provider.Healthy {
		return fmt.Errorf("%w: %s", errProviderUnhealthy, info.Provider.Error)
	}
	return nil
}

// gatherStatus probes the active provider and reads the rest from the runtime.
func gatherStatus(ctx context.Context, a *app.App, providerName, model string) statusInfo {
	info := statusInfo{
		Version: version.Get(),
		Project: a.Workspace.Root(),
		Mode:    string(a.Config.Execution.Mode),
		Agents: agentStatusInfo{
			Enabled: a.Config.Agents.Enabled,
			Max:     a.Config.Agents.Max,
		},
		Network:  a.Config.Network.Enabled,
		Session:  "none",
		Warnings: a.Warnings,
	}

	info.Provider.Name = providerName
	if pc, ok := a.Config.Providers[providerName]; ok {
		info.Provider.BaseURL = pc.BaseURL
	}

	info.Model.Name = model
	info.Model.Explicit = model != ""

	probeCtx, cancel := context.WithTimeout(ctx, statusProbeTimeout)
	defer cancel()

	start := time.Now()
	healthErr := a.Router.Health(probeCtx, providerName)
	info.Provider.LatencyMS = time.Since(start).Milliseconds()
	if healthErr != nil {
		info.Provider.Healthy = false
		info.Provider.Error = healthErr.Error()
		return info
	}
	info.Provider.Healthy = true

	// Capabilities are only meaningful once the provider answered, and are
	// discovered from it rather than inferred from the model name (§8).
	p, ok := a.Router.Registry().Get(providerName)
	if !ok {
		return info
	}
	if caps, err := p.Capabilities(probeCtx, model); err == nil {
		info.Model.CapsKnown = true
		info.Model.Tools = caps.Has(provider.CapabilityTools)
		info.Model.Vision = caps.Has(provider.CapabilityVision)
		info.Model.Reasoning = caps.Has(provider.CapabilityReasoning)
	}
	if model != "" {
		if models, err := p.ListModels(probeCtx); err == nil {
			for _, m := range models {
				if m.ID == model {
					info.Model.ContextWindow = m.ContextWindow
					break
				}
			}
		}
	}
	return info
}

// renderStatus writes the report as aligned text or as JSON.
func renderStatus(w io.Writer, s statusInfo, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(s)
	}

	fmt.Fprintf(w, "boop v%s", s.Version.Version)
	if s.Version.Dirty {
		fmt.Fprint(w, " (dirty)")
	}
	fmt.Fprintf(w, "  ·  %s\n\n", s.Project)

	base := s.Provider.BaseURL
	if base == "" {
		base = "(adapter default)"
	}
	health := fmt.Sprintf("healthy (%dms)", s.Provider.LatencyMS)
	if !s.Provider.Healthy {
		health = "unhealthy: " + s.Provider.Error
	}
	fmt.Fprintf(w, "%-9s %-11s %-26s %s\n", "provider", s.Provider.Name, base, health)

	model := s.Model.Name
	if model == "" {
		model = "(provider default)"
	}
	switch {
	case !s.Provider.Healthy:
		fmt.Fprintf(w, "%-9s %-11s capabilities unknown (provider unreachable)\n", "model", model)
	case !s.Model.CapsKnown:
		fmt.Fprintf(w, "%-9s %-11s capabilities unknown\n", "model", model)
	default:
		caps := fmt.Sprintf("tools %s  vision %s  reasoning %s",
			check(s.Model.Tools), check(s.Model.Vision), check(s.Model.Reasoning))
		if s.Model.ContextWindow > 0 {
			caps += fmt.Sprintf("  %d ctx", s.Model.ContextWindow)
		}
		fmt.Fprintf(w, "%-9s %-11s %s\n", "model", model, caps)
	}

	agents := "off"
	if s.Agents.Enabled {
		agents = fmt.Sprintf("%d/%d", s.Agents.Running, s.Agents.Max)
	} else if s.Agents.Max > 0 {
		agents = fmt.Sprintf("off (max %d)", s.Agents.Max)
	}
	fmt.Fprintf(w, "%-9s %-11s agents %s   network %s\n", "mode", s.Mode, agents, onOff(s.Network))
	fmt.Fprintf(w, "%-9s %s\n", "session", s.Session)

	if len(s.Warnings) > 0 {
		fmt.Fprintf(w, "\n%-9s %d\n", "warnings", len(s.Warnings))
		for _, warn := range s.Warnings {
			fmt.Fprintf(w, "  %s\n", warn)
		}
	}
	return nil
}

func check(ok bool) string {
	if ok {
		return "✓"
	}
	return "✗"
}

func onOff(ok bool) string {
	if ok {
		return "on"
	}
	return "off"
}

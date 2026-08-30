package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/kawaiipantsu/boop/internal/app"
	"github.com/kawaiipantsu/boop/internal/project"
	"github.com/kawaiipantsu/boop/internal/provider"
	"github.com/kawaiipantsu/boop/internal/version"
)

// runStatus implements `boop status` / `boop --status` (§54).
func runStatus(ctx context.Context, opts options, stdout, stderr io.Writer) error {
	cfg, err := loadConfig(opts)
	if err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	root := cwd
	if found, err := project.FindRoot(cwd); err == nil && found != "" {
		root = found
	}

	v := version.Get()
	fmt.Fprintf(stdout, "boop %s  ·  %s\n", v.Version, root)

	// Build app runtime with discarded logs and memory db to avoid side effects
	application, err := app.New(ctx, app.Options{
		Config:       cfg,
		DatabasePath: ":memory:",
		LogPath:      ":discard",
		WorkingDir:   root,
		Stderr:       nil,
	})
	if err != nil {
		return fmt.Errorf("boop status: %w", err)
	}
	defer application.Close()

	activeProviderName := cfg.Provider
	var isHealthy bool
	var activeHealthMsg string

	var p provider.Provider
	if application.Router != nil && application.Router.Registry() != nil {
		p, _ = application.Router.Registry().Get(activeProviderName)
	}
	if p != nil {
		probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		start := time.Now()
		if hErr := p.Health(probeCtx); hErr == nil {
			isHealthy = true
			activeHealthMsg = fmt.Sprintf("healthy (%dms)", time.Since(start).Milliseconds())
		} else {
			activeHealthMsg = fmt.Sprintf("unreachable (%v)", hErr)
		}
		baseURL := ""
		if pcfg, ok := cfg.Providers[activeProviderName]; ok && pcfg.BaseURL != "" {
			baseURL = pcfg.BaseURL
		}
		if baseURL != "" {
			fmt.Fprintf(stdout, "%-9s %-11s %-24s %s\n", "provider", activeProviderName, baseURL, activeHealthMsg)
		} else {
			fmt.Fprintf(stdout, "%-9s %-11s %s\n", "provider", activeProviderName, activeHealthMsg)
		}
	} else {
		fmt.Fprintf(stdout, "%-9s %-11s %s\n", "provider", activeProviderName, "not configured or unavailable")
	}

	// Model information
	model := cfg.Model
	if model == "" && p != nil {
		models, _ := p.ListModels(ctx)
		if len(models) > 0 {
			model = models[0].ID
		}
	}
	if model == "" {
		model = "<default>"
	}

	toolsMark := "tools ✓"
	visionMark := "vision ✗"
	ctxInfo := ""
	if p != nil {
		caps, _ := p.Capabilities(ctx, model)
		if caps.Has("vision") {
			visionMark = "vision ✓"
		}
		if models, err := p.ListModels(ctx); err == nil {
			for _, m := range models {
				if m.ID == model && m.ContextWindow > 0 {
					ctxInfo = fmt.Sprintf("  %d ctx", m.ContextWindow)
					break
				}
			}
		}
	}
	fmt.Fprintf(stdout, "%-9s %-12s %s  %s%s\n", "model", model, toolsMark, visionMark, ctxInfo)

	// Mode, agents, network
	netStatus := "network off"
	if cfg.Network.Enabled {
		netStatus = "network on"
	}
	agentStatus := fmt.Sprintf("agents 0/%d", cfg.Agents.Max)
	if !cfg.Agents.Enabled {
		agentStatus = "agents off"
	}
	fmt.Fprintf(stdout, "%-9s %-12s %-15s %s\n", "mode", string(cfg.Execution.Mode), agentStatus, netStatus)

	// Session
	sessionInfo := "none"
	fmt.Fprintf(stdout, "%-9s %s\n", "session", sessionInfo)

	// Warnings
	if len(application.Warnings) > 0 {
		fmt.Fprintf(stdout, "%-9s %d provider(s) unavailable (%s)\n", "warnings", len(application.Warnings), strings.Join(application.Warnings, "; "))
	}

	if !isHealthy {
		return fmt.Errorf("active provider %q is not healthy", activeProviderName)
	}

	return nil
}

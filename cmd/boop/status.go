package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/kawaiipantsu/boop/internal/app"
	"github.com/kawaiipantsu/boop/internal/config"
	"github.com/kawaiipantsu/boop/internal/version"
)

// runStatus implements `boop status` / `boop --status` (§54).
func runStatus(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	dir := ""
	if len(args) > 0 {
		dir = args[0]
	}
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("cannot determine working directory: %w", err)
		}
		dir = cwd
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	v := version.Get()
	fmt.Fprintf(stdout, "boop %s  ·  %s\n", v.Version, dir)

	// Build app in lightweight headless mode
	application, err := app.New(ctx, app.Options{
		Config:       cfg,
		WorkingDir:   dir,
		DatabasePath: ":memory:",
		LogPath:      ":discard",
		Stderr:       io.Discard,
	})
	if err != nil {
		fmt.Fprintf(stdout, "runtime error: %v\n", err)
		return err
	}
	defer application.Close()

	activeProvider := cfg.Provider
	var pConfig *config.ProviderConfig
	if cfg.Providers != nil {
		if pc, ok := cfg.Providers[activeProvider]; ok {
			pConfig = &pc
		}
	}

	providerURL := "-"
	if pConfig != nil && pConfig.BaseURL != "" {
		providerURL = pConfig.BaseURL
	}

	healthStr := "configured"
	if application.Router != nil && application.Router.Registry() != nil {
		if p, ok := application.Router.Registry().Get(activeProvider); ok && p != nil {
			start := time.Now()
			if err := p.Health(ctx); err == nil {
				healthStr = fmt.Sprintf("healthy (%s)", time.Since(start).Round(time.Millisecond))
			} else {
				healthStr = fmt.Sprintf("unhealthy (%v)", err)
			}
		}
	}

	fmt.Fprintf(stdout, "%-10s%-12s %-24s %s\n", "provider", activeProvider, providerURL, healthStr)
	modelName := cfg.Model
	if modelName == "" {
		modelName = "(provider default)"
	}
	fmt.Fprintf(stdout, "%-10s%s\n", "model", modelName)
	fmt.Fprintf(stdout, "%-10s%-12s agents %s      network %s\n",
		"mode",
		cfg.Execution.Mode,
		agentsStatusText(cfg.Agents.Enabled, cfg.Agents.Max),
		networkStatusText(cfg.Network.Enabled),
	)
	fmt.Fprintf(stdout, "%-10s%s\n", "session", "none (standalone CLI)\n")

	return nil
}

func agentsStatusText(enabled bool, max int) string {
	if !enabled {
		return "disabled"
	}
	return fmt.Sprintf("0/%d", max)
}

func networkStatusText(enabled bool) string {
	if enabled {
		return "on"
	}
	return "off"
}

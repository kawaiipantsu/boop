package tui

import (
	"fmt"
	"os"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kawaiipantsu/boop/internal/config"
)

// configCmd shows the configuration this process is actually running with
// (§55), including the overrides applied on the command line.
//
// It is read-only. Editing happens in the config file or the WebUI settings
// page; a half-applied change made mid-session would be worse than one that
// takes effect on restart, which is the same reason PUT /api/config does not
// mutate the running process either.
func (m *Model) configCmd(cmd Command) tea.Cmd {
	if m.app == nil || m.app.Config == nil {
		return m.say(EntryError, "no runtime is attached")
	}
	if len(cmd.Args) > 0 {
		return m.say(EntryError, "usage: /config — it reports the effective configuration; edit the file or use the WebUI to change it")
	}
	return m.say(EntrySystem, configText(m.app.Config, os.LookupEnv))
}

// configText renders the effective configuration.
//
// No credential is ever printed. Configuration names an environment variable
// through api_key_env and token_env; the name and whether it is set are
// reportable, the value never is, and provider headers are shown by key only
// because a header is where a hand-written token would hide (§45).
func configText(cfg *config.Config, lookupEnv func(string) (string, bool)) string {
	var b strings.Builder
	b.WriteString("effective configuration\n")
	if path, err := config.ConfigFile(); err == nil {
		fmt.Fprintf(&b, "  file             %s\n", path)
	}
	fmt.Fprintf(&b, "  version          %d\n", cfg.Version)
	fmt.Fprintf(&b, "  provider/model   %s / %s\n", cfg.Provider, orDefault(cfg.Model, "(provider default)"))

	b.WriteString("\nexecution\n")
	fmt.Fprintf(&b, "  mode             %s\n", cfg.Execution.Mode)
	fmt.Fprintf(&b, "  command timeout  %s\n", cfg.Execution.CommandTimeout.Std())
	fmt.Fprintf(&b, "  max iterations   %d\n", cfg.Execution.MaxToolIterations)
	fmt.Fprintf(&b, "  max retries      %d per command\n", cfg.Execution.MaxRetriesPerCommand)
	if cfg.Execution.Unrestricted {
		b.WriteString("  UNRESTRICTED     confirmation is disabled for this run\n")
	}

	b.WriteString("\nagents\n")
	fmt.Fprintf(&b, "  enabled          %t\n", cfg.Agents.Enabled)
	fmt.Fprintf(&b, "  max concurrent   %d\n", cfg.Agents.Max)

	b.WriteString("\nnetwork (outbound access to third-party sites)\n")
	fmt.Fprintf(&b, "  enabled          %t\n", cfg.Network.Enabled)
	fmt.Fprintf(&b, "  search provider  %s (max %d results)\n", cfg.Network.Search.Provider, cfg.Network.Search.MaxResults)
	fmt.Fprintf(&b, "  private networks %s\n", allowedOrNot(cfg.Network.AllowPrivateNetworks))
	if len(cfg.Network.AllowedDomains) > 0 {
		fmt.Fprintf(&b, "  allowed domains  %s\n", strings.Join(cfg.Network.AllowedDomains, ", "))
	}
	if len(cfg.Network.BlockedDomains) > 0 {
		fmt.Fprintf(&b, "  blocked domains  %s\n", strings.Join(cfg.Network.BlockedDomains, ", "))
	}

	b.WriteString("\nwebui (Boop's own local server)\n")
	fmt.Fprintf(&b, "  enabled          %t\n", cfg.Web.Enabled)
	fmt.Fprintf(&b, "  listen           %s:%d\n", cfg.Web.Listen, cfg.Web.Port)
	fmt.Fprintf(&b, "  auth             %s\n", onOff(cfg.Web.Auth.Enabled))

	b.WriteString("\nlogging\n")
	fmt.Fprintf(&b, "  level/format     %s / %s\n", cfg.Logging.Level, cfg.Logging.Format)
	if cfg.Logging.File != "" {
		fmt.Fprintf(&b, "  file             %s\n", cfg.Logging.File)
	}

	b.WriteString("\nproviders\n")
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		pc := cfg.Providers[name]
		marker := " "
		if name == cfg.Provider {
			marker = "*"
		}
		fmt.Fprintf(&b, " %s %-12s %-18s %s\n", marker, name, pc.Type, pc.BaseURL)
		if pc.Disabled {
			b.WriteString("      disabled\n")
		}
		fmt.Fprintf(&b, "      credential   %s\n", credentialNote(pc.APIKeyEnv, lookupEnv))
		if len(pc.Headers) > 0 {
			keys := make([]string, 0, len(pc.Headers))
			for k := range pc.Headers {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			fmt.Fprintf(&b, "      headers      %s (values hidden)\n", strings.Join(keys, ", "))
		}
	}

	if len(cfg.Routing) > 0 {
		b.WriteString("\nrouting\n")
		keys := make([]string, 0, len(cfg.Routing))
		for k := range cfg.Routing {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			t := cfg.Routing[k]
			fmt.Fprintf(&b, "  %-16s %s/%s\n", k, t.Provider, orDefault(t.Model, "default"))
		}
	}
	if len(cfg.Fallback) > 0 {
		fmt.Fprintf(&b, "\nfallback order   %s\n", strings.Join(cfg.Fallback, " → "))
	}

	b.WriteString("\nsecrets are read from the environment; only the variable names are shown (§45)\n")
	fmt.Fprintf(&b, "  webui token      %s\n", credentialNote(cfg.Web.Auth.TokenEnv, lookupEnv))

	if warnings, err := cfg.Validate(); err != nil {
		fmt.Fprintf(&b, "\ninvalid: %s\n", err)
	} else {
		for _, w := range warnings {
			fmt.Fprintf(&b, "warning: %s\n", w)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// credentialNote reports an environment variable by name and whether it holds
// a value. It never reads the value into the output.
func credentialNote(env string, lookupEnv func(string) (string, bool)) string {
	env = strings.TrimSpace(env)
	if env == "" {
		return "none configured"
	}
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	v, ok := lookupEnv(env)
	if ok && strings.TrimSpace(v) != "" {
		return "$" + env + " (set)"
	}
	return "$" + env + " (not set)"
}

func allowedOrNot(v bool) string {
	if v {
		return "allowed"
	}
	return "blocked"
}

package tui

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kawaiipantsu/boop/internal/config"
	"github.com/kawaiipantsu/boop/internal/permissions"
)

// configCmd shows the effective configuration or adjusts runtime settings directly (§55).
func (m *Model) configCmd(cmd Command) tea.Cmd {
	if m.app == nil || m.app.Config == nil {
		return m.say(EntryError, "no runtime is attached")
	}
	cfg := m.app.Config

	if len(cmd.Args) == 0 {
		return m.say(EntrySystem, configText(cfg, os.LookupEnv))
	}

	switch strings.ToLower(cmd.Arg(0)) {
	case "mode":
		switch strings.ToLower(cmd.Arg(1)) {
		case "auto":
			cfg.Execution.Mode = permissions.ModeAuto
			return m.say(EntrySystem, "execution mode set to auto (approved categories run without prompting)")
		case "confirm":
			cfg.Execution.Mode = permissions.ModeConfirm
			return m.say(EntrySystem, "execution mode set to confirm (all actions require user approval)")
		default:
			return m.say(EntryError, "usage: /config mode [auto|confirm]")
		}
	case "agents":
		switch strings.ToLower(cmd.Arg(1)) {
		case "on":
			return m.setAgentsEnabled(true)
		case "off":
			return m.setAgentsEnabled(false)
		case "max":
			return m.setAgentsMax(cmd.Arg(2))
		default:
			return m.say(EntryError, "usage: /config agents [on|off|max <n>]")
		}
	case "web":
		switch strings.ToLower(cmd.Arg(1)) {
		case "on":
			cfg.Web.Enabled = true
			return m.say(EntrySystem, fmt.Sprintf("WebUI enabled (runs on %s:%d)", cfg.Web.Listen, cfg.Web.Port))
		case "off":
			cfg.Web.Enabled = false
			return m.say(EntrySystem, "WebUI disabled")
		case "port":
			p, err := strconv.Atoi(cmd.Arg(2))
			if err != nil || p < 1 || p > 65535 {
				return m.say(EntryError, "usage: /config web port <1-65535>")
			}
			cfg.Web.Port = p
			return m.say(EntrySystem, fmt.Sprintf("WebUI port set to %d (takes effect on restart)", p))
		default:
			return m.say(EntryError, "usage: /config web [on|off|port <n>]")
		}
	case "log", "logging":
		lvl := strings.ToLower(cmd.Arg(1))
		switch lvl {
		case "trace", "debug", "info", "warn", "error":
			cfg.Logging.Level = lvl
			return m.say(EntrySystem, fmt.Sprintf("log level set to %s", lvl))
		default:
			return m.say(EntryError, "usage: /config log [trace|debug|info|warn|error]")
		}
	case "save":
		if err := cfg.Save(); err != nil {
			return m.say(EntryError, "failed to save config: "+err.Error())
		}
		return m.say(EntrySystem, "configuration saved to disk")
	default:
		return m.say(EntryError, "usage: /config [mode auto|confirm | agents on|off|max <n> | web on|off|port <n> | log <level> | save]")
	}
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

package tui

import (
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kawaiipantsu/boop/internal/config"
	"github.com/kawaiipantsu/boop/internal/logging"
	"github.com/kawaiipantsu/boop/internal/permissions"
)

// configCmd shows the configuration this process is actually running with
// (§55), including the overrides applied on the command line, and — with a
// recognised sub-command — writes a single setting to config.yaml and swaps it
// into the running process.
//
// Every set command changes one field, persists it, and — via App.ApplyConfig
// (§6) — applies whatever a running process can honour immediately, reporting
// precisely which groups need a restart. Bulk editing still belongs in
// config.yaml: one field at a time keeps a half-applied change from being worse
// than one that lands cleanly.
func (m *Model) configCmd(cmd Command) tea.Cmd {
	if m.app == nil || m.app.Config() == nil {
		return m.say(EntryError, "no runtime is attached")
	}
	if len(cmd.Args) == 0 {
		return m.say(EntrySystem, configText(m.app.Config(), os.LookupEnv))
	}
	switch cmd.Arg(0) {
	case "mode":
		return m.configSetMode(cmd.Arg(1))
	case "provider":
		return m.configSetProvider(cmd.Arg(1))
	case "model":
		return m.configSetModel(strings.TrimSpace(strings.Join(cmd.Args[1:], " ")))
	case "agents":
		return m.configSetAgents(cmd.Arg(1), cmd.Arg(2))
	case "network":
		return m.configSetNetwork(cmd.Arg(1))
	case "web":
		return m.configSetWeb(cmd.Arg(1), cmd.Arg(2))
	case "base-url":
		return m.configSetBaseURL(cmd.Args[1:])
	case "timeout":
		return m.configSetTimeout(cmd.Arg(1))
	case "max-iterations":
		return m.configSetLoopLimit("max-iterations", cmd.Arg(1))
	case "max-retries":
		return m.configSetLoopLimit("max-retries", cmd.Arg(1))
	case "log", "logging":
		return m.configSetLog(cmd.Arg(1), cmd.Arg(2))
	default:
		return m.say(EntryError, configSetUsage())
	}
}

// configSetUsage lists every direct-command form (§55).
func configSetUsage() string {
	return strings.Join([]string{
		"usage: /config with no arguments reports the effective configuration.",
		"direct settings, each written to config.yaml and applied live where possible:",
		"  /config mode auto|confirm       execution mode",
		"  /config provider <name>         active provider",
		"  /config model <id>|default      active model",
		"  /config base-url [provider] <url>   a provider's endpoint",
		"  /config agents on|off           agent delegation",
		"  /config agents max <n>          concurrent-agent ceiling",
		"  /config network on|off          outbound web access",
		"  /config max-iterations <n>      tool-loop iteration cap",
		"  /config max-retries <n>         identical-retry cap per command",
		"  /config timeout <duration>      per-command timeout, e.g. 90s, 5m",
		"  /config log level <trace|debug|info|warn|error>",
		"  /config log format text|json",
		"  /config web on|off              serve the WebUI when `boop --web` runs",
		"  /config web port <1-65535>      WebUI port",
		"  /config web listen <ip>         WebUI bind address",
	}, "\n")
}

// configApply persists one change to config.yaml and swaps it into the running
// runtime. It returns the file it wrote, any advisory validation warnings, and
// the groups that changed but only take effect on restart (empty means the
// change is fully live).
//
// Disk and the live process get the same mutation from different bases: the
// file is reloaded first so a --flag override is never frozen in and a
// concurrent edit is not clobbered, while the live swap starts from the
// running snapshot.
func (m *Model) configApply(mutate func(*config.Config)) (path string, warnings, restart []string, err error) {
	path, warnings, err = persistConfigField(mutate)
	if err != nil {
		return "", nil, nil, err
	}
	next := m.app.Config().Clone()
	mutate(next)
	restart = m.app.ApplyConfig(next)
	return path, warnings, restart, nil
}

// configApplied renders the confirmation shared by every set command: what
// moved, whether it is live or needs a restart, where it was written, and any
// validation warnings.
func (m *Model) configApplied(what, path string, warnings, restart []string) tea.Cmd {
	var b strings.Builder
	b.WriteString(what)
	if len(restart) == 0 {
		b.WriteString(" — in effect now")
	} else {
		fmt.Fprintf(&b, " — saved; restart to apply (%s)", strings.Join(restart, ", "))
	}
	fmt.Fprintf(&b, "\nsaved to %s", path)
	for _, w := range warnings {
		fmt.Fprintf(&b, "\nwarning: %s", w)
	}
	return m.say(EntrySystem, b.String())
}

// persistConfigField reads config.yaml, applies one change, validates it and
// writes it back, returning the file path and any advisory warnings.
//
// It reloads from disk rather than saving m.app.Config() so a per-invocation flag
// override (--mode, --provider, --dangerously-unrestricted) is never frozen
// into the file, and so a change made in another editor is not clobbered. The
// caller mirrors the field onto m.app.Config() for whatever can take effect
// without a restart.
func persistConfigField(apply func(*config.Config)) (path string, warnings []string, err error) {
	path, err = config.ConfigFile()
	if err != nil {
		return "", nil, err
	}
	disk, err := config.Load()
	if err != nil {
		return "", nil, err
	}
	apply(disk)
	if warnings, err = disk.Validate(); err != nil {
		return "", nil, err
	}
	if err = disk.Save(); err != nil {
		return "", nil, err
	}
	return path, warnings, nil
}

// configSetMode changes execution.mode. ApplyConfig re-derives the permission
// policy, so it moves under the running process the same way /permissions mode
// does.
func (m *Model) configSetMode(v string) tea.Cmd {
	mode := permissions.Mode(strings.TrimSpace(v))
	if !mode.Valid() {
		return m.say(EntryError, "usage: /config mode auto|confirm")
	}
	path, warnings, restart, err := m.configApply(func(c *config.Config) { c.Execution.Mode = mode })
	if err != nil {
		return m.say(EntryError, "could not save the configuration: "+err.Error())
	}
	return m.configApplied("execution mode is now "+string(mode), path, warnings, restart)
}

// configSetProvider switches the active provider. It must already be configured;
// adding a provider is a config.yaml edit. The router reads the selection per
// turn, so the switch is live.
func (m *Model) configSetProvider(name string) tea.Cmd {
	name = strings.TrimSpace(name)
	if name == "" {
		return m.say(EntryError, "usage: /config provider <name>")
	}
	if _, ok := m.app.Router.Registry().Get(name); !ok {
		return m.say(EntryError, fmt.Sprintf("no provider named %q is configured; available: %s",
			name, strings.Join(m.app.Router.Registry().Names(), ", ")))
	}
	path, warnings, restart, err := m.configApply(func(c *config.Config) { c.Provider = name })
	if err != nil {
		return m.say(EntryError, "could not save the configuration: "+err.Error())
	}
	return tea.Batch(
		m.configApplied("active provider is now "+name, path, warnings, restart),
		m.persistSelection(),
	)
}

// configSetModel switches the active model; "default" or "" clears it so the
// provider's own default is used. The router reads it per turn.
func (m *Model) configSetModel(id string) tea.Cmd {
	if id == "default" {
		id = ""
	}
	path, warnings, restart, err := m.configApply(func(c *config.Config) { c.Model = id })
	if err != nil {
		return m.say(EntryError, "could not save the configuration: "+err.Error())
	}
	return tea.Batch(
		m.configApplied("active model is now "+orDefault(id, "(provider default)"), path, warnings, restart),
		m.persistSelection(),
	)
}

// configSetBaseURL sets a provider's endpoint. With one argument it targets the
// active provider; with two, the named one. A provider block is wired at
// construction, so this is restart-only.
func (m *Model) configSetBaseURL(args []string) tea.Cmd {
	var name, url string
	switch len(args) {
	case 1:
		name, url = m.app.Config().Provider, strings.TrimSpace(args[0])
	case 2:
		name, url = strings.TrimSpace(args[0]), strings.TrimSpace(args[1])
	default:
		return m.say(EntryError, "usage: /config base-url [provider] <url>")
	}
	if url == "" {
		return m.say(EntryError, "usage: /config base-url [provider] <url>")
	}
	if _, ok := m.app.Config().Providers[name]; !ok {
		return m.say(EntryError, fmt.Sprintf("no provider named %q is configured", name))
	}
	path, warnings, restart, err := m.configApply(func(c *config.Config) {
		pc := c.Providers[name]
		pc.BaseURL = url
		c.Providers[name] = pc
	})
	if err != nil {
		return m.say(EntryError, "could not save the configuration: "+err.Error())
	}
	return m.configApplied(fmt.Sprintf("%s base URL is now %s", name, url), path, warnings, restart)
}

// configSetAgents changes agents.enabled or agents.max, persisting the change
// and moving the live fleet with it.
func (m *Model) configSetAgents(sub, arg string) tea.Cmd {
	switch sub {
	case "on", "off":
		on := sub == "on"
		path, warnings, restart, err := m.configApply(func(c *config.Config) { c.Agents.Enabled = on })
		if err != nil {
			return m.say(EntryError, "could not save the configuration: "+err.Error())
		}
		if c := m.coordinator(); c != nil {
			c.SetEnabled(on)
		}
		if on {
			m.syncAgentCount()
		} else {
			m.fleet, m.agentsActive = nil, 0
		}
		return m.configApplied(fmt.Sprintf("agent delegation is %s", onOff(on)), path, warnings, restart)
	case "max":
		n, err := strconv.Atoi(strings.TrimSpace(arg))
		if err != nil || n < 1 {
			return m.say(EntryError, "usage: /config agents max <n> — a whole number, at least 1")
		}
		path, warnings, restart, err := m.configApply(func(c *config.Config) { c.Agents.Max = n })
		if err != nil {
			return m.say(EntryError, "could not save the configuration: "+err.Error())
		}
		if c := m.coordinator(); c != nil {
			_ = c.SetMax(n)
		}
		return m.configApplied(fmt.Sprintf("at most %d agent(s) will run at once", n), path, warnings, restart)
	default:
		return m.say(EntryError, "usage: /config agents on|off|max <n>")
	}
}

// configSetNetwork toggles outbound web access. It gates which tools are
// registered at construction, so it is restart-only.
func (m *Model) configSetNetwork(sub string) tea.Cmd {
	if sub != "on" && sub != "off" {
		return m.say(EntryError, "usage: /config network on|off")
	}
	on := sub == "on"
	path, warnings, restart, err := m.configApply(func(c *config.Config) { c.Network.Enabled = on })
	if err != nil {
		return m.say(EntryError, "could not save the configuration: "+err.Error())
	}
	return m.configApplied("outbound web access is "+onOff(on), path, warnings, restart)
}

// configSetLoopLimit changes execution.max_tool_iterations or
// execution.max_retries_per_command. The loop reads both when it starts a turn,
// so the change is live from the next message.
func (m *Model) configSetLoopLimit(which, arg string) tea.Cmd {
	n, err := strconv.Atoi(strings.TrimSpace(arg))
	if err != nil || n < 1 {
		return m.say(EntryError, fmt.Sprintf("usage: /config %s <n> — a whole number, at least 1", which))
	}
	label := "tool-loop iteration cap"
	apply := func(c *config.Config) { c.Execution.MaxToolIterations = n }
	if which == "max-retries" {
		label = "identical-retry cap per command"
		apply = func(c *config.Config) { c.Execution.MaxRetriesPerCommand = n }
	}
	path, warnings, restart, err := m.configApply(apply)
	if err != nil {
		return m.say(EntryError, "could not save the configuration: "+err.Error())
	}
	return m.configApplied(fmt.Sprintf("%s is now %d", label, n), path, warnings, restart)
}

// configSetTimeout changes execution.command_timeout. It is read when the
// executor is built, so it is restart-only.
func (m *Model) configSetTimeout(arg string) tea.Cmd {
	d, err := time.ParseDuration(strings.TrimSpace(arg))
	if err != nil || d <= 0 {
		return m.say(EntryError, "usage: /config timeout <duration> — e.g. 90s, 5m, 1h30m")
	}
	path, warnings, restart, err := m.configApply(func(c *config.Config) {
		c.Execution.CommandTimeout = config.Duration(d)
	})
	if err != nil {
		return m.say(EntryError, "could not save the configuration: "+err.Error())
	}
	return m.configApplied("per-command timeout is now "+d.String(), path, warnings, restart)
}

// configSetLog changes logging.level or logging.format. The logger opens once
// at startup, so both are restart-only.
func (m *Model) configSetLog(sub, arg string) tea.Cmd {
	arg = strings.TrimSpace(arg)
	switch sub {
	case "level":
		if _, err := logging.ParseLevel(arg); err != nil {
			return m.say(EntryError, "usage: /config log level <trace|debug|info|warn|error>")
		}
		path, warnings, restart, err := m.configApply(func(c *config.Config) { c.Logging.Level = arg })
		if err != nil {
			return m.say(EntryError, "could not save the configuration: "+err.Error())
		}
		return m.configApplied("log level is now "+arg, path, warnings, restart)
	case "format":
		if arg != "text" && arg != "json" {
			return m.say(EntryError, "usage: /config log format text|json")
		}
		path, warnings, restart, err := m.configApply(func(c *config.Config) { c.Logging.Format = arg })
		if err != nil {
			return m.say(EntryError, "could not save the configuration: "+err.Error())
		}
		return m.configApplied("log format is now "+arg, path, warnings, restart)
	default:
		return m.say(EntryError, "usage: /config log level <lvl> | /config log format text|json")
	}
}

// configSetWeb changes web.enabled, web.port or web.listen. The WebUI binds at
// startup (§22), so these are restart-only regardless of the running terminal.
func (m *Model) configSetWeb(sub, arg string) tea.Cmd {
	switch sub {
	case "on", "off":
		on := sub == "on"
		path, warnings, restart, err := m.configApply(func(c *config.Config) { c.Web.Enabled = on })
		if err != nil {
			return m.say(EntryError, "could not save the configuration: "+err.Error())
		}
		return m.configApplied(fmt.Sprintf("web.enabled = %t (start the server with `boop --web`)", on), path, warnings, restart)
	case "port":
		n, err := strconv.Atoi(strings.TrimSpace(arg))
		if err != nil || n < 1 || n > 65535 {
			return m.say(EntryError, "usage: /config web port <1-65535>")
		}
		path, warnings, restart, err := m.configApply(func(c *config.Config) { c.Web.Port = n })
		if err != nil {
			return m.say(EntryError, "could not save the configuration: "+err.Error())
		}
		return m.configApplied(fmt.Sprintf("web.port is now %d", n), path, warnings, restart)
	case "listen":
		addr := strings.TrimSpace(arg)
		if net.ParseIP(addr) == nil {
			return m.say(EntryError, "usage: /config web listen <ip> — an IP address (127.0.0.1 for local-only, 0.0.0.0 for every interface)")
		}
		path, warnings, restart, err := m.configApply(func(c *config.Config) { c.Web.Listen = addr })
		if err != nil {
			return m.say(EntryError, "could not save the configuration: "+err.Error())
		}
		return m.configApplied("web.listen is now "+addr, path, warnings, restart)
	default:
		return m.say(EntryError, "usage: /config web on|off|port <n>|listen <ip>")
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

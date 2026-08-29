package tui

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kawaiipantsu/boop/internal/config"
	"github.com/kawaiipantsu/boop/internal/permissions"
	"github.com/kawaiipantsu/boop/internal/provider"
	"github.com/kawaiipantsu/boop/internal/session"
	"github.com/kawaiipantsu/boop/internal/version"
)

// asyncTimeout bounds a command that touches the store or a provider, so a
// wedged backend cannot leave the UI showing a spinner forever.
const asyncTimeout = 20 * time.Second

// dispatch executes a parsed slash command.
//
// Commands that only read local state answer immediately; anything that talks
// to the store or a provider returns a tea.Cmd and answers with an infoMsg, so
// the update goroutine is never blocked on I/O.
func (m *Model) dispatch(cmd Command) tea.Cmd {
	spec, known := lookupCommand(cmd.Name)
	if !known {
		text := fmt.Sprintf("unknown command /%s — try /help", cmd.Name)
		if suggestion, ok := suggestCommand(cmd.Name); ok {
			text = fmt.Sprintf("unknown command /%s — did you mean /%s? Try /help", cmd.Name, suggestion)
		}
		return m.say(EntryError, text)
	}
	if spec.Status == cmdPending {
		return m.say(EntrySystem, fmt.Sprintf("/%s is not available yet: %s", spec.Name, spec.Blocker))
	}

	switch cmd.Name {
	case "help":
		return m.say(EntrySystem, helpText())
	case "boop":
		return m.say(EntrySystem, boopText())
	case "quit", "exit":
		return m.shutdown()
	case "clear":
		m.transcript.Clear()
		m.follow = true
		m.refresh()
		return nil
	case "reset":
		return m.resetConversation()
	case "status":
		return m.say(EntrySystem, m.statusText())
	case "stats":
		return m.say(EntrySystem, m.statsText())
	case "context":
		return m.contextCmd(cmd)
	case "tokens":
		return m.tokensCmd()
	case "prep", "init":
		return m.prepCmd(cmd)
	case "config":
		return m.configCmd(cmd)
	case "agents":
		return m.agentsCmd(cmd)
	case "run":
		return m.runCmd(cmd)
	case "test", "build":
		return m.execTaskCmd(cmd)
	case "files", "tree":
		return m.listCmd(cmd)
	case "search":
		return m.searchCmd(cmd)
	case "web":
		return m.webCmd(cmd)
	case "provider":
		return m.providerCmd(cmd)
	case "model":
		return m.modelCmd(cmd)
	case "models":
		return m.modelsCmd(cmd)
	case "permissions":
		return m.permissionsCmd(cmd)
	case "session":
		return m.sessionCmd(cmd)
	default:
		return m.say(EntryError, "/"+cmd.Name+" is listed but not dispatched; this is a bug")
	}
}

// say appends a local response to the transcript.
func (m *Model) say(kind EntryKind, text string) tea.Cmd {
	m.transcript.Append(Entry{Kind: kind, Text: text})
	m.follow = true
	m.refresh()
	return nil
}

// resetConversation starts over: the transcript, the conversation, the
// selected context and the counters all go, and a fresh session record is
// opened so the next turn is not appended to the old one.
//
// This is what separates /reset from /clear. /clear only empties the view —
// the model still remembers everything — whereas after /reset nothing that
// came before is sent to the model or written under the old session id.
func (m *Model) resetConversation() tea.Cmd {
	var system []provider.Message
	if len(m.history) > 0 && m.history[0].Role == provider.RoleSystem {
		system = m.history[:1]
	}
	m.history = append([]provider.Message(nil), system...)
	m.transcript.Clear()
	m.stats = Stats{}
	m.selection.Clear()
	m.fleet, m.agentsActive = nil, 0
	m.follow = true
	if m.app == nil {
		return m.say(EntrySystem, "conversation reset")
	}
	return m.newSessionCmd("everything before this point has been forgotten")
}

// ---------------------------------------------------------------------------
// Read-only reports
// ---------------------------------------------------------------------------

// statusText reports operational metadata (§54), never secrets.
func (m *Model) statusText() string {
	var b strings.Builder
	info := version.Get()
	fmt.Fprintf(&b, "boop %s (%s, %s)\n", info.Version, info.Commit, info.Platform)
	fmt.Fprintf(&b, "uptime      %s\n", formatDuration(time.Since(m.startedAt).Truncate(time.Second)))
	fmt.Fprintf(&b, "status      %s\n", m.status)
	fmt.Fprintf(&b, "session     %s", m.sessionID)
	if m.sessionTitle != "" {
		fmt.Fprintf(&b, "  (%s)", m.sessionTitle)
	}
	b.WriteString("\n")

	if m.app == nil {
		b.WriteString("runtime     not attached\n")
		return strings.TrimRight(b.String(), "\n")
	}
	cfg := m.app.Config
	fmt.Fprintf(&b, "provider    %s\n", m.providerModel())
	fmt.Fprintf(&b, "mode        %s\n", cfg.Execution.Mode)
	fmt.Fprintf(&b, "workdir     %s\n", m.workingDir())
	fmt.Fprintf(&b, "tools       %d registered: %s\n", len(m.app.Tools.Names()), strings.Join(m.app.Tools.Names(), ", "))
	fmt.Fprintf(&b, "agents      %s\n", m.agentStatusLine())
	fmt.Fprintf(&b, "web access  %s\n", onOff(cfg.Network.Enabled))
	fmt.Fprintf(&b, "webui       %s\n", m.webStatusLine())

	if m.app.Router != nil {
		names := m.app.Router.Registry().Names()
		fmt.Fprintf(&b, "providers   %s\n", strings.Join(names, ", "))
		health := m.app.Router.HealthSnapshot()
		keys := make([]string, 0, len(health))
		for name := range health {
			keys = append(keys, name)
		}
		sort.Strings(keys)
		for _, name := range keys {
			if err := health[name]; err != nil {
				fmt.Fprintf(&b, "  %-10s unhealthy: %s\n", name, err)
				continue
			}
			fmt.Fprintf(&b, "  %-10s ok\n", name)
		}
		if decision, ok := m.app.Router.LastDecision(); ok {
			fmt.Fprintf(&b, "last route  %s\n", decision)
		}
	}
	for _, w := range m.app.Warnings {
		fmt.Fprintf(&b, "warning     %s\n", w)
	}
	return strings.TrimRight(b.String(), "\n")
}

// statsText reports the counters for this session (§28).
func (m *Model) statsText() string {
	s := m.stats
	var b strings.Builder
	b.WriteString("session statistics\n")
	fmt.Fprintf(&b, "  turns            %d\n", s.Turns)
	fmt.Fprintf(&b, "  model round trips %d\n", s.Iterations)
	fmt.Fprintf(&b, "  messages         %d\n", s.Messages)
	fmt.Fprintf(&b, "  tool calls       %d (%d failed)\n", s.ToolCalls, s.ToolFailures)
	fmt.Fprintf(&b, "  approvals asked  %d (%d refused)\n", s.Approvals, s.Denied)
	fmt.Fprintf(&b, "  prompt tokens    %d\n", s.Prompt)
	fmt.Fprintf(&b, "  output tokens    %d\n", s.Completion)
	fmt.Fprintf(&b, "  total tokens     %d\n", s.Total)
	fmt.Fprintf(&b, "  elapsed          %s\n", formatDuration(time.Since(m.startedAt).Truncate(time.Second)))
	if m.app != nil && m.app.Config != nil && isLocalProvider(m.app.Config, m.app.Config.Provider) {
		b.WriteString("  api cost         0.00 (local provider)\n")
	} else {
		b.WriteString("  api cost         not tracked (no pricing metadata configured)\n")
	}
	if m.app != nil && m.app.Stats != nil {
		b.WriteString("\n")
		b.WriteString(trackerText(m.app.Stats.Snapshot(), m.sessionID))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// isLocalProvider reports whether the named provider runs on this machine.
//
// Local inference has no API bill, and §28 wants that stated rather than left
// blank. The judgement is made from the configured adapter type, not the
// provider name, so a locally-hosted OpenAI-compatible server still counts.
func isLocalProvider(cfg *config.Config, name string) bool {
	pc, ok := cfg.Providers[name]
	if !ok {
		return false
	}
	switch pc.Type {
	case "lemonade", "lmstudio", "ollama":
		return true
	case "openai-compatible":
		return isLoopbackURL(pc.BaseURL)
	default:
		return false
	}
}

// isLoopbackURL reports whether a base URL points at this machine.
func isLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func onOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}

// ---------------------------------------------------------------------------
// Commands that reach into the runtime
// ---------------------------------------------------------------------------

// tokensCmd reports this process's counters, the shared tracker and the
// persisted totals, which are three different facts and are labelled as such.
func (m *Model) tokensCmd() tea.Cmd {
	live := fmt.Sprintf("this session (live)\n  prompt %d · output %d · total %d",
		m.stats.Prompt, m.stats.Completion, m.stats.Total)
	if m.app == nil {
		return m.say(EntrySystem, live)
	}
	if m.app.Stats != nil {
		snap := m.app.Stats.Snapshot()
		agg, ok := snap.Sessions[m.sessionID]
		if !ok {
			agg = snap.Totals
		}
		if !agg.Tokens.Approximate().IsZero() {
			live += "\n\nshared tracker\n  " + tokenLine(agg.Tokens)
		}
	}
	sessions, id := m.app.Sessions, m.sessionID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), asyncTimeout)
		defer cancel()
		totals, err := sessions.Usage(ctx, id)
		if err != nil {
			return infoMsg{entries: []Entry{{Kind: EntrySystem, Text: live},
				{Kind: EntryError, Text: "stored totals unavailable: " + err.Error()}}}
		}
		stored := fmt.Sprintf("stored for this session\n  exchanges %d · prompt %d · output %d · total %d · cached %d",
			totals.Exchanges, totals.PromptTokens, totals.CompletionTokens, totals.TotalTokens, totals.CachedTokens)
		if totals.CostUSD > 0 {
			stored += fmt.Sprintf("\n  estimated cost $%.4f", totals.CostUSD)
		}
		return infoMsg{entries: []Entry{{Kind: EntrySystem, Text: live + "\n\n" + stored}}}
	}
}

// providerCmd shows or switches the active provider.
func (m *Model) providerCmd(cmd Command) tea.Cmd {
	if m.app == nil {
		return m.say(EntryError, "no runtime is attached")
	}
	names := m.app.Router.Registry().Names()
	if len(cmd.Args) == 0 {
		return m.say(EntrySystem, fmt.Sprintf("provider %s\nconfigured: %s\nswitch with /provider <name>",
			m.app.Config.Provider, strings.Join(names, ", ")))
	}
	want := cmd.Arg(0)
	if _, ok := m.app.Router.Registry().Get(want); !ok {
		return m.say(EntryError, fmt.Sprintf("no provider named %q is configured; available: %s",
			want, strings.Join(names, ", ")))
	}
	m.app.Config.Provider = want
	m.say(EntrySystem, "provider is now "+want)
	return m.persistSelection()
}

// modelCmd shows or switches the active model.
func (m *Model) modelCmd(cmd Command) tea.Cmd {
	if m.app == nil {
		return m.say(EntryError, "no runtime is attached")
	}
	if len(cmd.Args) == 0 {
		return m.say(EntrySystem, fmt.Sprintf("model %s on %s\nlist options with /models, switch with /model <id>",
			orDefault(m.app.Config.Model, "(provider default)"), m.app.Config.Provider))
	}
	m.app.Config.Model = cmd.Rest
	m.say(EntrySystem, "model is now "+cmd.Rest)
	return m.persistSelection()
}

// persistSelection records the provider/model choice on the session row, so a
// resumed session comes back on the same pairing.
func (m *Model) persistSelection() tea.Cmd {
	sessions, id := m.app.Sessions, m.sessionID
	providerName, model := m.app.Config.Provider, m.app.Config.Model
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), asyncTimeout)
		defer cancel()
		sess, err := sessions.Load(ctx, id)
		if err != nil {
			return infoMsg{entries: []Entry{{Kind: EntryError, Text: "could not update the session record: " + err.Error()}}}
		}
		if err := sessions.SetModel(ctx, sess, providerName, model); err != nil {
			return infoMsg{entries: []Entry{{Kind: EntryError, Text: "could not update the session record: " + err.Error()}}}
		}
		return infoMsg{}
	}
}

// modelsCmd lists what a provider offers (§8: capabilities are discovered).
func (m *Model) modelsCmd(cmd Command) tea.Cmd {
	if m.app == nil {
		return m.say(EntryError, "no runtime is attached")
	}
	name := cmd.Arg(0)
	if name == "" {
		name = m.app.Config.Provider
	}
	p, ok := m.app.Router.Registry().Get(name)
	if !ok {
		return m.say(EntryError, fmt.Sprintf("no provider named %q is configured; available: %s",
			name, strings.Join(m.app.Router.Registry().Names(), ", ")))
	}
	m.say(EntrySystem, "asking "+name+" for its models…")
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), asyncTimeout)
		defer cancel()
		models, err := p.ListModels(ctx)
		if err != nil {
			return infoMsg{entries: []Entry{{Kind: EntryError, Text: name + ": " + err.Error()}}}
		}
		return infoMsg{entries: []Entry{{Kind: EntrySystem, Text: formatModels(name, models)}}}
	}
}

// formatModels renders a provider's catalogue.
func formatModels(providerName string, models []provider.Model) string {
	if len(models) == 0 {
		return providerName + " reports no models"
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	width := 0
	for _, mod := range models {
		width = maxInt(width, len(mod.ID))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s offers %d model(s)\n", providerName, len(models))
	for _, mod := range models {
		fmt.Fprintf(&b, "  %s", padRight(mod.ID, width))
		if mod.ContextWindow > 0 {
			fmt.Fprintf(&b, "  ctx %d", mod.ContextWindow)
		}
		if caps := mod.Capabilities.Strings(); len(caps) > 0 {
			fmt.Fprintf(&b, "  [%s]", strings.Join(caps, " "))
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// permissionsCmd shows the policy and the standing session grants, and can
// switch execution mode.
//
// Grants are shown because a permission the user cannot see is one they cannot
// revoke; that is the whole reason the broker keeps them in memory only.
func (m *Model) permissionsCmd(cmd Command) tea.Cmd {
	if m.app == nil {
		return m.say(EntryError, "no runtime is attached")
	}
	switch cmd.Arg(0) {
	case "clear":
		if broker := m.broker(); broker != nil {
			broker.ClearSessionGrants()
		}
		return m.say(EntrySystem, "session grants cleared; every action will be asked about again")
	case "mode":
		mode := permissions.Mode(cmd.Arg(1))
		if !mode.Valid() {
			return m.say(EntryError, "usage: /permissions mode confirm|auto")
		}
		policy := m.app.Evaluator.Policy()
		policy.Mode = mode
		m.app.Evaluator.SetPolicy(policy)
		m.app.Config.Execution.Mode = mode
		return m.say(EntrySystem, "execution mode is now "+string(mode))
	case "":
		return m.say(EntrySystem, m.permissionsText())
	default:
		return m.say(EntryError, "usage: /permissions [mode confirm|auto] [clear]")
	}
}

// permissionsText renders the effective policy.
func (m *Model) permissionsText() string {
	policy := m.app.Evaluator.Policy()
	var b strings.Builder
	fmt.Fprintf(&b, "permission policy — mode %s\n", policy.Mode)
	if policy.Unrestricted {
		b.WriteString("  UNRESTRICTED: confirmation is disabled for this run\n")
	}
	if policy.ProductionAuthorized {
		b.WriteString("  production work is authorised for this session\n")
	}
	cats := make([]string, 0, len(policy.Rules))
	for cat := range policy.Rules {
		cats = append(cats, string(cat))
	}
	sort.Strings(cats)
	for _, cat := range cats {
		fmt.Fprintf(&b, "  %-20s %s\n", cat, policy.Rules[permissions.Category(cat)])
	}

	if broker := m.broker(); broker != nil {
		grants := broker.SessionGrants()
		if len(grants) == 0 {
			b.WriteString("\nno standing session grants\n")
		} else {
			b.WriteString("\nstanding session grants (memory only, cleared on exit)\n")
			for _, g := range grants {
				fmt.Fprintf(&b, "  %-16s %s\n", g.Scope, g.Label)
			}
			b.WriteString("  revoke them all with /permissions clear\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// sessionCmd manages sessions (§46).
func (m *Model) sessionCmd(cmd Command) tea.Cmd {
	if m.app == nil {
		return m.say(EntryError, "no runtime is attached")
	}
	switch cmd.Arg(0) {
	case "", "show":
		return m.say(EntrySystem, fmt.Sprintf("session %s\ntitle: %s\nsubcommands: new, list, save <title>, load <id>",
			m.sessionID, orDefault(m.sessionTitle, "(untitled)")))
	case "new":
		return m.newSessionCmd("")
	case "list":
		return m.listSessionsCmd()
	case "save":
		return m.saveSessionCmd(strings.Join(cmd.Args[1:], " "))
	case "load":
		if cmd.Arg(1) == "" {
			return m.say(EntryError, "usage: /session load <id>")
		}
		return m.loadSessionCmd(cmd.Arg(1))
	default:
		return m.say(EntryError, "usage: /session [new|list|save <title>|load <id>]")
	}
}

// newSessionCmd opens a fresh session record and switches to it. The note, when
// given, is shown under the confirmation — /reset uses it to say what was lost.
func (m *Model) newSessionCmd(note string) tea.Cmd {
	application := m.app
	system := m.systemMessage()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), asyncTimeout)
		defer cancel()
		sess, err := application.Sessions.Create(ctx, session.CreateOptions{
			ProjectPath: application.Workspace.Root(),
			Provider:    application.Config.Provider,
			Model:       application.Config.Model,
		})
		if err != nil {
			return infoMsg{entries: []Entry{{Kind: EntryError, Text: "could not start a session: " + err.Error()}}}
		}
		text := "started session " + sess.ID
		if note != "" {
			text += "\n" + note
		}
		return sessionSwitchedMsg{
			id:      sess.ID,
			history: system,
			entries: []Entry{{Kind: EntrySystem, Text: text}},
		}
	}
}

func (m *Model) listSessionsCmd() tea.Cmd {
	application := m.app
	current := m.sessionID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), asyncTimeout)
		defer cancel()
		sessions, err := application.Sessions.List(ctx, session.ListOptions{
			ProjectPath: application.Workspace.Root(),
			Limit:       20,
		})
		if err != nil {
			return infoMsg{entries: []Entry{{Kind: EntryError, Text: "could not list sessions: " + err.Error()}}}
		}
		return infoMsg{entries: []Entry{{Kind: EntrySystem, Text: formatSessions(sessions, current)}}}
	}
}

func formatSessions(sessions []*session.Session, current string) string {
	if len(sessions) == 0 {
		return "no sessions recorded for this project yet"
	}
	var b strings.Builder
	b.WriteString("sessions for this project (newest first)\n")
	for _, s := range sessions {
		marker := " "
		if s.ID == current {
			marker = "*"
		}
		fmt.Fprintf(&b, " %s %s  %s  %s/%s  %s\n", marker, shortID(s.ID),
			s.UpdatedAt.Local().Format("2006-01-02 15:04"),
			s.Provider, orDefault(s.Model, "default"), orDefault(s.Title, "(untitled)"))
	}
	b.WriteString("load one with /session load <id>")
	return b.String()
}

func (m *Model) saveSessionCmd(title string) tea.Cmd {
	title = strings.TrimSpace(title)
	if title == "" {
		return m.say(EntrySystem, "sessions are persisted continuously; pass a title to name this one: /session save <title>")
	}
	application, id := m.app, m.sessionID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), asyncTimeout)
		defer cancel()
		sess, err := application.Sessions.Load(ctx, id)
		if err != nil {
			return infoMsg{entries: []Entry{{Kind: EntryError, Text: "could not load the session: " + err.Error()}}}
		}
		if err := application.Sessions.SetTitle(ctx, sess, title); err != nil {
			return infoMsg{entries: []Entry{{Kind: EntryError, Text: "could not save the title: " + err.Error()}}}
		}
		return infoMsg{entries: []Entry{{Kind: EntrySystem, Text: "session titled " + strconv.Quote(title)}}}
	}
}

// loadSessionCmd resumes a stored session, rebuilding both the conversation
// sent to the model and the transcript the user reads.
func (m *Model) loadSessionCmd(id string) tea.Cmd {
	application := m.app
	system := m.systemMessage()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), asyncTimeout)
		defer cancel()
		sess, err := application.Sessions.Resume(ctx, id, application.Workspace.Root())
		if err != nil {
			return infoMsg{entries: []Entry{{Kind: EntryError, Text: "could not load session " + id + ": " + err.Error()}}}
		}
		messages, err := application.Sessions.History().Messages(ctx, sess.ID, session.TranscriptOptions{})
		if err != nil {
			return infoMsg{entries: []Entry{{Kind: EntryError, Text: "could not read the transcript: " + err.Error()}}}
		}
		history := append(append([]provider.Message(nil), system...), messages...)
		entries := append([]Entry{{Kind: EntrySystem,
			Text: fmt.Sprintf("resumed session %s with %d stored message(s)", sess.ID, len(messages))}},
			entriesFromMessages(messages)...)
		return sessionSwitchedMsg{id: sess.ID, title: sess.Title, history: history, entries: entries}
	}
}

// entriesFromMessages rebuilds transcript entries from stored messages.
func entriesFromMessages(messages []provider.Message) []Entry {
	out := make([]Entry, 0, len(messages))
	for _, msg := range messages {
		switch msg.Role {
		case provider.RoleUser:
			out = append(out, Entry{Kind: EntryUser, Text: msg.Content})
		case provider.RoleAssistant:
			if strings.TrimSpace(msg.Content) != "" {
				out = append(out, Entry{Kind: EntryAssistant, Text: msg.Content})
			}
			for _, call := range msg.ToolCalls {
				out = append(out, Entry{Kind: EntryTool, Tool: call.Name, Text: call.Arguments})
			}
		case provider.RoleTool:
			out = append(out, Entry{Kind: EntryOutput, Text: clipLines(msg.Content, maxToolOutputLines)})
		}
	}
	return out
}

// systemMessage returns the leading system prompt, if the conversation has one.
func (m *Model) systemMessage() []provider.Message {
	if len(m.history) > 0 && m.history[0].Role == provider.RoleSystem {
		return append([]provider.Message(nil), m.history[0])
	}
	return nil
}

func orDefault(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// ---------------------------------------------------------------------------
// /web
// ---------------------------------------------------------------------------

// webCmd reports where the WebUI is configured to run and how to start it.
//
// It deliberately does not start or stop a server. The WebUI is a peer
// frontend over the same runtime, not a subsystem of this one: `boop --web`
// owns a process, a signal handler and a shutdown sequence of its own, and a
// server whose lifetime is a slash command would outlive or predecease the
// terminal that spawned it in ways neither side can reason about. Reporting
// the address honestly is more useful than a half-owned listener (§22, §58).
func (m *Model) webCmd(cmd Command) tea.Cmd {
	if m.app == nil || m.app.Config == nil {
		return m.say(EntryError, "no runtime is attached")
	}
	switch cmd.Arg(0) {
	case "", "show", "status":
		return m.say(EntrySystem, m.webText())
	case "on", "off":
		return m.say(EntrySystem, m.webText())
	default:
		return m.say(EntryError, "usage: /web [on|off]")
	}
}

// webText describes the configured WebUI.
func (m *Model) webText() string {
	cfg := m.app.Config
	var b strings.Builder
	b.WriteString("the WebUI is a separate process; this terminal does not start one\n")
	fmt.Fprintf(&b, "  configured  http://%s:%d\n", cfg.Web.Listen, cfg.Web.Port)
	fmt.Fprintf(&b, "  enabled     %t\n", cfg.Web.Enabled)
	fmt.Fprintf(&b, "  auth        %s\n", onOff(cfg.Web.Auth.Enabled))
	b.WriteString("  start it with `boop --web` (add --listen/--port to override)\n")
	b.WriteString("  it shares the same store, so a session started here can be opened there")
	return b.String()
}

// webStatusLine is the /status row for the WebUI.
func (m *Model) webStatusLine() string {
	cfg := m.app.Config
	return fmt.Sprintf("not served from this process (%s:%d, enabled=%t) — run `boop --web`",
		cfg.Web.Listen, cfg.Web.Port, cfg.Web.Enabled)
}

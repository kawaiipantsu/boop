package tui

import (
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kawaiipantsu/boop/internal/agent"
)

// agentsCmd implements /agents and its subcommands (§10).
func (m *Model) agentsCmd(cmd Command) tea.Cmd {
	if m.app == nil {
		return m.say(EntryError, "no runtime is attached")
	}
	switch cmd.Arg(0) {
	case "on":
		return m.setAgentsEnabled(true)
	case "off":
		return m.setAgentsEnabled(false)
	case "max":
		return m.setAgentsMax(cmd.Arg(1))
	case "stop":
		return m.stopAgent(strings.Join(cmd.Args[1:], " "))
	case "list":
		return m.listAgents()
	case "", "show":
		return m.showAgents()
	default:
		return m.say(EntryError, "usage: /agents [list|on|off|max <n>|stop <id>]")
	}
}

// coordinator returns the fleet for this session, building it on first use.
//
// A nil return means delegation is off — that is how agent.NewFromApp reports
// `agents.enabled: false`, and it is not an error. Every caller must check,
// because a nil Coordinator is not usable.
func (m *Model) coordinator() *agent.Coordinator {
	if m.app == nil {
		return nil
	}
	if m.fleet == nil {
		m.fleet = agent.NewFromApp(m.app, m.sessionID)
	}
	return m.fleet
}

// agentsOffText explains a nil coordinator once, in one place.
func agentsOffText() string {
	return "agent delegation is off (agents.enabled = false), so nothing can be delegated.\n" +
		"turn it on for this run with /agents on."
}

func (m *Model) showAgents() tea.Cmd {
	c := m.coordinator()
	if c == nil {
		return m.say(EntrySystem, agentsOffText())
	}
	snap := c.Snapshot()
	m.agentsActive = snap.Active
	var b strings.Builder
	b.WriteString(snap.String())
	fmt.Fprintf(&b, "\nlimits: %d concurrent · depth %d · %d agents per objective",
		snap.Max, snap.MaxDepth, snap.MaxAgents)
	b.WriteString("\nsubcommands: list, on, off, max <n>, stop <id>")
	return m.say(EntrySystem, b.String())
}

func (m *Model) listAgents() tea.Cmd {
	c := m.coordinator()
	if c == nil {
		return m.say(EntrySystem, agentsOffText())
	}
	agents := c.List()
	m.syncAgentCount()
	if len(agents) == 0 {
		return m.say(EntrySystem, "no agents have run in this session")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d agent(s)\n", len(agents))
	for _, a := range agents {
		b.WriteString("  " + a.Line() + "\n")
	}
	b.WriteString("stop one with /agents stop <id> — a unique id prefix is enough")
	return m.say(EntrySystem, strings.TrimRight(b.String(), "\n"))
}

func (m *Model) stopAgent(id string) tea.Cmd {
	id = strings.TrimSpace(id)
	if id == "" {
		return m.say(EntryError, "usage: /agents stop <id>")
	}
	c := m.coordinator()
	if c == nil {
		return m.say(EntrySystem, agentsOffText())
	}
	if err := c.Stop(id); err != nil {
		return m.say(EntryError, "cannot stop "+id+": "+err.Error())
	}
	m.syncAgentCount()
	return m.say(EntrySystem, "stopped agent "+id)
}

// setAgentsMax changes the concurrency ceiling, which takes effect on work
// that has not started yet — including work already under way.
func (m *Model) setAgentsMax(arg string) tea.Cmd {
	n, err := strconv.Atoi(strings.TrimSpace(arg))
	if err != nil {
		return m.say(EntryError, "usage: /agents max <n> — a whole number of concurrent agents")
	}
	c := m.coordinator()
	if c == nil {
		// Record the ceiling anyway: it is the value the fleet will start
		// with once /agents on builds one.
		if n < 1 {
			return m.say(EntryError, fmt.Sprintf("the agent limit must be at least 1, got %d", n))
		}
		m.app.Config.Agents.Max = n
		return m.say(EntrySystem, fmt.Sprintf("agent limit set to %d; %s", n, agentsOffText()))
	}
	if err := c.SetMax(n); err != nil {
		return m.say(EntryError, err.Error())
	}
	m.app.Config.Agents.Max = n
	return m.say(EntrySystem, fmt.Sprintf("at most %d agent(s) will run at once", n))
}

// setAgentsEnabled implements /agents on and /agents off.
//
// Both the configuration flag and the live coordinator are moved together:
// the flag is what agent.NewFromApp reads when a fleet is next built, and the
// coordinator is what stops the agents already running.
func (m *Model) setAgentsEnabled(on bool) tea.Cmd {
	if !on {
		if c := m.coordinator(); c != nil {
			c.SetEnabled(false)
		}
		m.app.Config.Agents.Enabled = false
		m.fleet, m.agentsActive = nil, 0
		return m.say(EntrySystem, "agents are off; anything running has been cancelled")
	}

	m.app.Config.Agents.Enabled = true
	c := m.coordinator()
	if c == nil {
		return m.say(EntryError, "agents could not be enabled; the runtime has no coordinator")
	}
	c.SetEnabled(true)
	m.syncAgentCount()
	return m.say(EntrySystem, fmt.Sprintf("agents are on, up to %d at once", c.Max()))
}

// syncAgentCount refreshes the header's running-agent figure.
//
// It is called from the clock tick and from agent events rather than from
// View, because View must stay a pure function of the model and a snapshot
// copies the whole fleet.
func (m *Model) syncAgentCount() {
	c := m.fleet
	if c == nil {
		m.agentsActive = 0
		return
	}
	m.agentsActive = c.Snapshot().Active
}

// agentStatusLine is the /status row for the fleet.
func (m *Model) agentStatusLine() string {
	cfg := m.app.Config
	c := m.fleet
	if c == nil {
		// No fleet has been built yet, so nothing is running whatever the
		// configuration says; report the setting rather than inventing state.
		if cfg.Agents.Enabled {
			return fmt.Sprintf("enabled, none started (max=%d)", cfg.Agents.Max)
		}
		return fmt.Sprintf("off — agents.enabled = false (max=%d)", cfg.Agents.Max)
	}
	snap := c.Snapshot()
	return fmt.Sprintf("%s · %d cancelled · %d known", snap.Summary(), snap.Cancelled, snap.Total)
}

package tui

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kawaiipantsu/boop/internal/permissions"
	"github.com/kawaiipantsu/boop/internal/version"
)

// View implements tea.Model.
//
// It is a pure function of the model: every row comes from the layout budget
// computed in relayout, so the frame is always exactly Height rows tall and a
// resize can never leave a torn screen behind.
func (m *Model) View() string {
	if !m.ready || m.width <= 0 || m.height <= 0 {
		return "starting boop…"
	}
	if m.quitting {
		return ""
	}

	l := m.layout
	rows := make([]string, 0, l.Height)

	if l.Header > 0 {
		rows = append(rows, m.headerRow())
	}
	if l.Rules > 0 {
		rows = append(rows, m.ruleRow())
	}
	rows = append(rows, m.bodyRows()...)
	rows = append(rows, m.approvalRows()...)
	if l.Rules > 1 {
		rows = append(rows, m.ruleRow())
	}
	rows = append(rows, m.inputRows()...)
	if l.Footer > 0 {
		rows = append(rows, m.footerRow())
	}
	return strings.Join(rows, "\n")
}

// headerRow renders the identity/status bar (§19).
func (m *Model) headerRow() string {
	left, right := HeaderSegments(m.width)
	brand := m.theme.headerBrand.Render(padRight(" BOOP "+version.Get().Version, left))

	status := string(m.status)
	statusCell := m.theme.statusStyle(m.status).Render(status)

	fields := []string{
		m.theme.headerBar.Render(m.providerModel()),
		m.theme.headerDim.Render("mode " + m.modeName()),
		statusCell,
		m.theme.headerDim.Render(fmt.Sprintf("agents %d", m.agentCount())),
		m.theme.headerDim.Render(m.tokenField()),
	}
	if m.networkOn() {
		// Outbound web access sends the user's text to third parties, so it
		// is called out rather than implied (§19, invariant on network).
		fields = append(fields, m.theme.webOn.Render("WEB↑ON"))
	} else {
		fields = append(fields, m.theme.webOff.Render("web off"))
	}

	sep := m.theme.headerDim.Render(" · ")
	body := strings.Join(fields, sep)
	// Pad on the plain text width so escape codes do not skew the alignment.
	plain := m.headerPlain()
	if gap := right - displayWidth(plain) - 1; gap > 0 {
		body = m.theme.headerBar.Render(strings.Repeat(" ", gap)) + body
	} else if right > 1 {
		body = m.theme.headerBar.Render(truncate(plain, right-1))
	}
	return brand + body + m.theme.headerBar.Render(" ")
}

// headerPlain is the unstyled form of the header's right side, used for width
// maths.
func (m *Model) headerPlain() string {
	web := "web off"
	if m.networkOn() {
		web = "WEB↑ON"
	}
	return strings.Join([]string{
		m.providerModel(),
		"mode " + m.modeName(),
		string(m.status),
		fmt.Sprintf("agents %d", m.agentCount()),
		m.tokenField(),
		web,
	}, " · ")
}

func (m *Model) ruleRow() string {
	return m.theme.rule.Render(strings.Repeat("─", maxInt(1, m.width)))
}

// bodyRows renders the transcript viewport padded to its budgeted height, or
// the full-screen config editor when it is open.
func (m *Model) bodyRows() []string {
	if m.layout.Body <= 0 {
		return nil
	}
	if m.editor != nil {
		return m.editorRows()
	}
	view := strings.Split(m.viewport.View(), "\n")
	rows := make([]string, 0, m.layout.Body)
	pad := strings.Repeat(" ", framePadding)
	for i := 0; i < m.layout.Body; i++ {
		if i < len(view) {
			rows = append(rows, pad+view[i])
			continue
		}
		rows = append(rows, "")
	}
	return rows
}

// approvalRows renders the inline permission prompt.
func (m *Model) approvalRows() []string {
	if m.layout.Approval <= 0 || m.prompt == nil {
		return nil
	}
	lines := m.prompt.Lines(m.layout.ContentWidth())
	rows := make([]string, 0, m.layout.Approval)
	pad := strings.Repeat(" ", framePadding)
	for i := 0; i < m.layout.Approval; i++ {
		if i >= len(lines) {
			rows = append(rows, "")
			continue
		}
		line := lines[i]
		if line.Style == promptButtons {
			rows = append(rows, pad+m.renderButtons())
			continue
		}
		rows = append(rows, pad+m.theme.promptStyleFor(line.Style).Render(line.Text))
	}
	return rows
}

// renderButtons paints the answer buttons with the highlight on the current
// choice.
func (m *Model) renderButtons() string {
	var b strings.Builder
	b.WriteString(strings.Repeat(" ", buttonIndent))
	for i, c := range m.prompt.choices {
		if i > 0 {
			b.WriteString("  ")
		}
		marker := " "
		if i == m.prompt.cursor {
			marker = ">"
		}
		text := fmt.Sprintf("[%s%s (%c) ]", marker, c.Label, c.Key)
		if i == m.prompt.cursor {
			b.WriteString(m.theme.buttonActive.Render(text))
			continue
		}
		b.WriteString(m.theme.buttonIdle.Render(text))
	}
	return b.String()
}

// inputRows renders the composer.
func (m *Model) inputRows() []string {
	if m.layout.Input <= 0 {
		return nil
	}
	view := strings.Split(m.input.View(), "\n")
	rows := make([]string, 0, m.layout.Input)
	pad := strings.Repeat(" ", framePadding)
	for i := 0; i < m.layout.Input; i++ {
		if i < len(view) {
			rows = append(rows, pad+view[i])
			continue
		}
		rows = append(rows, "")
	}
	return rows
}

// footerRow renders the status line: what Boop is doing, where, and the keys
// that matter right now.
func (m *Model) footerRow() string {
	width := maxInt(1, m.width)

	var left string
	switch {
	case m.search != nil:
		left = m.searchField()
	case m.notice != "":
		left = m.theme.notice.Render(truncate(m.notice, width-1))
	default:
		left = m.theme.footer.Render(m.contextField())
	}

	hints := m.hints()
	plainLeft := displayWidth(stripStyle(left))
	if gap := width - plainLeft - displayWidth(hints) - 1; gap > 0 {
		return " " + left + strings.Repeat(" ", gap) + m.theme.footer.Render(hints)
	}
	return " " + left
}

// stripStyle removes escape sequences so widths can be measured.
func stripStyle(s string) string { return sanitize(s) }

// searchField renders the reverse history search prompt.
func (m *Model) searchField() string {
	state := "no match"
	if m.search.found {
		state = m.search.match
	}
	return m.theme.footerKey.Render("(history search)") +
		m.theme.footer.Render(fmt.Sprintf(" `%s': %s", m.search.query, truncate(state, maxInt(8, m.width/2))))
}

// contextField describes where the session is working.
func (m *Model) contextField() string {
	parts := []string{}
	if dir := m.workingDir(); dir != "" {
		parts = append(parts, filepath.Base(dir))
	}
	if m.sessionID != "" {
		parts = append(parts, "session "+shortID(m.sessionID))
	}
	if m.waiting > 1 {
		parts = append(parts, fmt.Sprintf("%d approvals waiting", m.waiting))
	}
	if !m.follow {
		parts = append(parts, "scrolled up — Ctrl+End to follow")
	}
	return truncate(strings.Join(parts, " · "), maxInt(1, m.width-24))
}

// hints lists the bindings relevant to the current state.
func (m *Model) hints() string {
	switch {
	case m.prompt != nil:
		return "a approve · s session · r/Esc reject"
	case m.editor != nil:
		return "↑↓ move · ←→ change · enter edit · Ctrl+S save · Esc cancel"
	case m.turnActive:
		return "Ctrl+C cancel"
	default:
		return "Enter send · Ctrl+J newline · /help · Ctrl+C quit"
	}
}

// ---------------------------------------------------------------------------
// Header data helpers
// ---------------------------------------------------------------------------

func (m *Model) providerModel() string {
	if m.app == nil || m.app.Config() == nil {
		return "no provider"
	}
	model := m.app.Config().Model
	if model == "" {
		model = "default"
	}
	return m.app.Config().Provider + "/" + model
}

func (m *Model) modeName() string {
	if m.app == nil || m.app.Config() == nil {
		return string(permissions.ModeConfirm)
	}
	return string(m.app.Config().Execution.Mode)
}

// agentCount is the number of agents currently occupying a concurrency slot.
//
// It reads the cached figure rather than snapshotting the coordinator: View
// runs on every keystroke and must not mutate the model or copy the fleet.
// syncAgentCount refreshes it on the clock tick and on every agent event, so
// the header is at most one second behind (§19).
func (m *Model) agentCount() int { return m.agentsActive }

func (m *Model) networkOn() bool {
	return m.app != nil && m.app.Config() != nil && m.app.Config().Network.Enabled
}

func (m *Model) workingDir() string {
	if m.app == nil || m.app.Workspace == nil {
		return ""
	}
	return m.app.Workspace.Root()
}

// tokenField summarises token spend and how much conversation is loaded.
func (m *Model) tokenField() string {
	return fmt.Sprintf("%s tok · ctx ~%s", compactCount(m.stats.Total), compactCount(m.contextTokens()))
}

// contextTokens estimates the conversation size (§47 makes the real selection;
// this is only the readout).
func (m *Model) contextTokens() int {
	total := 0
	for _, msg := range m.history {
		total += estimateTokens(msg.Content)
	}
	return total
}

// estimateTokens approximates tokens from characters.
//
// Boop deliberately does not ship a tokenizer per provider: the header needs a
// figure that is right to within a factor, not an exact count, and a wrong
// exact number would be worse than an obvious estimate.
func estimateTokens(s string) int { return (len(s) + 3) / 4 }

// compactCount renders large counts briefly.
func compactCount(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
}

// shortID abbreviates a UUID for status lines.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// handleKey routes a keystroke to whichever surface currently owns input.
//
// Priority is deliberate: a pending approval takes the keyboard, because an
// unmissable prompt that can be typed past is not unmissable (§49). History
// search comes next, then global bindings, and only then the composer.
func (m *Model) handleKey(msg tea.KeyMsg) tea.Cmd {
	key := msg.String()

	// Ctrl+C is handled everywhere, including inside a dialog.
	if key == "ctrl+c" {
		return m.interrupt()
	}
	// Any other keystroke clears both the armed quit and the transient
	// notice explaining it.
	m.interruptArmed = false
	m.notice = ""

	if m.prompt != nil {
		return m.approvalKey(msg)
	}
	if m.editor != nil {
		return m.editorKey(msg)
	}
	if m.search != nil {
		return m.searchKey(msg)
	}

	switch key {
	case "esc":
		if m.input.Value() != "" {
			m.input.Reset()
			m.relayout()
			return nil
		}
		m.follow = true
		m.viewport.GotoBottom()
		return nil

	case "ctrl+d":
		if m.input.Value() == "" {
			return m.shutdown()
		}

	case "ctrl+o":
		return m.toggleMouse()

	case "ctrl+r":
		m.search = &searchState{}
		return nil

	case "ctrl+j":
		m.input.InsertString("\n")
		m.relayout()
		return nil

	case "pgup":
		m.viewport.PageUp()
		m.follow = m.viewport.AtBottom()
		return nil

	case "pgdown":
		m.viewport.PageDown()
		m.follow = m.viewport.AtBottom()
		return nil

	case "ctrl+home":
		m.viewport.GotoTop()
		m.follow = false
		return nil

	case "ctrl+end":
		m.viewport.GotoBottom()
		m.follow = true
		return nil

	case "shift+up":
		m.viewport.ScrollUp(1)
		m.follow = m.viewport.AtBottom()
		return nil

	case "shift+down":
		m.viewport.ScrollDown(1)
		m.follow = m.viewport.AtBottom()
		return nil

	case "up":
		if m.historyNavigable() {
			return m.recallHistory(-1)
		}

	case "down":
		if m.historyNavigable() {
			return m.recallHistory(1)
		}
	}

	if isSubmit(msg, m.input.Value()) {
		text := m.input.Value()
		m.input.Reset()
		m.relayout()
		return m.submit(text)
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	m.relayout()
	return cmd
}

// isSubmit decides whether a keystroke sends the message.
//
// §19 asks for Enter to insert a newline in multi-line mode with Ctrl+Enter or
// Alt+Enter to send. Terminals almost universally deliver Ctrl+Enter as a
// plain Enter, so the rule is: while the message is a single line Enter sends
// it, and once it spans several lines Enter becomes a newline and Alt+Enter or
// Ctrl+S sends. The composer never traps a message the user cannot get out of.
func isSubmit(msg tea.KeyMsg, value string) bool {
	switch msg.Type {
	case tea.KeyCtrlS:
		return true
	case tea.KeyEnter:
		if msg.Alt {
			return true
		}
		return !strings.Contains(value, "\n")
	default:
		return false
	}
}

// historyNavigable reports whether Up/Down should walk input history rather
// than move the cursor. A single-line composer always navigates; a multi-line
// one only at its edges, so arrow keys still work inside a long message.
func (m *Model) historyNavigable() bool {
	if m.input.LineCount() <= 1 {
		return true
	}
	line := m.input.Line()
	return line == 0 || line == m.input.LineCount()-1
}

// recallHistory steps through submitted inputs; -1 is older, +1 is newer.
func (m *Model) recallHistory(delta int) tea.Cmd {
	n := len(m.inputHistory)
	if n == 0 {
		return nil
	}
	if m.histIdx == -1 {
		if delta > 0 {
			return nil
		}
		m.histDraft = m.input.Value()
		m.histIdx = n
	}
	next := m.histIdx + delta
	switch {
	case next < 0:
		next = 0
	case next >= n:
		m.histIdx = -1
		m.input.SetValue(m.histDraft)
		m.relayout()
		return nil
	}
	m.histIdx = next
	m.input.SetValue(m.inputHistory[next])
	m.relayout()
	return nil
}

// interrupt implements the two-stage Ctrl+C contract (§51): the first press
// stops whatever is running, and a second press with nothing running quits.
func (m *Model) interrupt() tea.Cmd {
	if m.interruptArmed {
		return m.shutdown()
	}
	m.interruptArmed = true
	if m.prompt != nil || m.turnActive {
		m.cancelTurn()
		m.notice = "interrupted — press Ctrl+C again to quit"
		m.status = StatusIdle
		m.relayout()
		return nil
	}
	m.notice = "press Ctrl+C again to quit, or use /quit"
	return nil
}

// toggleMouse turns mouse reporting on and off.
//
// With reporting on the terminal hands drags to Boop, which means the user
// loses native text selection. Copying output is a normal thing to want, so
// the capture is switchable rather than permanent.
func (m *Model) toggleMouse() tea.Cmd {
	m.mouseOn = !m.mouseOn
	if m.mouseOn {
		m.notice = "mouse capture on"
		return tea.EnableMouseCellMotion
	}
	m.notice = "mouse capture off — the terminal can select text again"
	return tea.DisableMouse
}

// approvalKey handles input while a permission request is on screen.
func (m *Model) approvalKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		// Esc is a denial, never a dismissal: silently closing a permission
		// prompt would leave the loop parked and the user guessing (§51).
		return m.answerApproval(approvalChoice{Approved: false})
	case "left", "shift+tab":
		m.prompt.move(-1)
		return nil
	case "right", "tab":
		m.prompt.move(1)
		return nil
	case "enter", " ":
		return m.answerApproval(m.prompt.selected())
	}
	if len(msg.Runes) == 1 {
		if choice, ok := m.prompt.choiceFor(lowerRune(msg.Runes[0])); ok {
			return m.answerApproval(choice)
		}
	}
	return nil
}

func lowerRune(r rune) rune {
	if r >= 'A' && r <= 'Z' {
		return r + ('a' - 'A')
	}
	return r
}

// searchKey handles Ctrl+R reverse history search.
func (m *Model) searchKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc", "ctrl+g":
		m.search = nil
		return nil
	case "enter":
		if m.search.found {
			m.input.SetValue(m.search.match)
		}
		m.search = nil
		m.relayout()
		return nil
	case "backspace":
		if q := m.search.query; q != "" {
			m.search.query = q[:len(q)-1]
		}
	case "ctrl+r":
		// Repeating Ctrl+R walks to the next older match.
		m.searchOlder()
		return nil
	default:
		if len(msg.Runes) > 0 {
			m.search.query += string(msg.Runes)
		}
	}
	m.search.match, m.search.found = m.findHistory(m.search.query, len(m.inputHistory))
	return nil
}

// searchOlder advances the search past the current match.
func (m *Model) searchOlder() {
	limit := len(m.inputHistory)
	for i := len(m.inputHistory) - 1; i >= 0; i-- {
		if m.inputHistory[i] == m.search.match {
			limit = i
			break
		}
	}
	if match, ok := m.findHistory(m.search.query, limit); ok {
		m.search.match, m.search.found = match, true
	}
}

// findHistory returns the newest entry before limit containing query.
func (m *Model) findHistory(query string, limit int) (string, bool) {
	if query == "" {
		return "", false
	}
	limit = minInt(limit, len(m.inputHistory))
	for i := limit - 1; i >= 0; i-- {
		if strings.Contains(m.inputHistory[i], query) {
			return m.inputHistory[i], true
		}
	}
	return "", false
}

// handleMouse implements the pointer interactions §19 asks for: scrolling the
// transcript and clicking the approval buttons.
func (m *Model) handleMouse(msg tea.MouseMsg) tea.Cmd {
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		m.viewport.ScrollUp(3)
		m.follow = m.viewport.AtBottom()
		return nil
	case tea.MouseButtonWheelDown:
		m.viewport.ScrollDown(3)
		m.follow = m.viewport.AtBottom()
		return nil
	}
	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return nil
	}
	if m.prompt != nil {
		if row, ok := m.buttonRowIndex(); ok && msg.Y == row {
			_, spans := buttonRow(m.prompt.choices, m.prompt.cursor)
			if i := hitButton(spans, msg.X-framePadding); i >= 0 {
				m.prompt.cursor = i
				return m.answerApproval(m.prompt.choices[i])
			}
		}
	}
	return nil
}

// buttonRowIndex returns the screen row the approval buttons occupy.
func (m *Model) buttonRowIndex() (int, bool) {
	if m.prompt == nil {
		return 0, false
	}
	top := m.layout.Header + minInt(m.layout.Rules, 1) + m.layout.Body
	for i, line := range m.prompt.Lines(m.layout.ContentWidth()) {
		if i >= m.layout.Approval {
			break
		}
		if line.Style == promptButtons {
			return top + i, true
		}
	}
	return 0, false
}

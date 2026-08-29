package tui

import (
	"fmt"
	"strings"

	"github.com/kawaiipantsu/boop/internal/permissions"
)

// approvalChoice is one answer the operator can give to a permission request.
type approvalChoice struct {
	// Key is the single character that selects this choice.
	Key rune
	// Label is what the button says.
	Label string
	// Approved is the answer this choice sends.
	Approved bool
	// Scope is how far the answer reaches.
	Scope permissions.GrantScope
}

// approvalChoices builds the answers offered for an action.
//
// "Always for session" is offered only when the permission engine says the
// answer may be remembered. Critical risk and anything touching production
// always require a fresh decision, and the rule lives in permissions.
// CanGrantSession so the TUI, WebUI and CLI cannot disagree about it (§49).
func approvalChoices(action permissions.Action) []approvalChoice {
	out := []approvalChoice{
		{Key: 'a', Label: "Approve once", Approved: true, Scope: permissions.ScopeOnce},
	}
	if permissions.CanGrantSession(action) {
		out = append(out, approvalChoice{
			Key: 's', Label: "Always for session",
			Approved: true, Scope: permissions.ScopeSessionCommand,
		})
	}
	out = append(out, approvalChoice{Key: 'r', Label: "Reject", Approved: false, Scope: permissions.ScopeOnce})
	return out
}

// promptStyle tags a prompt row so the view can paint it without re-deriving
// what the row means.
type promptStyle int

const (
	promptPlain promptStyle = iota
	promptTitle
	promptProduction
	promptDetail
	promptRisk
	promptReason
	promptButtons
	promptHint
)

// promptLine is one row of the approval prompt, free of escape codes.
type promptLine struct {
	Text  string
	Style promptStyle
}

// approvalPrompt is the inline permission request shown above the composer.
type approvalPrompt struct {
	pending permissions.PendingApproval
	choices []approvalChoice
	cursor  int
	// queued counts further requests waiting behind this one.
	queued int
}

// newApprovalPrompt builds the prompt for a pending request.
func newApprovalPrompt(p permissions.PendingApproval) *approvalPrompt {
	return &approvalPrompt{pending: p, choices: approvalChoices(p.Action)}
}

// selected returns the highlighted choice.
func (a *approvalPrompt) selected() approvalChoice { return a.choices[a.cursor] }

// move steps the highlight, wrapping at both ends.
func (a *approvalPrompt) move(delta int) {
	n := len(a.choices)
	if n == 0 {
		return
	}
	a.cursor = ((a.cursor+delta)%n + n) % n
}

// choiceFor returns the choice bound to a key, if any.
func (a *approvalPrompt) choiceFor(key rune) (approvalChoice, bool) {
	for _, c := range a.choices {
		if c.Key == key {
			return c, true
		}
	}
	return approvalChoice{}, false
}

// buttonIndent is the left inset of the button row, used for hit testing.
const buttonIndent = 2

// buttonSpan is the column range a button occupies, so a mouse click can be
// resolved back to a choice.
type buttonSpan struct {
	Index int
	Start int
	End   int
}

// buttonRow renders the buttons and reports where each one landed.
func buttonRow(choices []approvalChoice, cursor int) (string, []buttonSpan) {
	var b strings.Builder
	b.WriteString(strings.Repeat(" ", buttonIndent))
	spans := make([]buttonSpan, 0, len(choices))
	for i, c := range choices {
		if i > 0 {
			b.WriteString("  ")
		}
		marker := " "
		if i == cursor {
			marker = ">"
		}
		text := fmt.Sprintf("[%s%s (%c) ]", marker, c.Label, c.Key)
		start := displayWidth(b.String())
		b.WriteString(text)
		spans = append(spans, buttonSpan{Index: i, Start: start, End: start + displayWidth(text)})
	}
	return b.String(), spans
}

// hitButton resolves a click at column x to a choice index, or -1.
func hitButton(spans []buttonSpan, x int) int {
	for _, s := range spans {
		if x >= s.Start && x < s.End {
			return s.Index
		}
	}
	return -1
}

// Lines renders the prompt wrapped to width.
//
// Everything the operator needs to judge the request is here and none of it is
// elided: what Boop wants to do, the literal command or path, the risk, and
// why the engine is asking. A production action says so on its own line
// because the difference between "this repo" and "the live system" is the
// single most important thing on the screen (§49, §15).
func (a *approvalPrompt) Lines(width int) []promptLine {
	action := a.pending.Action
	var out []promptLine

	title := "APPROVAL REQUIRED"
	style := promptTitle
	if action.Production {
		title = "!! PRODUCTION CHANGE — APPROVAL REQUIRED"
		style = promptProduction
	}
	if a.queued > 0 {
		title += fmt.Sprintf("   (%d more waiting)", a.queued)
	}
	out = append(out, promptLine{Text: truncate(title, width), Style: style})

	summary := action.Summary
	if summary == "" {
		summary = fmt.Sprintf("%s wants to act", action.Tool)
	}
	for _, l := range wrapBlock("Boop wants to: "+summary, "", "  ", width) {
		out = append(out, promptLine{Text: l})
	}

	if action.Detail != "" && action.Detail != action.Summary {
		for _, l := range wrapBlock(action.Detail, "  ", "  ", width) {
			out = append(out, promptLine{Text: l, Style: promptDetail})
		}
	}
	if len(action.Paths) > 0 {
		for _, l := range wrapBlock("paths: "+strings.Join(action.Paths, ", "), "  ", "    ", width) {
			out = append(out, promptLine{Text: l, Style: promptDetail})
		}
	}

	risk := fmt.Sprintf("  risk: %s   category: %s", strings.ToUpper(string(action.Risk)), action.Category)
	if action.Production {
		risk += "   affects production"
	}
	out = append(out, promptLine{Text: truncate(risk, width), Style: promptRisk})

	if reason := a.pending.Decision.Reason; reason != "" {
		for _, l := range wrapBlock(reason, "  ", "  ", width) {
			out = append(out, promptLine{Text: l, Style: promptReason})
		}
	}

	row, _ := buttonRow(a.choices, a.cursor)
	out = append(out, promptLine{Text: truncate(row, width), Style: promptButtons})
	out = append(out, promptLine{Text: truncate("  ←/→ or a/s/r to choose · Enter confirms · Esc rejects", width), Style: promptHint})
	return out
}

// resolutionEntry summarises a settled request for the scrollback, so the
// decision survives the prompt disappearing.
func resolutionEntry(ev permissions.ApprovalEvent) Entry {
	verb := "denied"
	state := ToolDenied
	if ev.Approved {
		verb = "approved"
		state = ToolOK
	}
	text := fmt.Sprintf("%s: %s", verb, actionHeadline(ev.Approval.Action))
	if ev.Approved && ev.Scope != permissions.ScopeOnce && ev.Scope != "" {
		text += " (remembered for this session)"
	}
	return Entry{Kind: EntryApproval, Tool: ev.Approval.Action.Tool, Text: text, State: state}
}

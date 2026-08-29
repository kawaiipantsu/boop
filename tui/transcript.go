package tui

import (
	"fmt"
	"strings"
	"time"
)

// EntryKind classifies a transcript entry. The kind drives both the marker in
// front of the text and the colour View paints it, which is why rendering
// returns the kind alongside each line instead of baking in escape codes.
type EntryKind string

const (
	// EntryUser is something the operator typed.
	EntryUser EntryKind = "user"
	// EntryAssistant is model prose, streamed in token by token.
	EntryAssistant EntryKind = "assistant"
	// EntryReasoning is separately-exposed model thinking.
	EntryReasoning EntryKind = "reasoning"
	// EntryTool is a tool call and its outcome.
	EntryTool EntryKind = "tool"
	// EntryOutput is stdout/stderr from an executed command.
	EntryOutput EntryKind = "output"
	// EntryError is a failure worth interrupting the reader for.
	EntryError EntryKind = "error"
	// EntrySystem is Boop talking about itself: command results, notices.
	EntrySystem EntryKind = "system"
	// EntryApproval records how a permission request was answered, so the
	// decision stays in the scrollback after the prompt disappears.
	EntryApproval EntryKind = "approval"
)

// ToolState is the lifecycle of a tool call as the transcript knows it.
type ToolState string

const (
	// ToolRunning means the call was approved and is executing.
	ToolRunning ToolState = "running"
	// ToolOK means the call returned a usable result.
	ToolOK ToolState = "ok"
	// ToolFailed means the call returned a structured error, which the model
	// will see and may repair (§2.6).
	ToolFailed ToolState = "failed"
	// ToolDenied means the user refused the action.
	ToolDenied ToolState = "denied"
)

// Markers prefixed to each kind of entry. They are ASCII-safe on purpose:
// Windows terminals in a legacy code page still render them.
const (
	markerUser      = "> "
	markerIndent    = "  "
	markerTool      = "  - "
	markerToolCont  = "    "
	markerOutput    = "  | "
	markerError     = "  ! "
	markerReasoning = "  ~ "
	markerSystem    = "  "
)

// Entry is one item in the conversation transcript.
type Entry struct {
	Kind EntryKind
	Text string
	// Tool names the tool for EntryTool entries.
	Tool string
	// State is the tool lifecycle for EntryTool entries.
	State ToolState
	// Duration is how long a completed tool call took.
	Duration time.Duration
	// Outcome is the tool's own short summary of what it produced, such as
	// "10 results" or "exit 0". Empty falls back to a bare ok/failed.
	Outcome string
	// At is when the entry was created.
	At time.Time
	// open marks a streaming assistant entry that later tokens append to.
	open bool
	// attached marks a tool entry whose output has already been placed
	// beneath it, so a second pass cannot duplicate it.
	attached bool
}

// Line is one rendered row of the transcript, still free of escape codes so
// that layout and wrapping can be asserted in tests.
type Line struct {
	Kind EntryKind
	Text string
	// Emphasis marks the headline row of an entry, which View may render
	// brighter than its continuation rows.
	Emphasis bool
}

// Transcript is the ordered record of everything the session has shown.
//
// It is an ordinary value type with no Charm dependency: the model owns one,
// events mutate it, and the view asks it for lines at the current width. That
// separation is what makes resize correctness testable (§4.2).
type Transcript struct {
	entries []Entry
	// maxEntries bounds memory on a long session. Zero means unbounded.
	maxEntries int
	// dropped counts entries evicted by the bound, so the UI can say so
	// rather than silently losing history.
	dropped int
}

// NewTranscript returns a transcript bounded to maxEntries items; pass zero
// for unbounded.
func NewTranscript(maxEntries int) *Transcript {
	return &Transcript{maxEntries: maxEntries}
}

// Len reports how many entries are retained.
func (t *Transcript) Len() int { return len(t.entries) }

// Dropped reports how many entries were evicted to stay within the bound.
func (t *Transcript) Dropped() int { return t.dropped }

// Entries returns a copy of the retained entries.
func (t *Transcript) Entries() []Entry {
	return append([]Entry(nil), t.entries...)
}

// Clear empties the transcript.
func (t *Transcript) Clear() {
	t.entries = nil
	t.dropped = 0
}

// Append adds an entry, closing any open stream first.
func (t *Transcript) Append(e Entry) {
	t.CloseStream()
	if e.At.IsZero() {
		e.At = time.Now()
	}
	e.Text = sanitize(e.Text)
	t.push(e)
}

// AppendText is Append for a plain text entry.
func (t *Transcript) AppendText(kind EntryKind, text string) {
	t.Append(Entry{Kind: kind, Text: text})
}

// AppendLines appends several text entries of the same kind in one go, which
// is how multi-line command results reach the transcript.
func (t *Transcript) AppendLines(kind EntryKind, text string) {
	if text == "" {
		return
	}
	t.Append(Entry{Kind: kind, Text: text})
}

// AppendToken streams assistant text.
//
// Tokens arrive one at a time and must coalesce into a single paragraph rather
// than one entry per token, or wrapping would be computed against fragments.
func (t *Transcript) AppendToken(text string) {
	if text == "" {
		return
	}
	if n := len(t.entries); n > 0 && t.entries[n-1].open && t.entries[n-1].Kind == EntryAssistant {
		t.entries[n-1].Text += sanitize(text)
		return
	}
	t.push(Entry{Kind: EntryAssistant, Text: sanitize(text), At: time.Now(), open: true})
}

// CloseStream ends the current streaming entry so later output starts fresh.
func (t *Transcript) CloseStream() {
	if n := len(t.entries); n > 0 && t.entries[n-1].open {
		t.entries[n-1].open = false
		t.entries[n-1].Text = strings.TrimRight(t.entries[n-1].Text, "\n")
		if strings.TrimSpace(t.entries[n-1].Text) == "" {
			t.entries = t.entries[:n-1]
		}
	}
}

// Streaming reports whether an assistant entry is still open.
func (t *Transcript) Streaming() bool {
	n := len(t.entries)
	return n > 0 && t.entries[n-1].open
}

// StartTool records a tool call that is about to run.
func (t *Transcript) StartTool(tool, summary string) {
	t.Append(Entry{Kind: EntryTool, Tool: tool, Text: summary, State: ToolRunning})
}

// FinishTool marks the most recent running call of that tool as complete.
//
// Matching is by name from the end because a turn may have several calls in
// flight conceptually, but the loop runs them in order, so the newest running
// entry for a name is always the one that just finished.
func (t *Transcript) FinishTool(tool string, state ToolState, d time.Duration) bool {
	return t.FinishToolWithOutcome(tool, state, d, "")
}

// FinishToolWithOutcome closes a running tool line and records what it
// produced, so the transcript shows the result rather than only the timing.
func (t *Transcript) FinishToolWithOutcome(tool string, state ToolState, d time.Duration, outcome string) bool {
	for i := len(t.entries) - 1; i >= 0; i-- {
		e := &t.entries[i]
		if e.Kind == EntryTool && e.State == ToolRunning && (tool == "" || e.Tool == tool) {
			e.State = state
			e.Duration = d
			e.Outcome = outcome
			return true
		}
	}
	return false
}

// maxToolOutputLines bounds how much of a tool result is inlined. The full
// text is always in the session store; the transcript is a place to notice
// what happened, not a log file.
const maxToolOutputLines = 12

// AttachToolOutput places a tool's output directly beneath the call that
// produced it, rather than at the end of the turn.
//
// The runtime only reveals tool result content when the turn completes, but
// the reader needs it next to its call to make sense of it, so the output is
// inserted in place. Calls are matched oldest-first because the loop runs them
// in order.
func (t *Transcript) AttachToolOutput(tool, content string, isError bool) bool {
	content = strings.TrimRight(sanitize(content), "\n")
	if strings.TrimSpace(content) == "" {
		return false
	}
	idx := -1
	for i := range t.entries {
		e := &t.entries[i]
		if e.Kind == EntryTool && e.Tool == tool && !e.attached {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false
	}
	t.entries[idx].attached = true

	kind := EntryOutput
	if isError {
		kind = EntryError
	}
	entry := Entry{Kind: kind, Text: clipLines(content, maxToolOutputLines), At: t.entries[idx].At}

	rest := append([]Entry(nil), t.entries[idx+1:]...)
	t.entries = append(t.entries[:idx+1], entry)
	t.entries = append(t.entries, rest...)
	return true
}

// clipLines shortens text to at most n lines, saying how much was hidden.
func clipLines(text string, n int) string {
	if n <= 0 {
		return text
	}
	lines := strings.Split(text, "\n")
	if len(lines) <= n {
		return text
	}
	hidden := len(lines) - n
	noun := "lines"
	if hidden == 1 {
		noun = "line"
	}
	return strings.Join(lines[:n], "\n") + fmt.Sprintf("\n… %d more %s", hidden, noun)
}

// push appends and enforces the entry bound.
func (t *Transcript) push(e Entry) {
	t.entries = append(t.entries, e)
	if t.maxEntries > 0 && len(t.entries) > t.maxEntries {
		over := len(t.entries) - t.maxEntries
		t.entries = append([]Entry(nil), t.entries[over:]...)
		t.dropped += over
	}
}

// Lines renders the whole transcript wrapped to width.
func (t *Transcript) Lines(width int) []Line {
	out := make([]Line, 0, len(t.entries)*2)
	if t.dropped > 0 {
		out = append(out, Line{
			Kind: EntrySystem,
			Text: fmt.Sprintf("%s… %d earlier entries dropped to bound memory", markerSystem, t.dropped),
		})
	}
	for i, e := range t.entries {
		if i > 0 && separateFrom(t.entries[i-1].Kind, e.Kind) {
			out = append(out, Line{Kind: e.Kind, Text: ""})
		}
		out = append(out, RenderEntry(e, width)...)
	}
	return out
}

// separateFrom reports whether a blank row belongs between two kinds. Runs of
// the same output kind stay tight; a change of speaker gets air.
func separateFrom(prev, next EntryKind) bool {
	if prev == next && (next == EntryOutput || next == EntryTool) {
		return false
	}
	return true
}

// RenderEntry wraps one entry into display lines.
func RenderEntry(e Entry, width int) []Line {
	kind := e.Kind
	switch kind {
	case EntryUser:
		return lines(kind, wrapBlock(e.Text, markerUser, markerIndent, width), true)
	case EntryAssistant:
		return lines(kind, wrapBlock(e.Text, markerIndent, markerIndent, width), false)
	case EntryReasoning:
		return lines(kind, wrapBlock(e.Text, markerReasoning, markerReasoning, width), false)
	case EntryOutput:
		return lines(kind, wrapBlock(e.Text, markerOutput, markerOutput, width), false)
	case EntryError:
		return lines(kind, wrapBlock(e.Text, markerError, markerIndent+markerIndent, width), true)
	case EntryTool:
		return lines(kind, wrapBlock(toolHeadline(e), markerTool, markerToolCont, width), true)
	case EntryApproval:
		return lines(kind, wrapBlock(e.Text, markerTool, markerToolCont, width), true)
	default:
		return lines(kind, wrapBlock(e.Text, markerSystem, markerSystem, width), false)
	}
}

// toolHeadline formats the one-line summary of a tool call and its outcome.
//
// The outcome carries what the call actually produced — "10 results", "exit 0",
// "42 lines" — because a bare tool name plus a duration tells a watching user
// nothing about whether it worked.
func toolHeadline(e Entry) string {
	var b strings.Builder
	b.WriteString(e.Tool)
	if e.Text != "" && e.Text != e.Tool {
		b.WriteString("  ")
		b.WriteString(e.Text)
	}
	switch e.State {
	case ToolRunning:
		b.WriteString("  [running]")
	case ToolOK:
		b.WriteString("  [" + outcomeLabel(e, "ok") + "]")
	case ToolFailed:
		b.WriteString("  [" + outcomeLabel(e, "failed") + "]")
	case ToolDenied:
		b.WriteString("  [denied]")
	}
	return b.String()
}

// outcomeLabel renders the bracketed result, preferring the tool's own summary
// over the bare verb when it supplied one.
func outcomeLabel(e Entry, verb string) string {
	if e.Outcome != "" {
		return e.Outcome + " · " + formatDuration(e.Duration)
	}
	return verb + " " + formatDuration(e.Duration)
}

// formatDuration renders a duration compactly enough for a status suffix.
func formatDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return "0ms"
	case d < time.Millisecond:
		return fmt.Sprintf("%dµs", d.Microseconds())
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
}

func lines(kind EntryKind, texts []string, emphasiseFirst bool) []Line {
	out := make([]Line, len(texts))
	for i, s := range texts {
		out[i] = Line{Kind: kind, Text: s, Emphasis: emphasiseFirst && i == 0}
	}
	return out
}

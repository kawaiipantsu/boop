package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/kawaiipantsu/boop/internal/app"
	"github.com/kawaiipantsu/boop/internal/permissions"
	"github.com/kawaiipantsu/boop/internal/provider"
)

// maxTranscriptEntries bounds transcript memory over a long session.
const maxTranscriptEntries = 4000

// maxInputHistory bounds remembered prompts.
const maxInputHistory = 500

// Stats are the per-session counters the header and /stats report (§28).
type Stats struct {
	Turns        int
	Messages     int
	ToolCalls    int
	ToolFailures int
	Denied       int
	Approvals    int
	Iterations   int
	Prompt       int
	Completion   int
	Total        int
}

// Messages passed between goroutines and the update loop.
type (
	// turnDoneMsg reports that Loop.Run returned.
	turnDoneMsg struct {
		turn *app.Turn
		err  error
	}
	// infoMsg carries the result of an asynchronous command.
	infoMsg struct {
		entries []Entry
		notice  string
	}
	// sessionSwitchedMsg replaces the active session.
	sessionSwitchedMsg struct {
		id      string
		title   string
		history []provider.Message
		entries []Entry
	}
	// tickMsg drives the elapsed-time readout.
	tickMsg time.Time
	// submitInitialMsg submits the prompt given on the command line.
	submitInitialMsg struct{}
)

// Model is the Bubble Tea model for Boop's terminal UI.
//
// It owns presentation only. Every decision with consequences — which provider
// answers, whether a command may run, what is persisted — belongs to
// internal/app and its subsystems; this type turns their events into rows on a
// screen and turns keystrokes back into calls on them (§2.3, §64.1).
type Model struct {
	app      *app.App
	approver *Approver
	ctx      context.Context
	theme    theme

	sessionID    string
	sessionTitle string
	startedAt    time.Time
	initial      string

	width, height int
	layout        Layout

	transcript *Transcript
	viewport   viewport.Model
	input      textarea.Model
	ready      bool
	follow     bool

	status  Status
	prompt  *approvalPrompt
	waiting int

	history      []provider.Message
	inputHistory []string
	histIdx      int
	histDraft    string
	search       *searchState

	turnCancel context.CancelFunc
	turnActive bool
	turns      *sync.WaitGroup

	interruptArmed bool
	quitting       bool
	mouseOn        bool

	stats  Stats
	notice string
	pump   *pump
}

// searchState is the Ctrl+R reverse history search.
type searchState struct {
	query string
	match string
	found bool
}

// newModel builds a model. The runtime may be nil, which yields a UI that
// renders and parses commands but cannot talk to a model; that is what the
// tests use and what a startup failure would fall back to.
func newModel(ctx context.Context, application *app.App, approver *Approver, sessionID, title, initial string, history []provider.Message) *Model {
	ta := textarea.New()
	ta.Placeholder = "Ask Boop something, or type /help"
	ta.Prompt = "│ "
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.MaxHeight = maxInputHeight
	ta.Focus()

	m := &Model{
		app:          application,
		approver:     approver,
		ctx:          ctx,
		theme:        newTheme(),
		sessionID:    sessionID,
		sessionTitle: title,
		startedAt:    time.Now(),
		initial:      initial,
		transcript:   NewTranscript(maxTranscriptEntries),
		viewport:     viewport.New(0, 0),
		input:        ta,
		follow:       true,
		status:       StatusIdle,
		history:      history,
		turns:        &sync.WaitGroup{},
		mouseOn:      true,
	}
	m.histIdx = -1
	m.pump = newPump(nil)
	return m
}

// Init implements tea.Model.
func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{textarea.Blink, tickEvery()}
	if strings.TrimSpace(m.initial) != "" {
		cmds = append(cmds, func() tea.Msg { return submitInitialMsg{} })
	}
	return tea.Batch(cmds...)
}

// tickEvery schedules the next clock tick.
func tickEvery() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// Update implements tea.Model.
//
// Every state change funnels through here, including everything the runtime
// reports, which is what keeps the UI consistent: there is exactly one
// goroutine mutating this struct.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return m, m.resize(msg.Width, msg.Height)

	case tickMsg:
		return m, tickEvery()

	case flushMsg:
		m.applyEvents(m.pump.drain())
		m.refresh()
		return m, nil

	case approvalMsg:
		return m, m.handleApproval(msg.event)

	case turnDoneMsg:
		return m, m.finishTurn(msg)

	case infoMsg:
		for _, e := range msg.entries {
			m.transcript.Append(e)
		}
		if msg.notice != "" {
			m.notice = msg.notice
		}
		m.refresh()
		return m, nil

	case sessionSwitchedMsg:
		m.sessionID = msg.id
		m.sessionTitle = msg.title
		m.history = msg.history
		m.stats = Stats{}
		m.transcript.Clear()
		for _, e := range msg.entries {
			m.transcript.Append(e)
		}
		m.refresh()
		return m, nil

	case submitInitialMsg:
		text := m.initial
		m.initial = ""
		return m, m.submit(text)

	case tea.KeyMsg:
		return m, m.handleKey(msg)

	case tea.MouseMsg:
		return m, m.handleMouse(msg)
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// resize recomputes the layout and resizes the child components.
func (m *Model) resize(width, height int) tea.Cmd {
	m.width, m.height = width, height
	m.relayout()
	m.ready = true
	return nil
}

// relayout recomputes the row budget and pushes it into the components.
func (m *Model) relayout() {
	inputLines := m.desiredInputHeight()
	approvalLines := 0
	if m.prompt != nil {
		approvalLines = len(m.prompt.Lines(m.contentWidth()))
	}
	m.layout = ComputeLayout(m.width, m.height, inputLines, approvalLines)

	m.viewport.Width = m.layout.ContentWidth()
	m.viewport.Height = maxInt(1, m.layout.Body)
	m.input.SetWidth(m.layout.ContentWidth())
	m.input.SetHeight(maxInt(1, m.layout.Input))
	m.refresh()
}

// contentWidth is the text width inside the frame gutters.
func (m *Model) contentWidth() int {
	if m.width <= 0 {
		return 80
	}
	return maxInt(1, m.width-2*framePadding)
}

// desiredInputHeight estimates the rows the composer needs, accounting for
// soft wrapping of long lines.
func (m *Model) desiredInputHeight() int {
	inner := maxInt(1, m.contentWidth()-displayWidth(m.input.Prompt))
	total := 0
	for _, line := range strings.Split(m.input.Value(), "\n") {
		total += maxInt(1, (displayWidth(line)+inner-1)/inner)
	}
	return InputLines(total)
}

// refresh re-renders the transcript into the viewport.
func (m *Model) refresh() {
	width := m.layout.ContentWidth()
	if width <= 0 {
		width = m.contentWidth()
	}
	lines := m.transcript.Lines(width)
	rendered := make([]string, len(lines))
	for i, l := range lines {
		style := m.theme.entryStyle(l.Kind)
		if l.Emphasis {
			style = style.Bold(true)
		}
		rendered[i] = style.Render(l.Text)
	}
	m.viewport.SetContent(strings.Join(rendered, "\n"))
	if m.follow {
		m.viewport.GotoBottom()
	}
}

// applyEvents folds runtime events into the transcript and status.
func (m *Model) applyEvents(events []uiEvent) {
	for _, ev := range events {
		switch ev.kind {
		case evToken:
			m.status = StatusThinking
			m.transcript.AppendToken(ev.text)
		case evReasoning:
			m.status = StatusPlanning
			m.transcript.Append(Entry{Kind: EntryReasoning, Text: ev.text})
		case evModelStarted:
			if m.status != StatusWaiting {
				m.status = StatusThinking
			}
		case evModelCompleted:
			m.transcript.CloseStream()
		case evToolRequested:
			m.status = statusForTool(ev.tool)
			m.transcript.StartTool(ev.tool, ev.text)
		case evToolCompleted:
			m.stats.ToolCalls++
			state := ToolOK
			if ev.isError {
				state = ToolFailed
				m.stats.ToolFailures++
			}
			m.transcript.FinishToolWithOutcome(ev.tool, state, ev.duration, ev.text)
			m.status = StatusThinking
		case evCommandOutput:
			m.transcript.Append(Entry{Kind: EntryOutput, Text: ev.text})
		case evCommandError:
			m.transcript.Append(Entry{Kind: EntryError, Text: ev.text})
		case evRuntimeError:
			m.transcript.Append(Entry{Kind: EntryError, Text: ev.text})
		case evApprovalDecided:
			m.transcript.FinishTool(ev.tool, ToolDenied, 0)
			m.status = StatusThinking
		case evAgentChanged:
			if ev.text != "" {
				m.transcript.Append(Entry{Kind: EntrySystem, Text: "agent: " + ev.text})
			}
		}
	}
}

// handleApproval reacts to a change in the approval queue.
func (m *Model) handleApproval(ev permissions.ApprovalEvent) tea.Cmd {
	switch ev.Kind {
	case permissions.ApprovalAdded:
		m.stats.Approvals++
		if m.prompt == nil {
			m.prompt = newApprovalPrompt(ev.Approval)
		}
		m.status = StatusWaiting
	case permissions.ApprovalResolved, permissions.ApprovalCancelled:
		if !ev.Approved {
			m.stats.Denied++
		}
		if ev.Kind == permissions.ApprovalResolved {
			m.transcript.Append(resolutionEntry(ev))
		}
		if m.prompt != nil && m.prompt.pending.ID == ev.Approval.ID {
			m.prompt = nil
			if m.turnActive {
				m.status = StatusWorking
			}
		}
	}
	m.syncApprovals()
	m.relayout()
	return nil
}

// syncApprovals reconciles the prompt with the broker's queue.
//
// The queue, not the event stream, is the source of truth: the broker drops
// events for a slow consumer, so every change re-reads Pending() rather than
// trying to keep a private tally in step.
func (m *Model) syncApprovals() {
	broker := m.broker()
	if broker == nil {
		m.waiting = 0
		if m.prompt != nil {
			m.waiting = 1
		}
		return
	}
	pending := broker.Pending()
	m.waiting = len(pending)
	if m.prompt == nil && len(pending) > 0 {
		m.prompt = newApprovalPrompt(pending[0])
		m.status = StatusWaiting
	}
	if m.prompt != nil {
		m.prompt.queued = maxInt(0, len(pending)-1)
	}
}

// broker returns the approval broker, or nil when running without a runtime.
func (m *Model) broker() *permissions.Broker {
	if m.approver == nil {
		return nil
	}
	return m.approver.Broker()
}

// answerApproval resolves the visible request.
func (m *Model) answerApproval(choice approvalChoice) tea.Cmd {
	if m.prompt == nil {
		return nil
	}
	id := m.prompt.pending.ID
	m.prompt = nil
	if broker := m.broker(); broker != nil {
		if err := broker.ResolveWithScope(id, choice.Approved, choice.Scope); err != nil {
			m.transcript.Append(Entry{Kind: EntryError, Text: "approval: " + err.Error()})
		}
	}
	m.syncApprovals()
	if m.prompt == nil && m.turnActive {
		m.status = StatusWorking
	}
	m.relayout()
	return nil
}

// submit sends a line of input: a slash command, or a prompt for the model.
func (m *Model) submit(text string) tea.Cmd {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	m.rememberInput(text)
	m.notice = ""

	if cmd, ok := ParseCommand(trimmed); ok {
		m.transcript.Append(Entry{Kind: EntryUser, Text: trimmed})
		return m.dispatch(cmd)
	}
	return m.startTurn(UnescapeMessage(text))
}

// rememberInput records a submitted line for history recall.
func (m *Model) rememberInput(text string) {
	if n := len(m.inputHistory); n > 0 && m.inputHistory[n-1] == text {
		m.histIdx = -1
		return
	}
	m.inputHistory = append(m.inputHistory, text)
	if len(m.inputHistory) > maxInputHistory {
		m.inputHistory = append([]string(nil), m.inputHistory[len(m.inputHistory)-maxInputHistory:]...)
	}
	m.histIdx = -1
}

// startTurn runs one user turn on its own goroutine.
//
// Loop.Run blocks for as long as the model and its tools take, so it must
// never touch the update goroutine. It runs inside a tea.Cmd; progress arrives
// through the pump and completion through turnDoneMsg.
func (m *Model) startTurn(text string) tea.Cmd {
	if m.turnActive {
		m.notice = "a turn is already running — Ctrl+C cancels it"
		return nil
	}
	m.transcript.Append(Entry{Kind: EntryUser, Text: text})
	m.stats.Turns++
	m.refresh()

	if m.app == nil {
		m.transcript.Append(Entry{Kind: EntryError, Text: "no runtime is attached, so there is nothing to ask"})
		m.refresh()
		return nil
	}

	user := provider.Message{Role: provider.RoleUser, Content: text}
	m.history = append(m.history, user)
	history := append([]provider.Message(nil), m.history...)

	ctx, cancel := context.WithCancel(m.ctx)
	m.turnCancel = cancel
	m.turnActive = true
	m.status = StatusThinking
	m.interruptArmed = false
	if m.approver != nil {
		m.approver.SetTurnContext(ctx)
	}

	sessionID := m.sessionID
	application := m.app
	wg := m.turns
	wg.Add(1)

	return func() tea.Msg {
		defer wg.Done()
		if _, err := application.Sessions.AppendMessage(ctx, sessionID, user); err != nil && ctx.Err() == nil {
			// Persistence failure is worth reporting but not worth
			// abandoning the turn over.
			application.Bus.Emit(app.EventError, sessionID, "could not record the prompt: "+err.Error())
		}
		turn, err := application.NewLoop(sessionID).Run(ctx, history)
		if turn != nil && len(turn.Messages) > 0 {
			// Use a detached context: the turn may have been cancelled, and
			// what it produced before that is still worth keeping (§2.7).
			saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer saveCancel()
			if _, saveErr := application.Sessions.AppendMessages(saveCtx, sessionID, turn.Messages...); saveErr != nil {
				application.Bus.Emit(app.EventError, sessionID, "could not record the answer: "+saveErr.Error())
			}
		}
		return turnDoneMsg{turn: turn, err: err}
	}
}

// finishTurn folds a completed turn back into the UI.
func (m *Model) finishTurn(msg turnDoneMsg) tea.Cmd {
	m.applyEvents(m.pump.drain())
	m.transcript.CloseStream()
	m.turnActive = false
	if m.turnCancel != nil {
		m.turnCancel()
		m.turnCancel = nil
	}
	if m.approver != nil {
		m.approver.SetTurnContext(nil)
	}

	// A tool that failed before the runtime could report completion would
	// otherwise sit at [running] for the rest of the session.
	for m.transcript.FinishTool("", ToolFailed, 0) {
	}

	if msg.turn != nil {
		m.attachToolOutput(msg.turn)
		m.stats.Iterations += msg.turn.Iterations
		m.stats.Prompt += msg.turn.Usage.PromptTokens
		m.stats.Completion += msg.turn.Usage.CompletionTokens
		m.stats.Total += msg.turn.Usage.TotalTokens
		m.stats.Messages += len(msg.turn.Messages)
		m.history = append(m.history, msg.turn.Messages...)
		if msg.turn.StoppedAtLimit {
			m.transcript.Append(Entry{Kind: EntryError, Text: fmt.Sprintf(
				"stopped after %d tool iterations, so the answer may be incomplete; raise execution.max_tool_iterations to allow more",
				msg.turn.Iterations)})
		}
	}

	switch {
	case msg.err != nil && m.ctx.Err() == nil && isCancellation(msg.err):
		m.transcript.Append(Entry{Kind: EntrySystem, Text: "interrupted."})
		m.status = StatusIdle
	case msg.err != nil:
		m.transcript.Append(Entry{Kind: EntryError, Text: msg.err.Error()})
		m.status = StatusError
	default:
		m.status = StatusIdle
	}
	m.follow = true
	m.relayout()
	return nil
}

// attachToolOutput places each tool result under the call it belongs to.
//
// The runtime does not stream tool result bodies — the loop only publishes
// that a call finished — so the text becomes available when the turn returns.
// Inserting it in place keeps the reading order honest.
func (m *Model) attachToolOutput(turn *app.Turn) {
	for _, msg := range turn.Messages {
		if msg.Role != provider.RoleTool {
			continue
		}
		m.transcript.AttachToolOutput(msg.Name, msg.Content, false)
	}
}

// isCancellation reports whether an error is the user's own interrupt rather
// than a fault worth painting red.
func isCancellation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), context.Canceled.Error())
}

// cancelTurn stops the work in flight without leaving the loop parked.
//
// A pending approval is rejected first: cancelling the context alone would
// unblock the approver, but an explicit rejection is what the permission
// engine should record, and it is what the model is told (§51).
func (m *Model) cancelTurn() {
	if m.prompt != nil {
		if broker := m.broker(); broker != nil {
			_ = broker.Resolve(m.prompt.pending.ID, false)
		}
		m.prompt = nil
	}
	if m.turnCancel != nil {
		m.turnCancel()
		m.turnCancel = nil
	}
}

// shutdown releases everything the runtime might be blocked on and quits.
func (m *Model) shutdown() tea.Cmd {
	m.quitting = true
	m.cancelTurn()
	// Closing the broker fails every parked approval closed rather than
	// leaving the loop goroutine waiting for an answer nobody will give.
	if broker := m.broker(); broker != nil {
		broker.Close()
	}
	return tea.Quit
}

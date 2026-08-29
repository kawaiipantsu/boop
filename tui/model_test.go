package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kawaiipantsu/boop/internal/app"
	"github.com/kawaiipantsu/boop/internal/permissions"
	"github.com/kawaiipantsu/boop/internal/provider"
)

// newTestModel returns a sized model with no runtime attached. Update is a
// pure function of (model, message), so the interesting behaviour is testable
// without a terminal or a provider.
func newTestModel(t *testing.T) *Model {
	t.Helper()
	m := newModel(context.Background(), nil, nil, "session-1", "", "", []provider.Message{
		{Role: provider.RoleSystem, Content: "you are boop"},
	})
	send(m, tea.WindowSizeMsg{Width: 80, Height: 24})
	return m
}

// send feeds a message through Update and discards the command.
func send(m *Model, msg tea.Msg) tea.Cmd {
	_, cmd := m.Update(msg)
	return cmd
}

// key builds a KeyMsg from the string form Bubble Tea reports.
func key(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "alt+enter":
		return tea.KeyMsg{Type: tea.KeyEnter, Alt: true}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	case "ctrl+s":
		return tea.KeyMsg{Type: tea.KeyCtrlS}
	case "ctrl+j":
		return tea.KeyMsg{Type: tea.KeyCtrlJ}
	case "ctrl+r":
		return tea.KeyMsg{Type: tea.KeyCtrlR}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEscape}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

// typeText feeds a string in one keystroke at a time.
func typeText(m *Model, text string) {
	for _, r := range text {
		send(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

func transcriptText(m *Model) string { return renderText(m.transcript, 78) }

func TestResizeRecomputesTheLayout(t *testing.T) {
	m := newTestModel(t)
	tests := []struct{ width, height int }{
		{80, 24}, {120, 40}, {40, 10}, {20, 5}, {200, 60}, {10, 3},
	}
	for _, tc := range tests {
		send(m, tea.WindowSizeMsg{Width: tc.width, Height: tc.height})
		if m.layout.Rows() != tc.height {
			t.Errorf("%dx%d: layout covers %d rows", tc.width, tc.height, m.layout.Rows())
		}
		if got := len(strings.Split(m.View(), "\n")); got != tc.height {
			t.Errorf("%dx%d: View rendered %d rows", tc.width, tc.height, got)
		}
	}
}

func TestViewBeforeTheFirstResize(t *testing.T) {
	m := newModel(context.Background(), nil, nil, "s", "", "", nil)
	if got := m.View(); !strings.Contains(got, "starting boop") {
		t.Fatalf("View = %q", got)
	}
}

func TestSubmitRoutesCommandsAndPrompts(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantInText string
		wantTurn   bool
	}{
		{name: "slash command", input: "/help", wantInText: "commands"},
		{name: "unknown command", input: "/nope", wantInText: "unknown command /nope"},
		{name: "typo gets a suggestion", input: "/statu", wantInText: "did you mean /status"},
		{name: "pending command explains itself", input: "/gui", wantInText: "not available yet"},
		{name: "easter egg", input: "/boop", wantInText: "boop."},
		{name: "prompt goes to the model", input: "hello there", wantInText: "> hello there", wantTurn: true},
		{name: "escaped slash is a prompt", input: "//help", wantInText: "> /help", wantTurn: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel(t)
			send(m, key("ctrl+u")) // no-op; keeps the composer clean
			typeText(m, tc.input)
			send(m, key("enter"))

			got := transcriptText(m)
			if !strings.Contains(got, tc.wantInText) {
				t.Fatalf("transcript missing %q:\n%s", tc.wantInText, got)
			}
			if tc.wantTurn && m.stats.Turns != 1 {
				t.Fatalf("turns = %d, want 1", m.stats.Turns)
			}
			if !tc.wantTurn && m.stats.Turns != 0 {
				t.Fatalf("turns = %d, want 0", m.stats.Turns)
			}
			if m.input.Value() != "" {
				t.Fatalf("composer was not cleared: %q", m.input.Value())
			}
		})
	}
}

func TestEnterInsertsANewlineOnceTheMessageIsMultiLine(t *testing.T) {
	m := newTestModel(t)
	typeText(m, "first")
	send(m, key("ctrl+j"))
	typeText(m, "second")
	if !strings.Contains(m.input.Value(), "\n") {
		t.Fatalf("Ctrl+J did not start a new line: %q", m.input.Value())
	}

	send(m, key("enter"))
	if m.input.Value() == "" {
		t.Fatal("Enter submitted a multi-line message instead of adding a line")
	}
	send(m, key("alt+enter"))
	if m.input.Value() != "" {
		t.Fatalf("Alt+Enter did not submit: %q", m.input.Value())
	}
}

func TestCtrlSSubmitsFromAnywhere(t *testing.T) {
	m := newTestModel(t)
	typeText(m, "one")
	send(m, key("ctrl+j"))
	typeText(m, "two")
	send(m, key("ctrl+s"))
	if m.input.Value() != "" {
		t.Fatalf("Ctrl+S did not submit: %q", m.input.Value())
	}
}

func TestIsSubmit(t *testing.T) {
	tests := []struct {
		name  string
		msg   tea.KeyMsg
		value string
		want  bool
	}{
		{"enter on one line", tea.KeyMsg{Type: tea.KeyEnter}, "hello", true},
		{"enter on several lines", tea.KeyMsg{Type: tea.KeyEnter}, "a\nb", false},
		{"alt enter on several lines", tea.KeyMsg{Type: tea.KeyEnter, Alt: true}, "a\nb", true},
		{"ctrl+s", tea.KeyMsg{Type: tea.KeyCtrlS}, "a\nb", true},
		{"a letter", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}, "", false},
	}
	for _, tc := range tests {
		if got := isSubmit(tc.msg, tc.value); got != tc.want {
			t.Errorf("%s: isSubmit = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestInputHistoryRecall(t *testing.T) {
	m := newTestModel(t)
	for _, line := range []string{"first", "second"} {
		typeText(m, line)
		send(m, key("enter"))
	}
	typeText(m, "draft")

	send(m, key("up"))
	if m.input.Value() != "second" {
		t.Fatalf("first Up = %q, want %q", m.input.Value(), "second")
	}
	send(m, key("up"))
	if m.input.Value() != "first" {
		t.Fatalf("second Up = %q, want %q", m.input.Value(), "first")
	}
	send(m, key("up"))
	if m.input.Value() != "first" {
		t.Fatalf("Up past the oldest entry = %q", m.input.Value())
	}
	send(m, key("down"))
	send(m, key("down"))
	if m.input.Value() != "draft" {
		t.Fatalf("returning to the draft = %q, want %q", m.input.Value(), "draft")
	}
}

func TestInputHistoryIgnoresConsecutiveDuplicates(t *testing.T) {
	m := newTestModel(t)
	for i := 0; i < 3; i++ {
		typeText(m, "same")
		send(m, key("enter"))
	}
	if len(m.inputHistory) != 1 {
		t.Fatalf("history = %q, want one entry", m.inputHistory)
	}
}

func TestReverseHistorySearch(t *testing.T) {
	m := newTestModel(t)
	for _, line := range []string{"run the tests", "read the file", "run the build"} {
		typeText(m, line)
		send(m, key("enter"))
	}

	send(m, key("ctrl+r"))
	if m.search == nil {
		t.Fatal("Ctrl+R did not open the search")
	}
	typeText(m, "run")
	if !m.search.found || m.search.match != "run the build" {
		t.Fatalf("search found %q (%v), want the newest match", m.search.match, m.search.found)
	}

	send(m, key("ctrl+r"))
	if m.search.match != "run the tests" {
		t.Fatalf("repeating Ctrl+R = %q, want the older match", m.search.match)
	}

	send(m, key("enter"))
	if m.search != nil {
		t.Fatal("Enter did not close the search")
	}
	if m.input.Value() != "run the tests" {
		t.Fatalf("accepted value = %q", m.input.Value())
	}
}

func TestReverseHistorySearchEscapeKeepsTheDraft(t *testing.T) {
	m := newTestModel(t)
	typeText(m, "old prompt")
	send(m, key("enter"))
	typeText(m, "draft")
	send(m, key("ctrl+r"))
	typeText(m, "old")
	send(m, key("esc"))
	if m.search != nil {
		t.Fatal("Esc did not close the search")
	}
	if m.input.Value() != "draft" {
		t.Fatalf("draft = %q", m.input.Value())
	}
}

func TestSearchBackspace(t *testing.T) {
	m := newTestModel(t)
	typeText(m, "alpha")
	send(m, key("enter"))
	send(m, key("ctrl+r"))
	typeText(m, "alz")
	if m.search.found {
		t.Fatalf("unexpected match %q", m.search.match)
	}
	send(m, key("backspace"))
	if !m.search.found || m.search.match != "alpha" {
		t.Fatalf("after backspace: %q (%v)", m.search.match, m.search.found)
	}
}

func TestCtrlCArmsThenQuitsWhenIdle(t *testing.T) {
	m := newTestModel(t)
	send(m, key("ctrl+c"))
	if !m.interruptArmed {
		t.Fatal("the first Ctrl+C did not arm the quit")
	}
	if m.quitting {
		t.Fatal("the first Ctrl+C quit immediately")
	}
	send(m, key("ctrl+c"))
	if !m.quitting {
		t.Fatal("the second Ctrl+C did not quit")
	}
}

func TestAnyOtherKeyDisarmsTheQuit(t *testing.T) {
	m := newTestModel(t)
	send(m, key("ctrl+c"))
	typeText(m, "x")
	if m.interruptArmed {
		t.Fatal("typing did not disarm the quit")
	}
	send(m, key("ctrl+c"))
	if m.quitting {
		t.Fatal("Ctrl+C quit without being re-armed")
	}
}

func TestCtrlCCancelsTheTurnFirst(t *testing.T) {
	m := newTestModel(t)
	cancelled := false
	m.turnActive = true
	m.turnCancel = func() { cancelled = true }

	send(m, key("ctrl+c"))
	if !cancelled {
		t.Fatal("the turn was not cancelled")
	}
	if m.quitting {
		t.Fatal("the first Ctrl+C quit instead of cancelling")
	}
	if m.status != StatusIdle {
		t.Fatalf("status = %q, want IDLE", m.status)
	}
}

func TestEscapeClearsTheComposerThenFollowsTheTranscript(t *testing.T) {
	m := newTestModel(t)
	typeText(m, "unwanted")
	send(m, key("esc"))
	if m.input.Value() != "" {
		t.Fatalf("Esc did not clear the composer: %q", m.input.Value())
	}
	m.follow = false
	send(m, key("esc"))
	if !m.follow {
		t.Fatal("Esc on an empty composer did not resume following")
	}
}

func TestClearAndResetCommands(t *testing.T) {
	m := newTestModel(t)
	m.history = append(m.history, provider.Message{Role: provider.RoleUser, Content: "hi"})
	m.transcript.AppendText(EntryAssistant, "hello")
	m.stats.Turns = 3

	send(m, key("/"))
	typeText(m, "clear")
	send(m, key("enter"))
	if strings.Contains(transcriptText(m), "hello") {
		t.Fatal("/clear left the transcript behind")
	}
	if len(m.history) != 2 {
		t.Fatalf("/clear discarded conversation history: %d messages", len(m.history))
	}

	typeText(m, "/reset")
	send(m, key("enter"))
	if len(m.history) != 1 || m.history[0].Role != provider.RoleSystem {
		t.Fatalf("/reset left history = %+v", m.history)
	}
	if m.stats.Turns != 0 {
		t.Fatalf("/reset left stats = %+v", m.stats)
	}
}

func TestQuitCommandShutsDown(t *testing.T) {
	for _, name := range []string{"/quit", "/exit"} {
		m := newTestModel(t)
		typeText(m, name)
		send(m, key("enter"))
		if !m.quitting {
			t.Fatalf("%s did not quit", name)
		}
	}
}

func TestTokenStreamUpdatesTheTranscriptAndStatus(t *testing.T) {
	m := newTestModel(t)
	m.pump.push(uiEvent{kind: evToken, text: "Hello "})
	m.pump.push(uiEvent{kind: evToken, text: "world"})
	send(m, flushMsg{})

	if m.status != StatusThinking {
		t.Fatalf("status = %q, want THINKING", m.status)
	}
	if got := transcriptText(m); !strings.Contains(got, "Hello world") {
		t.Fatalf("transcript = %q", got)
	}
}

func TestToolEventsDriveTheTranscript(t *testing.T) {
	m := newTestModel(t)
	m.pump.push(uiEvent{kind: evToolRequested, tool: "run", text: "go test ./..."})
	send(m, flushMsg{})
	if m.status != StatusRunning {
		t.Fatalf("status = %q, want RUNNING", m.status)
	}
	if !strings.Contains(transcriptText(m), "[running]") {
		t.Fatalf("transcript = %s", transcriptText(m))
	}

	m.pump.push(uiEvent{kind: evToolCompleted, tool: "run", isError: true, duration: 2 * time.Second})
	send(m, flushMsg{})
	if m.stats.ToolCalls != 1 || m.stats.ToolFailures != 1 {
		t.Fatalf("stats = %+v", m.stats)
	}
	if !strings.Contains(transcriptText(m), "[failed 2.0s]") {
		t.Fatalf("transcript = %s", transcriptText(m))
	}
}

func TestDeniedToolClosesItsLine(t *testing.T) {
	m := newTestModel(t)
	m.pump.push(uiEvent{kind: evToolRequested, tool: "run", text: "rm -rf /"})
	m.pump.push(uiEvent{kind: evApprovalDecided, tool: "run"})
	send(m, flushMsg{})
	if !strings.Contains(transcriptText(m), "[denied]") {
		t.Fatalf("transcript = %s", transcriptText(m))
	}
}

func TestStatusForTool(t *testing.T) {
	tests := []struct {
		tool string
		want Status
	}{
		{"run", StatusRunning},
		{"git", StatusRunning},
		{"test", StatusTesting},
		{"read", StatusWorking},
		{"", StatusWorking},
	}
	for _, tc := range tests {
		if got := statusForTool(tc.tool); got != tc.want {
			t.Errorf("statusForTool(%q) = %q, want %q", tc.tool, got, tc.want)
		}
	}
	if StatusIdle.Busy() || StatusError.Busy() {
		t.Error("IDLE and ERROR are not busy states")
	}
	if !StatusRunning.Busy() || !StatusWaiting.Busy() {
		t.Error("RUNNING and WAITING are busy states")
	}
}

func TestFinishTurnFoldsUsageAndClosesOrphanedToolLines(t *testing.T) {
	m := newTestModel(t)
	m.turnActive = true
	m.transcript.StartTool("run", "sleep 30")

	send(m, turnDoneMsg{turn: &app.Turn{
		Messages: []provider.Message{
			{Role: provider.RoleAssistant, Content: "done"},
			{Role: provider.RoleTool, Name: "run", Content: "output line"},
		},
		Usage:      provider.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		Iterations: 2,
	}})

	if m.turnActive {
		t.Fatal("the turn is still marked active")
	}
	if m.status != StatusIdle {
		t.Fatalf("status = %q, want IDLE", m.status)
	}
	if m.stats.Total != 15 || m.stats.Prompt != 10 || m.stats.Completion != 5 {
		t.Fatalf("usage = %+v", m.stats)
	}
	if len(m.history) != 3 {
		t.Fatalf("history = %d messages, want 3", len(m.history))
	}
	got := transcriptText(m)
	if !strings.Contains(got, "[failed") {
		t.Fatalf("an orphaned tool line was left running:\n%s", got)
	}
	if !strings.Contains(got, "output line") {
		t.Fatalf("tool output was not attached:\n%s", got)
	}
}

func TestFinishTurnReportsErrorsAndInterrupts(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus Status
		wantText   string
	}{
		{"failure", errors.New("provider unreachable"), StatusError, "provider unreachable"},
		{"interrupt", context.Canceled, StatusIdle, "interrupted."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel(t)
			m.turnActive = true
			send(m, turnDoneMsg{err: tc.err})
			if m.status != tc.wantStatus {
				t.Errorf("status = %q, want %q", m.status, tc.wantStatus)
			}
			if !strings.Contains(transcriptText(m), tc.wantText) {
				t.Errorf("transcript missing %q:\n%s", tc.wantText, transcriptText(m))
			}
		})
	}
}

func TestFinishTurnWarnsAboutTheIterationLimit(t *testing.T) {
	m := newTestModel(t)
	m.turnActive = true
	send(m, turnDoneMsg{turn: &app.Turn{Iterations: 50, StoppedAtLimit: true}})
	if !strings.Contains(transcriptText(m), "max_tool_iterations") {
		t.Fatalf("transcript = %s", transcriptText(m))
	}
}

func TestApprovalPromptTakesTheKeyboard(t *testing.T) {
	m := newTestModel(t)
	pending := permissions.PendingApproval{
		ID: "p1",
		Action: permissions.Action{Tool: "run", Category: permissions.CatShellExecute,
			Risk: permissions.RiskLow, Summary: "run a command", Detail: "ls"},
	}
	send(m, approvalMsg{event: permissions.ApprovalEvent{Kind: permissions.ApprovalAdded, Approval: pending}})

	if m.prompt == nil || m.status != StatusWaiting {
		t.Fatalf("prompt = %v, status = %q", m.prompt, m.status)
	}
	if m.layout.Approval == 0 {
		t.Fatal("the layout gave the approval no rows")
	}
	if !strings.Contains(m.View(), "APPROVAL REQUIRED") {
		t.Fatal("the approval is not visible in the frame")
	}

	// Typing must not leak into the composer while the prompt is up.
	typeText(m, "x")
	if m.input.Value() != "" {
		t.Fatalf("keystrokes leaked into the composer: %q", m.input.Value())
	}

	send(m, key("right"))
	if m.prompt.cursor != 1 {
		t.Fatalf("cursor = %d", m.prompt.cursor)
	}
	send(m, key("esc"))
	if m.prompt != nil {
		t.Fatal("Esc did not dismiss the prompt")
	}
}

func TestApprovalResolutionIsRecorded(t *testing.T) {
	m := newTestModel(t)
	pending := permissions.PendingApproval{ID: "p1",
		Action: permissions.Action{Tool: "run", Detail: "ls", Risk: permissions.RiskLow}}
	send(m, approvalMsg{event: permissions.ApprovalEvent{Kind: permissions.ApprovalAdded, Approval: pending}})
	send(m, approvalMsg{event: permissions.ApprovalEvent{
		Kind: permissions.ApprovalResolved, Approval: pending, Approved: true, Scope: permissions.ScopeOnce}})

	if m.prompt != nil {
		t.Fatal("the prompt outlived its resolution")
	}
	if !strings.Contains(transcriptText(m), "approved: ls") {
		t.Fatalf("transcript = %s", transcriptText(m))
	}
	if m.stats.Approvals != 1 {
		t.Fatalf("approvals = %d", m.stats.Approvals)
	}
}

func TestApprovalAnsweredThroughTheBroker(t *testing.T) {
	broker := permissions.NewBroker()
	approver := NewApprover(broker)
	m := newModel(context.Background(), nil, approver, "s", "", "", nil)
	send(m, tea.WindowSizeMsg{Width: 80, Height: 24})

	result := answer(approver, permissions.Action{Tool: "run", Category: permissions.CatShellExecute,
		Risk: permissions.RiskLow, Summary: "run a command", Detail: "ls"})
	pending := waitPending(t, broker)
	send(m, approvalMsg{event: permissions.ApprovalEvent{Kind: permissions.ApprovalAdded, Approval: pending}})

	send(m, key("a"))
	select {
	case got := <-result:
		if !got.ok || got.err != nil {
			t.Fatalf("Approve = (%v, %v)", got.ok, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pressing a never reached the loop")
	}
	if m.prompt != nil {
		t.Fatal("the prompt is still up")
	}
}

func TestApprovalRejectedByEscapeReachesTheLoop(t *testing.T) {
	broker := permissions.NewBroker()
	approver := NewApprover(broker)
	m := newModel(context.Background(), nil, approver, "s", "", "", nil)
	send(m, tea.WindowSizeMsg{Width: 80, Height: 24})

	result := answer(approver, permissions.Action{Tool: "run", Risk: permissions.RiskLow})
	pending := waitPending(t, broker)
	send(m, approvalMsg{event: permissions.ApprovalEvent{Kind: permissions.ApprovalAdded, Approval: pending}})
	send(m, key("esc"))

	select {
	case got := <-result:
		if got.ok {
			t.Fatal("Esc must be a denial, not consent")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Esc never reached the loop")
	}
}

func TestQuitWithAPendingApprovalDoesNotDeadlock(t *testing.T) {
	// §51 and §58: leaving while the loop is parked on an approval must
	// release it rather than hang shutdown.
	broker := permissions.NewBroker()
	approver := NewApprover(broker)
	m := newModel(context.Background(), nil, approver, "s", "", "", nil)
	send(m, tea.WindowSizeMsg{Width: 80, Height: 24})

	result := answer(approver, permissions.Action{Tool: "run", Risk: permissions.RiskLow})
	pending := waitPending(t, broker)
	send(m, approvalMsg{event: permissions.ApprovalEvent{Kind: permissions.ApprovalAdded, Approval: pending}})

	// The prompt owns the keyboard, so quitting here means the shutdown path
	// itself, not a keystroke that would be read as an answer.
	if cmd := m.shutdown(); cmd == nil {
		t.Fatal("shutdown returned no quit command")
	}

	select {
	case got := <-result:
		if got.ok {
			t.Fatal("shutting down must not read as consent")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the parked approval was never released")
	}
	if !m.quitting {
		t.Fatal("the model is not quitting")
	}
}

func TestApprovalQueueDepthComesFromTheBroker(t *testing.T) {
	broker := permissions.NewBroker()
	approver := NewApprover(broker)
	m := newModel(context.Background(), nil, approver, "s", "", "", nil)
	send(m, tea.WindowSizeMsg{Width: 80, Height: 24})

	first := answer(approver, permissions.Action{Tool: "run", Detail: "one", Risk: permissions.RiskLow})
	waitPending(t, broker)
	second := answer(approver, permissions.Action{Tool: "run", Detail: "two", Risk: permissions.RiskLow})
	waitForPendingCount(t, broker, 2)

	for _, p := range broker.Pending() {
		send(m, approvalMsg{event: permissions.ApprovalEvent{Kind: permissions.ApprovalAdded, Approval: p}})
	}
	if m.waiting != 2 {
		t.Fatalf("waiting = %d, want 2", m.waiting)
	}
	if m.prompt.queued != 1 {
		t.Fatalf("queued = %d, want 1", m.prompt.queued)
	}

	send(m, key("r"))
	<-first
	if m.prompt == nil {
		t.Fatal("the next queued approval was not promoted")
	}
	if m.prompt.pending.Action.Detail != "two" {
		t.Fatalf("promoted %q", m.prompt.pending.Action.Detail)
	}
	send(m, key("r"))
	<-second
}

func TestMouseWheelScrollsAndStopsFollowing(t *testing.T) {
	m := newTestModel(t)
	for i := 0; i < 200; i++ {
		m.transcript.AppendText(EntrySystem, "line")
	}
	m.refresh()

	send(m, tea.MouseMsg{Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress})
	if m.follow {
		t.Fatal("scrolling up should stop the transcript following the tail")
	}
	for i := 0; i < 200; i++ {
		send(m, tea.MouseMsg{Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
	}
	if !m.follow {
		t.Fatal("scrolling back to the bottom should resume following")
	}
}

func TestMouseClickAnswersAnApproval(t *testing.T) {
	broker := permissions.NewBroker()
	approver := NewApprover(broker)
	m := newModel(context.Background(), nil, approver, "s", "", "", nil)
	send(m, tea.WindowSizeMsg{Width: 100, Height: 30})

	result := answer(approver, permissions.Action{Tool: "run", Category: permissions.CatShellExecute,
		Risk: permissions.RiskLow, Summary: "run a command", Detail: "ls"})
	pending := waitPending(t, broker)
	send(m, approvalMsg{event: permissions.ApprovalEvent{Kind: permissions.ApprovalAdded, Approval: pending}})

	row, ok := m.buttonRowIndex()
	if !ok {
		t.Fatal("no button row is visible")
	}
	_, spans := buttonRow(m.prompt.choices, m.prompt.cursor)
	send(m, tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
		X: framePadding + spans[0].Start + 1, Y: row,
	})

	select {
	case got := <-result:
		if !got.ok {
			t.Fatalf("clicking Approve gave %v", got.ok)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the click never reached the loop")
	}
}

func TestMouseToggle(t *testing.T) {
	m := newTestModel(t)
	if !m.mouseOn {
		t.Fatal("mouse capture should start on")
	}
	send(m, tea.KeyMsg{Type: tea.KeyCtrlO})
	if m.mouseOn {
		t.Fatal("Ctrl+O did not turn mouse capture off")
	}
	send(m, tea.KeyMsg{Type: tea.KeyCtrlO})
	if !m.mouseOn {
		t.Fatal("Ctrl+O did not turn mouse capture back on")
	}
}

func TestStartTurnWithoutARuntimeSaysSo(t *testing.T) {
	m := newTestModel(t)
	typeText(m, "hello")
	send(m, key("enter"))
	if !strings.Contains(transcriptText(m), "no runtime is attached") {
		t.Fatalf("transcript = %s", transcriptText(m))
	}
}

func TestSecondTurnIsRefusedWhileOneIsRunning(t *testing.T) {
	m := newTestModel(t)
	m.turnActive = true
	typeText(m, "hello")
	send(m, key("enter"))
	if !strings.Contains(m.notice, "already running") {
		t.Fatalf("notice = %q", m.notice)
	}
	if m.stats.Turns != 0 {
		t.Fatalf("turns = %d, want 0", m.stats.Turns)
	}
}

func TestCommandsThatNeedARuntimeFailPolitely(t *testing.T) {
	for _, name := range []string{"/provider", "/model", "/models", "/permissions", "/session"} {
		m := newTestModel(t)
		typeText(m, name)
		send(m, key("enter"))
		if !strings.Contains(transcriptText(m), "no runtime is attached") {
			t.Errorf("%s did not explain itself:\n%s", name, transcriptText(m))
		}
	}
}

func TestLocalReportsRenderWithoutARuntime(t *testing.T) {
	m := newTestModel(t)
	for _, tc := range []struct{ cmd, want string }{
		{"/status", "uptime"},
		{"/stats", "session statistics"},
		{"/context", "conversation context"},
		{"/tokens", "this session (live)"},
	} {
		typeText(m, tc.cmd)
		send(m, key("enter"))
		if !strings.Contains(transcriptText(m), tc.want) {
			t.Errorf("%s missing %q", tc.cmd, tc.want)
		}
	}
}

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{{"", 0}, {"a", 1}, {"abcd", 1}, {"abcde", 2}}
	for _, tc := range tests {
		if got := estimateTokens(tc.in); got != tc.want {
			t.Errorf("estimateTokens(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestCompactCount(t *testing.T) {
	tests := []struct {
		in   int
		want string
	}{{0, "0"}, {999, "999"}, {1500, "1.5k"}, {2_500_000, "2.5M"}}
	for _, tc := range tests {
		if got := compactCount(tc.in); got != tc.want {
			t.Errorf("compactCount(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestShortID(t *testing.T) {
	if got := shortID("abcdefghijkl"); got != "abcdefgh" {
		t.Errorf("shortID = %q", got)
	}
	if got := shortID("abc"); got != "abc" {
		t.Errorf("shortID = %q", got)
	}
}

func TestHeaderShowsTheThingsSection19Requires(t *testing.T) {
	m := newTestModel(t)
	plain := m.headerPlain()
	for _, want := range []string{"no provider", "mode", "IDLE", "agents", "tok", "web off"} {
		if !strings.Contains(plain, want) {
			t.Errorf("header is missing %q: %s", want, plain)
		}
	}
}

func TestSessionSwitchReplacesEverything(t *testing.T) {
	m := newTestModel(t)
	m.transcript.AppendText(EntryAssistant, "old")
	m.stats.Turns = 4
	send(m, sessionSwitchedMsg{
		id: "new-session", title: "fresh",
		history: []provider.Message{{Role: provider.RoleSystem, Content: "sys"}},
		entries: []Entry{{Kind: EntrySystem, Text: "resumed"}},
	})
	if m.sessionID != "new-session" || m.sessionTitle != "fresh" {
		t.Fatalf("session = %q/%q", m.sessionID, m.sessionTitle)
	}
	if m.stats.Turns != 0 {
		t.Fatalf("stats survived the switch: %+v", m.stats)
	}
	got := transcriptText(m)
	if strings.Contains(got, "old") || !strings.Contains(got, "resumed") {
		t.Fatalf("transcript = %s", got)
	}
}

func TestInfoMsgAppendsEntries(t *testing.T) {
	m := newTestModel(t)
	send(m, infoMsg{entries: []Entry{{Kind: EntryError, Text: "could not reach the provider"}}, notice: "heads up"})
	if !strings.Contains(transcriptText(m), "could not reach the provider") {
		t.Fatalf("transcript = %s", transcriptText(m))
	}
	if m.notice != "heads up" {
		t.Fatalf("notice = %q", m.notice)
	}
}

func TestEntriesFromMessages(t *testing.T) {
	entries := entriesFromMessages([]provider.Message{
		{Role: provider.RoleSystem, Content: "ignored"},
		{Role: provider.RoleUser, Content: "hi"},
		{Role: provider.RoleAssistant, Content: "hello", ToolCalls: []provider.ToolCall{{Name: "run", Arguments: `{"cmd":"ls"}`}}},
		{Role: provider.RoleTool, Name: "run", Content: "a\nb"},
		{Role: provider.RoleAssistant, Content: "   "},
	})
	kinds := make([]EntryKind, len(entries))
	for i, e := range entries {
		kinds[i] = e.Kind
	}
	want := []EntryKind{EntryUser, EntryAssistant, EntryTool, EntryOutput}
	if len(kinds) != len(want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", kinds, want)
		}
	}
}

func TestIsCancellation(t *testing.T) {
	if !isCancellation(context.Canceled) {
		t.Error("context.Canceled is a cancellation")
	}
	if isCancellation(errors.New("provider down")) {
		t.Error("an ordinary error is not a cancellation")
	}
	if isCancellation(nil) {
		t.Error("nil is not a cancellation")
	}
}

func TestIsLoopbackURL(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"http://127.0.0.1:11434/v1", true},
		{"http://localhost:1234", true},
		{"http://[::1]:8080", true},
		{"https://api.example.com", false},
		{"://bad", false},
	}
	for _, tc := range tests {
		if got := isLoopbackURL(tc.in); got != tc.want {
			t.Errorf("isLoopbackURL(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func waitForPendingCount(t *testing.T, broker *permissions.Broker, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(broker.Pending()) >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d pending approvals", n)
}

func TestFrameIsAlwaysExactlyTheTerminalHeight(t *testing.T) {
	// Resize correctness is the one thing a terminal UI cannot get wrong, so
	// it is asserted across the awkward sizes as well as the comfortable ones.
	sizes := []struct{ width, height int }{
		{80, 24}, {132, 43}, {60, 12}, {40, 8}, {30, 6}, {24, 4}, {20, 3}, {15, 2}, {10, 1},
	}
	states := []struct {
		name  string
		setup func(*Model)
	}{
		{name: "empty", setup: func(*Model) {}},
		{
			name: "busy with a long transcript",
			setup: func(m *Model) {
				for i := 0; i < 120; i++ {
					m.transcript.AppendText(EntryOutput, "a line of command output that is long enough to wrap on narrow terminals")
				}
				m.turnActive = true
				m.status = StatusRunning
			},
		},
		{
			name: "multi-line composer",
			setup: func(m *Model) {
				m.input.SetValue("one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine")
			},
		},
		{
			name: "approval pending",
			setup: func(m *Model) {
				m.prompt = newApprovalPrompt(permissions.PendingApproval{
					ID: "p1",
					Action: permissions.Action{
						Tool: "run", Category: permissions.CatProductionChange, Risk: permissions.RiskCritical,
						Summary: "deploy the application to production",
						Detail:  "kubectl apply -f prod/deployment.yaml --namespace production",
						Paths:   []string{"prod/deployment.yaml"}, Production: true,
					},
					Decision: permissions.Decision{Reason: "production changes always need a fresh decision"},
				})
			},
		},
	}

	for _, state := range states {
		for _, size := range sizes {
			m := newTestModel(t)
			state.setup(m)
			send(m, tea.WindowSizeMsg{Width: size.width, Height: size.height})

			frame := m.View()
			rows := strings.Split(frame, "\n")
			if len(rows) != size.height {
				t.Errorf("%s at %dx%d: frame is %d rows, want %d",
					state.name, size.width, size.height, len(rows), size.height)
			}
			for i, row := range rows {
				if w := displayWidth(sanitize(row)); w > size.width {
					t.Errorf("%s at %dx%d: row %d is %d cells wide: %q",
						state.name, size.width, size.height, i, w, sanitize(row))
				}
			}
		}
	}
}

func TestApprovalStaysVisibleOnAShortTerminal(t *testing.T) {
	m := newTestModel(t)
	send(m, tea.WindowSizeMsg{Width: 70, Height: 10})
	send(m, approvalMsg{event: permissions.ApprovalEvent{
		Kind: permissions.ApprovalAdded,
		Approval: permissions.PendingApproval{ID: "p1", Action: permissions.Action{
			Tool: "run", Risk: permissions.RiskHigh, Summary: "push to the remote", Detail: "git push origin main",
		}},
	}})
	if m.layout.Approval == 0 {
		t.Fatal("the approval was squeezed out of the layout")
	}
	if m.layout.Approval > m.layout.Height/2 {
		t.Fatalf("the approval claimed %d of %d rows", m.layout.Approval, m.layout.Height)
	}
	if !strings.Contains(sanitize(m.View()), "APPROVAL REQUIRED") {
		t.Fatalf("the approval is not on screen:\n%s", m.View())
	}
}

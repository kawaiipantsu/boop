package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/permissions"
	"github.com/kawaiipantsu/boop/internal/provider"
	"github.com/kawaiipantsu/boop/internal/tools"
)

// scriptedProvider replays a fixed sequence of turns, so the loop can be
// driven deterministically without a model or a network.
type scriptedProvider struct {
	turns []([]provider.ChatEvent)
	calls int
}

func (s *scriptedProvider) Name() string                 { return "scripted" }
func (s *scriptedProvider) Health(context.Context) error { return nil }
func (s *scriptedProvider) ListModels(context.Context) ([]provider.Model, error) {
	return []provider.Model{{ID: "test", Provider: "scripted"}}, nil
}
func (s *scriptedProvider) Capabilities(context.Context, string) (provider.Capabilities, error) {
	return provider.Capabilities{provider.CapabilityStreaming, provider.CapabilityTools}, nil
}
func (s *scriptedProvider) Chat(ctx context.Context, _ provider.ChatRequest) (<-chan provider.ChatEvent, error) {
	if s.calls >= len(s.turns) {
		return nil, errors.New("scripted provider exhausted")
	}
	events := s.turns[s.calls]
	s.calls++
	ch := make(chan provider.ChatEvent, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

func textTurn(text string) []provider.ChatEvent {
	return []provider.ChatEvent{
		{Type: provider.EventDelta, Text: text},
		{Type: provider.EventUsage, Usage: &provider.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}},
		{Type: provider.EventDone, Finish: provider.FinishStop},
	}
}

func toolTurn(id, name, args string) []provider.ChatEvent {
	return []provider.ChatEvent{
		{Type: provider.EventToolCall, ToolCall: &provider.ToolCall{ID: id, Name: name, Arguments: args}},
		{Type: provider.EventDone, Finish: provider.FinishToolCalls},
	}
}

// recordingTool captures what it was asked to do and returns a scripted result.
type recordingTool struct {
	name     string
	action   permissions.Action
	result   tools.Result
	invoked  int
	lastArgs string
}

func (r *recordingTool) Name() string           { return r.name }
func (r *recordingTool) Description() string    { return "test tool" }
func (r *recordingTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (r *recordingTool) Permission(tools.Call) (permissions.Action, error) {
	return r.action, nil
}
func (r *recordingTool) Execute(_ context.Context, call tools.Call) (tools.Result, error) {
	r.invoked++
	r.lastArgs = string(call.Arguments)
	res := r.result
	res.CallID, res.Tool = call.ID, call.Name
	return res, nil
}

func newLoop(t *testing.T, p provider.Provider, reg *tools.Registry, pol permissions.Policy, ap permissions.Approver) *Loop {
	t.Helper()
	registry := provider.NewRegistry()
	if err := registry.Register(p); err != nil {
		t.Fatal(err)
	}
	router := provider.NewRouter(registry, provider.RouterConfig{
		Classes: map[provider.RouteClass]provider.Target{
			provider.ClassDefault: {Provider: p.Name(), Model: "test"},
		},
	})
	return &Loop{
		Router: router, Tools: reg, Bus: NewBus(),
		Evaluator: permissions.NewEvaluator(pol), Approver: ap,
		MaxIterations: 10,
		Selection:     provider.Selection{Provider: p.Name(), Model: "test"},
	}
}

func allowAll() permissions.Policy {
	rules := map[permissions.Category]permissions.Rule{}
	for cat := range permissions.DefaultRules() {
		rules[cat] = permissions.RuleAllow
	}
	return permissions.NewPolicy(permissions.ModeAuto, rules)
}

func TestLoopReturnsPlainAnswerWithoutTools(t *testing.T) {
	p := &scriptedProvider{turns: [][]provider.ChatEvent{textTurn("42")}}
	loop := newLoop(t, p, tools.NewRegistry(), allowAll(), nil)

	turn, err := loop.Run(context.Background(), []provider.Message{{Role: provider.RoleUser, Content: "q"}})
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if turn.Text != "42" {
		t.Errorf("Text = %q, want %q", turn.Text, "42")
	}
	if turn.Iterations != 1 {
		t.Errorf("Iterations = %d, want 1", turn.Iterations)
	}
	if turn.Usage.TotalTokens != 15 {
		t.Errorf("TotalTokens = %d, want 15", turn.Usage.TotalTokens)
	}
}

func TestLoopExecutesToolAndFeedsResultBack(t *testing.T) {
	tool := &recordingTool{
		name:   "probe",
		action: permissions.Action{Category: permissions.CatFilesystemRead, Risk: permissions.RiskLow, Summary: "probe"},
		result: tools.Result{Content: "the answer is 7"},
	}
	reg := tools.NewRegistry()
	reg.Register(tool)

	p := &scriptedProvider{turns: [][]provider.ChatEvent{
		toolTurn("c1", "probe", `{"x":1}`),
		textTurn("7"),
	}}
	loop := newLoop(t, p, reg, allowAll(), nil)

	turn, err := loop.Run(context.Background(), []provider.Message{{Role: provider.RoleUser, Content: "q"}})
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if tool.invoked != 1 {
		t.Fatalf("tool invoked %d times, want 1", tool.invoked)
	}
	if tool.lastArgs != `{"x":1}` {
		t.Errorf("arguments = %q, want %q", tool.lastArgs, `{"x":1}`)
	}
	if turn.Text != "7" {
		t.Errorf("Text = %q, want %q", turn.Text, "7")
	}
	// The tool result must reach the model as a tool message keyed by call id.
	var toolMsg *provider.Message
	for i := range turn.Messages {
		if turn.Messages[i].Role == provider.RoleTool {
			toolMsg = &turn.Messages[i]
		}
	}
	if toolMsg == nil {
		t.Fatal("no tool message was produced")
	}
	if toolMsg.ToolCallID != "c1" {
		t.Errorf("ToolCallID = %q, want c1", toolMsg.ToolCallID)
	}
	if !strings.Contains(toolMsg.Content, "the answer is 7") {
		t.Errorf("tool content = %q, want the tool's output", toolMsg.Content)
	}
}

// A failing tool must not abort the turn. The failure is returned to the model
// so it can diagnose and repair (§2.6, §13).
func TestLoopFeedsToolFailureBackForRepair(t *testing.T) {
	tool := &recordingTool{
		name:   "probe",
		action: permissions.Action{Category: permissions.CatShellExecute, Risk: permissions.RiskLow, Summary: "probe"},
		result: tools.Result{Content: "exit_code: 1\nno such file", IsError: true},
	}
	reg := tools.NewRegistry()
	reg.Register(tool)

	p := &scriptedProvider{turns: [][]provider.ChatEvent{
		toolTurn("c1", "probe", `{}`),
		textTurn("I see the file is missing; creating it instead."),
	}}
	loop := newLoop(t, p, reg, allowAll(), nil)

	turn, err := loop.Run(context.Background(), []provider.Message{{Role: provider.RoleUser, Content: "q"}})
	if err != nil {
		t.Fatalf("Run() = %v, want the failure handled as data", err)
	}
	if turn.Iterations != 2 {
		t.Errorf("Iterations = %d, want 2 (the model should get a chance to repair)", turn.Iterations)
	}
	if !strings.Contains(turn.Text, "missing") {
		t.Errorf("Text = %q, want the model's follow-up", turn.Text)
	}
}

type denyingApprover struct{ asked int }

func (d *denyingApprover) Approve(permissions.Action) (bool, error) { d.asked++; return false, nil }

func TestLoopDoesNotRunADeclinedTool(t *testing.T) {
	tool := &recordingTool{
		name:   "danger",
		action: permissions.Action{Category: permissions.CatShellExecute, Risk: permissions.RiskHigh, Summary: "rm -rf /"},
		result: tools.Result{Content: "should never run"},
	}
	reg := tools.NewRegistry()
	reg.Register(tool)

	approver := &denyingApprover{}
	p := &scriptedProvider{turns: [][]provider.ChatEvent{
		toolTurn("c1", "danger", `{}`),
		textTurn("understood, I will not do that"),
	}}
	loop := newLoop(t, p, reg, permissions.NewPolicy(permissions.ModeConfirm, nil), approver)

	turn, err := loop.Run(context.Background(), []provider.Message{{Role: provider.RoleUser, Content: "q"}})
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if approver.asked != 1 {
		t.Errorf("approver asked %d times, want 1", approver.asked)
	}
	if tool.invoked != 0 {
		t.Error("a declined tool was executed anyway")
	}
	// The model must be told, and told not to simply retry.
	var toolContent string
	for _, m := range turn.Messages {
		if m.Role == provider.RoleTool {
			toolContent = m.Content
		}
	}
	if !strings.Contains(toolContent, "declined") {
		t.Errorf("tool message = %q, want it to report the refusal", toolContent)
	}
}

// Without an approver attached, an action needing confirmation must fail rather
// than proceed unapproved.
func TestLoopRefusesConfirmationWithoutAnApprover(t *testing.T) {
	tool := &recordingTool{
		name:   "danger",
		action: permissions.Action{Category: permissions.CatShellExecute, Risk: permissions.RiskHigh, Summary: "danger"},
	}
	reg := tools.NewRegistry()
	reg.Register(tool)

	p := &scriptedProvider{turns: [][]provider.ChatEvent{
		toolTurn("c1", "danger", `{}`), textTurn("ok"),
	}}
	loop := newLoop(t, p, reg, permissions.NewPolicy(permissions.ModeConfirm, nil), nil)

	if _, err := loop.Run(context.Background(), []provider.Message{{Role: provider.RoleUser, Content: "q"}}); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if tool.invoked != 0 {
		t.Error("a tool needing confirmation ran with no approver attached")
	}
}

func TestLoopReportsUnknownTool(t *testing.T) {
	p := &scriptedProvider{turns: [][]provider.ChatEvent{
		toolTurn("c1", "nope", `{}`), textTurn("sorry"),
	}}
	loop := newLoop(t, p, tools.NewRegistry(), allowAll(), nil)

	turn, err := loop.Run(context.Background(), []provider.Message{{Role: provider.RoleUser, Content: "q"}})
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	var content string
	for _, m := range turn.Messages {
		if m.Role == provider.RoleTool {
			content = m.Content
		}
	}
	if !strings.Contains(content, "no tool named") {
		t.Errorf("content = %q, want it to name the problem", content)
	}
}

// A model that keeps calling tools must be stopped, not allowed to spin.
func TestLoopStopsAtTheIterationLimit(t *testing.T) {
	tool := &recordingTool{
		name:   "loopy",
		action: permissions.Action{Category: permissions.CatFilesystemRead, Risk: permissions.RiskLow},
		result: tools.Result{Content: "again"},
	}
	reg := tools.NewRegistry()
	reg.Register(tool)

	turns := make([][]provider.ChatEvent, 20)
	for i := range turns {
		turns[i] = toolTurn("c", "loopy", `{}`)
	}
	p := &scriptedProvider{turns: turns}
	loop := newLoop(t, p, reg, allowAll(), nil)
	loop.MaxIterations = 4

	turn, err := loop.Run(context.Background(), []provider.Message{{Role: provider.RoleUser, Content: "q"}})
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if !turn.StoppedAtLimit {
		t.Error("StoppedAtLimit = false, want true")
	}
	if turn.Iterations != 4 {
		t.Errorf("Iterations = %d, want 4", turn.Iterations)
	}
	if tool.invoked != 4 {
		t.Errorf("tool invoked %d times, want 4", tool.invoked)
	}
}

func TestLoopPropagatesStreamErrors(t *testing.T) {
	p := &scriptedProvider{turns: [][]provider.ChatEvent{{
		{Type: provider.EventError, Err: provider.NewError(provider.ErrServer, "scripted", "boom", nil)},
	}}}
	loop := newLoop(t, p, tools.NewRegistry(), allowAll(), nil)

	_, err := loop.Run(context.Background(), []provider.Message{{Role: provider.RoleUser, Content: "q"}})
	if err == nil {
		t.Fatal("Run() = nil, want the stream error")
	}
	if category, ok := provider.CategoryOf(err); !ok || category != provider.ErrServer {
		t.Errorf("category = %v, want %v", category, provider.ErrServer)
	}
}

func TestLoopEmitsToolEvents(t *testing.T) {
	tool := &recordingTool{
		name:   "probe",
		action: permissions.Action{Category: permissions.CatFilesystemRead, Risk: permissions.RiskLow, Summary: "probe"},
		result: tools.Result{Content: "ok"},
	}
	reg := tools.NewRegistry()
	reg.Register(tool)
	p := &scriptedProvider{turns: [][]provider.ChatEvent{toolTurn("c1", "probe", `{}`), textTurn("done")}}
	loop := newLoop(t, p, reg, allowAll(), nil)

	seen := map[EventType]int{}
	loop.Bus.Subscribe(func(ev Event) { seen[ev.Type]++ })

	if _, err := loop.Run(context.Background(), []provider.Message{{Role: provider.RoleUser, Content: "q"}}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []EventType{EventModelRequestStarted, EventToolRequested, EventToolCompleted, EventModelCompleted} {
		if seen[want] == 0 {
			t.Errorf("no %s event was published", want)
		}
	}
}

func TestLoopHonoursContextCancellation(t *testing.T) {
	p := &scriptedProvider{turns: [][]provider.ChatEvent{textTurn("hi")}}
	loop := newLoop(t, p, tools.NewRegistry(), allowAll(), nil)

	// Cancel up front rather than racing a short timeout against execution:
	// a deadline that may or may not have elapsed makes the test flaky, which
	// is worse than not having it.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := loop.Run(ctx, []provider.Message{{Role: provider.RoleUser, Content: "q"}}); err == nil {
		t.Error("Run() = nil, want a cancellation error")
	}
}

// A cancellation part-way through a stream must stop the turn rather than
// finishing it, so an interrupted user does not pay for a full answer (§51).
func TestLoopStopsWhenCancelledMidStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := &cancellingProvider{cancel: cancel}
	loop := newLoop(t, p, tools.NewRegistry(), allowAll(), nil)

	_, err := loop.Run(ctx, []provider.Message{{Role: provider.RoleUser, Content: "q"}})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Run() = %v, want context.Canceled", err)
	}
}

// cancellingProvider cancels the context after emitting its first delta, so
// the loop is interrupted deterministically part-way through a stream.
type cancellingProvider struct {
	scriptedProvider
	cancel context.CancelFunc
}

func (c *cancellingProvider) Chat(ctx context.Context, _ provider.ChatRequest) (<-chan provider.ChatEvent, error) {
	ch := make(chan provider.ChatEvent, 3)
	ch <- provider.ChatEvent{Type: provider.EventDelta, Text: "partial"}
	c.cancel()
	ch <- provider.ChatEvent{Type: provider.EventDelta, Text: " more"}
	ch <- provider.ChatEvent{Type: provider.EventDone, Finish: provider.FinishStop}
	close(ch)
	return ch, nil
}

func TestLoopRequiresItsCollaborators(t *testing.T) {
	if _, err := (&Loop{Tools: tools.NewRegistry()}).Run(context.Background(), nil); err == nil {
		t.Error("a missing router should be rejected")
	}
	if _, err := (&Loop{Router: &provider.Router{}}).Run(context.Background(), nil); err == nil {
		t.Error("a missing tool registry should be rejected")
	}
}

var _ = json.Marshal

// emptyStreamProvider closes its channel without emitting anything, which is
// what a cancelled or broken stream can look like from the loop's side.
type emptyStreamProvider struct{ scriptedProvider }

func (*emptyStreamProvider) Chat(context.Context, provider.ChatRequest) (<-chan provider.ChatEvent, error) {
	ch := make(chan provider.ChatEvent)
	close(ch)
	return ch, nil
}

// A stream that yields no events must be an error, not an empty answer.
//
// The check for cancellation used to live only inside the event loop, so a
// stream with nothing in it never reached it and the turn returned success
// with empty text — the model appearing to answer with silence.
func TestLoopRejectsAnEmptyStream(t *testing.T) {
	loop := newLoop(t, &emptyStreamProvider{}, tools.NewRegistry(), allowAll(), nil)

	turn, err := loop.Run(context.Background(), []provider.Message{{Role: provider.RoleUser, Content: "q"}})
	if err == nil {
		t.Fatalf("Run() = nil with text %q, want an error", turn.Text)
	}
	if category, ok := provider.CategoryOf(err); !ok || category != provider.ErrMalformedResponse {
		t.Errorf("category = %v, want %v", category, provider.ErrMalformedResponse)
	}
}

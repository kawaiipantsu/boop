package agent

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/kawaiipantsu/boop/internal/app"
	"github.com/kawaiipantsu/boop/internal/permissions"
	"github.com/kawaiipantsu/boop/internal/provider"
	"github.com/kawaiipantsu/boop/internal/tools"
)

// scriptedProvider replays fixed turns so a worker's loop runs deterministically
// with no model and no network.
type scriptedProvider struct {
	mu    sync.Mutex
	turns [][]provider.ChatEvent
	seen  []provider.ChatRequest
}

func (s *scriptedProvider) Name() string                 { return "scripted" }
func (s *scriptedProvider) Health(context.Context) error { return nil }

func (s *scriptedProvider) ListModels(context.Context) ([]provider.Model, error) {
	return []provider.Model{{ID: "test", Provider: "scripted"}}, nil
}

func (s *scriptedProvider) Capabilities(context.Context, string) (provider.Capabilities, error) {
	return provider.Capabilities{provider.CapabilityStreaming, provider.CapabilityTools}, nil
}

func (s *scriptedProvider) Chat(_ context.Context, req provider.ChatRequest) (<-chan provider.ChatEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, req)
	if len(s.turns) == 0 {
		return nil, errors.New("scripted provider exhausted")
	}
	events := s.turns[0]
	s.turns = s.turns[1:]
	ch := make(chan provider.ChatEvent, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

func (s *scriptedProvider) requests() []provider.ChatRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]provider.ChatRequest(nil), s.seen...)
}

func textTurn(text string) []provider.ChatEvent {
	return []provider.ChatEvent{
		{Type: provider.EventDelta, Text: text},
		{Type: provider.EventUsage, Usage: &provider.Usage{PromptTokens: 4, CompletionTokens: 2, TotalTokens: 6}},
		{Type: provider.EventDone, Finish: provider.FinishStop},
	}
}

func toolTurn(id, name, args string) []provider.ChatEvent {
	return []provider.ChatEvent{
		{Type: provider.EventToolCall, ToolCall: &provider.ToolCall{ID: id, Name: name, Arguments: args}},
		{Type: provider.EventDone, Finish: provider.FinishToolCalls},
	}
}

// fakeTool records whether it was ever executed.
type fakeTool struct {
	name string
	mu   sync.Mutex
	runs int
}

func (f *fakeTool) Name() string           { return f.name }
func (f *fakeTool) Description() string    { return "fake " + f.name }
func (f *fakeTool) Schema() map[string]any { return map[string]any{"type": "object"} }

func (f *fakeTool) Permission(tools.Call) (permissions.Action, error) {
	return permissions.Action{
		Category: permissions.CatFilesystemRead,
		Risk:     permissions.RiskLow,
		Summary:  "fake " + f.name,
	}, nil
}

func (f *fakeTool) Execute(_ context.Context, call tools.Call) (tools.Result, error) {
	f.mu.Lock()
	f.runs++
	f.mu.Unlock()
	return tools.Result{CallID: call.ID, Tool: call.Name, Content: f.name + " ran"}, nil
}

func (f *fakeTool) executions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs
}

func fullRegistry(names ...string) (*tools.Registry, map[string]*fakeTool) {
	reg := tools.NewRegistry()
	made := make(map[string]*fakeTool, len(names))
	for _, name := range names {
		ft := &fakeTool{name: name}
		made[name] = ft
		reg.Register(ft)
	}
	return reg, made
}

func allowAllPolicy() permissions.Policy {
	rules := map[permissions.Category]permissions.Rule{}
	for cat := range permissions.DefaultRules() {
		rules[cat] = permissions.RuleAllow
	}
	return permissions.NewPolicy(permissions.ModeAuto, rules)
}

// newLoopFactory builds a factory over a scripted provider, which is all a
// worker needs to run a real app.Loop in a test.
func newLoopFactory(t *testing.T, p *scriptedProvider, reg *tools.Registry, bus *app.Bus) LoopFactory {
	t.Helper()
	providers := provider.NewRegistry()
	if err := providers.Register(p); err != nil {
		t.Fatalf("registering the scripted provider: %v", err)
	}
	router := provider.NewRouter(providers, provider.RouterConfig{
		Classes: map[provider.RouteClass]provider.Target{
			provider.ClassDefault: {Provider: p.Name(), Model: "test"},
		},
	})
	return func(sessionID string) *app.Loop {
		return &app.Loop{
			Router:        router,
			Tools:         reg,
			Evaluator:     permissions.NewEvaluator(allowAllPolicy()),
			Bus:           bus,
			MaxIterations: 5,
			SessionID:     sessionID,
			Selection:     provider.Selection{Provider: p.Name(), Model: "test"},
		}
	}
}

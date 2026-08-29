package fixtures_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kawaiipantsu/boop/internal/provider"
	"github.com/kawaiipantsu/boop/tests/fixtures"
)

// drain collects a chat stream into a slice, failing on an unclosed channel.
func drain(t *testing.T, ch <-chan provider.ChatEvent) []provider.ChatEvent {
	t.Helper()
	var events []provider.ChatEvent
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return events
			}
			events = append(events, ev)
		case <-timeout:
			t.Fatal("timed out draining chat events: the provider never closed its channel")
		}
	}
}

// types extracts the event types for compact assertions.
func types(events []provider.ChatEvent) []provider.EventType {
	out := make([]provider.EventType, len(events))
	for i, ev := range events {
		out[i] = ev.Type
	}
	return out
}

func equalTypes(got []provider.EventType, want ...provider.EventType) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestFakeProviderStreamsScriptedTurn(t *testing.T) {
	turn := fixtures.TextTurn("")
	turn.TextChunks = []string{"Hel", "lo"}
	turn.Reasoning = "hmm"
	turn.Usage = &provider.Usage{PromptTokens: 4, CompletionTokens: 2, TotalTokens: 6}
	f := fixtures.NewFakeProvider("fake", turn)

	ch, err := f.Chat(context.Background(), provider.ChatRequest{Model: "boop-test-model"})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	events := drain(t, ch)
	if !equalTypes(types(events),
		provider.EventReasoning, provider.EventDelta, provider.EventDelta,
		provider.EventUsage, provider.EventDone) {
		t.Fatalf("event types = %v", types(events))
	}
	if events[1].Text+events[2].Text != "Hello" {
		t.Errorf("text = %q%q", events[1].Text, events[2].Text)
	}
	if events[4].Finish != provider.FinishStop {
		t.Errorf("finish = %q", events[4].Finish)
	}
	if f.TurnsRemaining() != 0 {
		t.Errorf("turns remaining = %d", f.TurnsRemaining())
	}
}

func TestFakeProviderToolCallTurn(t *testing.T) {
	f := fixtures.NewFakeProvider("fake",
		fixtures.ToolTurn(provider.ToolCall{ID: "call_1", Name: "run", Arguments: `{"command":"go test"}`}),
	)
	events := drain(t, mustChat(t, f, provider.ChatRequest{Model: "m"}))
	if !equalTypes(types(events), provider.EventToolCall, provider.EventDone) {
		t.Fatalf("event types = %v", types(events))
	}
	tc := events[0].ToolCall
	if tc == nil || tc.ID != "call_1" || tc.Arguments != `{"command":"go test"}` {
		t.Fatalf("tool call = %+v", tc)
	}
	if events[1].Finish != provider.FinishToolCalls {
		t.Errorf("finish = %q", events[1].Finish)
	}
}

func TestFakeProviderErrorTurn(t *testing.T) {
	want := provider.NewError(provider.ErrRateLimited, "fake", "slow down", nil)
	f := fixtures.NewFakeProvider("fake", fixtures.ErrorTurn(want))

	events := drain(t, mustChat(t, f, provider.ChatRequest{}))
	if !equalTypes(types(events), provider.EventError) {
		t.Fatalf("event types = %v", types(events))
	}
	if !errors.Is(events[0].Err, want) {
		t.Fatalf("err = %v", events[0].Err)
	}
	if !provider.IsRetryable(events[0].Err) {
		t.Error("a rate-limit error should be retryable")
	}
}

func TestFakeProviderExhaustedScriptFailsLoudly(t *testing.T) {
	f := fixtures.NewFakeProvider("fake", fixtures.TextTurn("only one"))
	drain(t, mustChat(t, f, provider.ChatRequest{}))

	_, err := f.Chat(context.Background(), provider.ChatRequest{})
	if err == nil {
		t.Fatal("a second call should fail: the script had one turn")
	}
	if cat, ok := provider.CategoryOf(err); !ok || cat != provider.ErrInvalidRequest {
		t.Errorf("category = %q ok=%v", cat, ok)
	}

	f.RepeatLastTurn(true)
	events := drain(t, mustChat(t, f, provider.ChatRequest{}))
	if len(events) == 0 || events[0].Text != "only one" {
		t.Errorf("RepeatLastTurn did not replay the final turn: %+v", events)
	}
}

func TestFakeProviderResponderOverridesScript(t *testing.T) {
	f := fixtures.NewFakeProvider("fake", fixtures.TextTurn("scripted"))
	f.SetResponder(func(req provider.ChatRequest) (fixtures.Turn, bool) {
		if req.Model == "special" {
			return fixtures.TextTurn("dynamic"), true
		}
		return fixtures.Turn{}, false
	})

	events := drain(t, mustChat(t, f, provider.ChatRequest{Model: "special"}))
	if events[0].Text != "dynamic" {
		t.Fatalf("text = %q", events[0].Text)
	}
	if f.TurnsRemaining() != 1 {
		t.Errorf("responder consumed a scripted turn: %d remaining", f.TurnsRemaining())
	}

	events = drain(t, mustChat(t, f, provider.ChatRequest{Model: "other"}))
	if events[0].Text != "scripted" {
		t.Fatalf("text = %q", events[0].Text)
	}
}

func TestFakeProviderCancellation(t *testing.T) {
	turn := fixtures.TextTurn("")
	turn.TextChunks = []string{"a", "b", "c", "d"}
	turn.EventDelay = 25 * time.Millisecond
	f := fixtures.NewFakeProvider("fake", turn)

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := f.Chat(ctx, provider.ChatRequest{})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	<-ch // let the first delta through
	cancel()

	events := drain(t, ch)
	if len(events) == 0 {
		t.Fatal("cancellation must still terminate the stream with an event")
	}
	last := events[len(events)-1]
	if last.Type != provider.EventError {
		t.Fatalf("last event = %+v, want an error", last)
	}
	if cat, _ := provider.CategoryOf(last.Err); cat != provider.ErrCancelled {
		t.Errorf("category = %q, want cancelled", cat)
	}
	if last.Finish != provider.FinishCancelled {
		t.Errorf("finish = %q", last.Finish)
	}
}

func TestFakeProviderChatErrorAndHealth(t *testing.T) {
	f := fixtures.NewFakeProvider("fake")
	boom := provider.NewError(provider.ErrUnavailable, "fake", "connection refused", nil)
	f.SetHealthError(boom)
	if err := f.Health(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("health = %v", err)
	}
	f.SetChatError(boom)
	if _, err := f.Chat(context.Background(), provider.ChatRequest{}); !errors.Is(err, boom) {
		t.Fatalf("chat = %v", err)
	}
}

func TestFakeProviderModelsAndCapabilities(t *testing.T) {
	f := fixtures.NewFakeProvider("fake")
	models, err := f.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].Provider != "fake" {
		t.Fatalf("models = %+v", models)
	}

	caps, err := f.Capabilities(context.Background(), "boop-test-vision")
	if err != nil {
		t.Fatal(err)
	}
	if !caps.HasAll(provider.CapabilityStreaming, provider.CapabilityTools, provider.CapabilityVision) {
		t.Errorf("vision model capabilities = %v", caps)
	}
	caps, err = f.Capabilities(context.Background(), "boop-test-model")
	if err != nil {
		t.Fatal(err)
	}
	if caps.Has(provider.CapabilityVision) {
		t.Errorf("text model should not claim vision: %v", caps)
	}

	f.SetCapabilities("pinned", provider.Capabilities{provider.CapabilityReasoning})
	caps, _ = f.Capabilities(context.Background(), "pinned")
	if len(caps) != 1 || !caps.Has(provider.CapabilityReasoning) {
		t.Errorf("pinned capabilities = %v", caps)
	}
}

func TestFakeProviderRecordsRequests(t *testing.T) {
	f := fixtures.NewFakeProvider("fake", fixtures.TextTurn("a"), fixtures.TextTurn("b"))
	drain(t, mustChat(t, f, provider.ChatRequest{Model: "m1"}))
	drain(t, mustChat(t, f, provider.ChatRequest{
		Model: "m2",
		Messages: []provider.Message{
			{Role: provider.RoleTool, ToolCallID: "call_1", Content: "exit status 1"},
		},
	}))

	reqs := f.Requests()
	if len(reqs) != 2 || reqs[0].Model != "m1" || reqs[1].Model != "m2" {
		t.Fatalf("requests = %+v", reqs)
	}
	last, ok := f.LastRequest()
	if !ok || len(last.Messages) != 1 || last.Messages[0].ToolCallID != "call_1" {
		t.Fatalf("last request = %+v", last)
	}

	f.Reset()
	if f.Requests() != nil || f.TurnsRemaining() != 2 {
		t.Errorf("Reset should clear captures and rewind: %d turns remain", f.TurnsRemaining())
	}
}

func TestRepairScriptShape(t *testing.T) {
	script := fixtures.RepairScript("run", `{"command":"go buld"}`, `{"command":"go build"}`, "fixed the typo")
	if len(script) != 3 {
		t.Fatalf("script = %d turns, want 3", len(script))
	}
	if len(script[0].ToolCalls) != 1 || script[0].ToolCalls[0].Arguments != `{"command":"go buld"}` {
		t.Errorf("first turn = %+v", script[0])
	}
	if len(script[1].ToolCalls) != 1 || script[1].ToolCalls[0].Arguments != `{"command":"go build"}` {
		t.Errorf("repair turn = %+v", script[1])
	}
	if script[1].Text == "" {
		t.Error("the repair turn should explain itself")
	}
	if script[2].Text != "fixed the typo" || len(script[2].ToolCalls) != 0 {
		t.Errorf("final turn = %+v", script[2])
	}
}

// mustChat starts a chat or fails the test.
func mustChat(t *testing.T, f *fixtures.FakeProvider, req provider.ChatRequest) <-chan provider.ChatEvent {
	t.Helper()
	ch, err := f.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	return ch
}

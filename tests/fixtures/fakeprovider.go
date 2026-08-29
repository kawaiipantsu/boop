package fixtures

import (
	"context"
	"sync"
	"time"

	"github.com/boop-dev/boop/internal/provider"
)

// Turn is one scripted assistant turn of a [FakeProvider] exchange.
//
// A turn maps onto exactly one Chat call: the events it describes are emitted
// in order — reasoning, then text, then tool calls, then usage, then done.
type Turn struct {
	// Text is the assistant's answer, emitted as EventDelta.
	Text string
	// TextChunks, when set, replaces Text and controls delta boundaries.
	TextChunks []string
	// Reasoning is emitted as EventReasoning before any answer text.
	Reasoning string
	// ReasoningChunks plays the role of TextChunks for Reasoning.
	ReasoningChunks []string
	// ToolCalls are emitted as fully assembled EventToolCall events, because
	// reassembly is the adapter's job and has already happened by the time an
	// agent sees a Provider.
	ToolCalls []provider.ToolCall
	// Usage, when set, is emitted as EventUsage before EventDone.
	Usage *provider.Usage
	// Finish overrides the finish reason on EventDone.
	Finish provider.FinishReason
	// Err, when set, makes the turn terminate with EventError instead of
	// EventDone, for driving retry and error-normalization paths.
	Err error
	// Delay stalls before the first event of the turn.
	Delay time.Duration
	// EventDelay stalls before every event, which is what gives a test a
	// window in which to cancel the context mid-stream.
	EventDelay time.Duration
}

// TextTurn scripts a plain assistant answer.
func TextTurn(text string) Turn { return Turn{Text: text} }

// ToolTurn scripts an assistant turn that calls tools.
func ToolTurn(calls ...provider.ToolCall) Turn {
	return Turn{ToolCalls: calls, Finish: provider.FinishToolCalls}
}

// ErrorTurn scripts a turn that fails mid-stream with err.
func ErrorTurn(err error) Turn { return Turn{Err: err} }

// RepairScript builds the canonical error-repair exchange of §13:
//
//	tool call → (caller executes it and it fails) → diagnosis and repaired
//	tool call → (caller executes it and it succeeds) → closing summary
//
// The caller drives the loop, appending the tool results to the conversation;
// the script only decides what the model says next, which is what makes the
// loop deterministic and re-runnable.
func RepairScript(toolName, brokenArgs, repairedArgs, summary string) []Turn {
	return []Turn{
		ToolTurn(provider.ToolCall{ID: "call_1", Name: toolName, Arguments: brokenArgs}),
		func() Turn {
			t := ToolTurn(provider.ToolCall{ID: "call_2", Name: toolName, Arguments: repairedArgs})
			t.Text = "That failed; adjusting and retrying."
			return t
		}(),
		TextTurn(summary),
	}
}

// FakeProvider is a deterministic provider.Provider with no transport at all.
//
// It replays scripted [Turn] values, one per Chat call, and records every
// request. Use it for end-to-end and agent-loop tests, where the wire format
// is irrelevant and reproducibility is everything (§41); use [Server] when the
// thing under test is an adapter's HTTP and parsing behaviour.
//
// It is safe for concurrent use.
type FakeProvider struct {
	mu         sync.Mutex
	name       string
	turns      []Turn
	cursor     int
	requests   []provider.ChatRequest
	models     []provider.Model
	caps       map[string]provider.Capabilities
	defaults   provider.Capabilities
	healthErr  error
	chatErr    error
	responder  func(req provider.ChatRequest) (Turn, bool)
	repeatLast bool
	buffer     int
}

// compile-time proof that the fake honours the contract.
var _ provider.Provider = (*FakeProvider)(nil)

// NewFakeProvider returns a provider that will replay the given turns.
func NewFakeProvider(name string, turns ...Turn) *FakeProvider {
	f := &FakeProvider{
		name:     orDefault(name, "fake"),
		turns:    turns,
		caps:     map[string]provider.Capabilities{},
		defaults: provider.Capabilities{provider.CapabilityStreaming, provider.CapabilityTools},
		buffer:   32,
	}
	for _, m := range DefaultModels() {
		f.models = append(f.models, provider.Model{
			ID:            m.ID,
			Provider:      f.name,
			DisplayName:   m.DisplayName,
			ContextWindow: m.ContextWindow,
			MaxOutput:     m.MaxOutput,
			Capabilities:  capabilitiesFromNames(m.Capabilities),
		})
	}
	return f
}

// capabilitiesFromNames maps vendor capability strings onto the neutral set,
// ignoring names Boop has no capability for (such as Ollama's "completion").
func capabilitiesFromNames(names []string) provider.Capabilities {
	caps := provider.Capabilities{provider.CapabilityStreaming}
	for _, n := range names {
		switch n {
		case "tools":
			caps = caps.Add(provider.CapabilityTools)
		case "vision":
			caps = caps.Add(provider.CapabilityVision)
		case "thinking", "reasoning":
			caps = caps.Add(provider.CapabilityReasoning)
		case "embedding", "embeddings":
			caps = caps.Add(provider.CapabilityEmbeddings)
		}
	}
	return caps.Add()
}

// Script appends further turns to the queue.
func (f *FakeProvider) Script(turns ...Turn) *FakeProvider {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.turns = append(f.turns, turns...)
	return f
}

// SetModels replaces the catalogue returned by ListModels.
func (f *FakeProvider) SetModels(models ...provider.Model) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.models = models
}

// SetCapabilities pins the capabilities reported for one model.
func (f *FakeProvider) SetCapabilities(model string, caps provider.Capabilities) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.caps[model] = caps
}

// SetDefaultCapabilities sets what Capabilities reports for unknown models.
func (f *FakeProvider) SetDefaultCapabilities(caps provider.Capabilities) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.defaults = caps
}

// SetHealthError makes Health fail, for testing provider fallback.
func (f *FakeProvider) SetHealthError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.healthErr = err
}

// SetChatError makes Chat fail before returning a channel at all, which is
// distinct from a turn that fails mid-stream.
func (f *FakeProvider) SetChatError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chatErr = err
}

// SetResponder installs a function consulted before the scripted queue. When
// it reports ok, its turn is used and the queue is left untouched — the hook
// for request-dependent behaviour a static script cannot express.
func (f *FakeProvider) SetResponder(fn func(req provider.ChatRequest) (Turn, bool)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responder = fn
}

// RepeatLastTurn makes an exhausted script replay its final turn forever
// instead of failing. Off by default: an unscripted call is usually a bug in
// the test, and failing loudly beats looping forever.
func (f *FakeProvider) RepeatLastTurn(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repeatLast = on
}

// Requests returns a copy of every ChatRequest received, in order.
func (f *FakeProvider) Requests() []provider.ChatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]provider.ChatRequest(nil), f.requests...)
}

// LastRequest returns the most recent ChatRequest, ok=false if there is none.
func (f *FakeProvider) LastRequest() (provider.ChatRequest, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return provider.ChatRequest{}, false
	}
	return f.requests[len(f.requests)-1], true
}

// TurnsRemaining reports how many scripted turns are unconsumed. Asserting it
// is zero is how a test proves the whole exchange actually ran.
func (f *FakeProvider) TurnsRemaining() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.turns) - f.cursor
}

// Reset clears the recorded requests and rewinds the script.
func (f *FakeProvider) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cursor = 0
	f.requests = nil
}

// Name identifies the provider.
func (f *FakeProvider) Name() string { return f.name }

// Health reports the scripted health error, nil by default.
func (f *FakeProvider) Health(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return provider.NewError(provider.ErrCancelled, f.name, "cancelled", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.healthErr
}

// ListModels returns the configured catalogue.
func (f *FakeProvider) ListModels(ctx context.Context) ([]provider.Model, error) {
	if err := ctx.Err(); err != nil {
		return nil, provider.NewError(provider.ErrCancelled, f.name, "cancelled", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]provider.Model(nil), f.models...), nil
}

// Capabilities reports the pinned capabilities for model, or the defaults.
func (f *FakeProvider) Capabilities(ctx context.Context, model string) (provider.Capabilities, error) {
	if err := ctx.Err(); err != nil {
		return nil, provider.NewError(provider.ErrCancelled, f.name, "cancelled", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if caps, ok := f.caps[model]; ok {
		return caps, nil
	}
	for _, m := range f.models {
		if m.ID == model && len(m.Capabilities) > 0 {
			return m.Capabilities, nil
		}
	}
	return f.defaults, nil
}

// Chat records the request and streams the next scripted turn.
//
// The returned channel is closed after exactly one terminating event, either
// EventDone or EventError, as the Provider contract requires. Cancelling ctx
// terminates the stream with a normalized ErrCancelled error.
func (f *FakeProvider) Chat(ctx context.Context, req provider.ChatRequest) (<-chan provider.ChatEvent, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	chatErr, responder := f.chatErr, f.responder
	f.mu.Unlock()

	if chatErr != nil {
		return nil, chatErr
	}

	var (
		turn Turn
		ok   bool
	)
	if responder != nil {
		turn, ok = responder(req)
	}
	if !ok {
		turn, ok = f.nextTurn()
	}
	if !ok {
		return nil, provider.NewError(provider.ErrInvalidRequest, f.name,
			"no scripted turns remain: the fake provider was called more times than the test scripted", nil)
	}

	ch := make(chan provider.ChatEvent, f.buffer)
	go f.play(ctx, turn, ch)
	return ch, nil
}

// nextTurn pops the next scripted turn.
func (f *FakeProvider) nextTurn() (Turn, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cursor < len(f.turns) {
		t := f.turns[f.cursor]
		f.cursor++
		return t, true
	}
	if f.repeatLast && len(f.turns) > 0 {
		return f.turns[len(f.turns)-1], true
	}
	return Turn{}, false
}

// play emits a turn's events and closes ch.
func (f *FakeProvider) play(ctx context.Context, turn Turn, ch chan<- provider.ChatEvent) {
	defer close(ch)

	send := func(ev provider.ChatEvent) bool {
		if turn.EventDelay > 0 {
			select {
			case <-time.After(turn.EventDelay):
			case <-ctx.Done():
				return false
			}
		}
		ev.At = FixedTime
		select {
		case ch <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}
	cancelled := func() {
		// Best effort: the buffered channel means a still-draining consumer
		// observes the cancellation, while an abandoned one cannot block us.
		select {
		case ch <- provider.ChatEvent{
			Type:   provider.EventError,
			Finish: provider.FinishCancelled,
			Err:    provider.NewError(provider.ErrCancelled, f.name, "request cancelled", ctx.Err()),
			At:     FixedTime,
		}:
		default:
		}
	}

	if turn.Delay > 0 {
		select {
		case <-time.After(turn.Delay):
		case <-ctx.Done():
			cancelled()
			return
		}
	}

	for _, chunk := range chunksOf(turn.Reasoning, turn.ReasoningChunks) {
		if !send(provider.ChatEvent{Type: provider.EventReasoning, Text: chunk}) {
			cancelled()
			return
		}
	}
	for _, chunk := range chunksOf(turn.Text, turn.TextChunks) {
		if !send(provider.ChatEvent{Type: provider.EventDelta, Text: chunk}) {
			cancelled()
			return
		}
	}
	for i := range turn.ToolCalls {
		tc := turn.ToolCalls[i]
		if !send(provider.ChatEvent{Type: provider.EventToolCall, ToolCall: &tc}) {
			cancelled()
			return
		}
	}
	if turn.Usage != nil {
		u := *turn.Usage
		if !send(provider.ChatEvent{Type: provider.EventUsage, Usage: &u}) {
			cancelled()
			return
		}
	}

	if turn.Err != nil {
		send(provider.ChatEvent{
			Type:   provider.EventError,
			Finish: provider.FinishError,
			Err:    turn.Err,
		})
		return
	}
	finish := turn.Finish
	if finish == "" {
		if len(turn.ToolCalls) > 0 {
			finish = provider.FinishToolCalls
		} else {
			finish = provider.FinishStop
		}
	}
	if !send(provider.ChatEvent{Type: provider.EventDone, Finish: finish}) {
		cancelled()
	}
}

// chunksOf resolves the emitted pieces of a text field.
func chunksOf(whole string, chunks []string) []string {
	if len(chunks) > 0 {
		return chunks
	}
	if whole == "" {
		return nil
	}
	return []string{whole}
}

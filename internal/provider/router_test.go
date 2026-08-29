package provider

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeProvider is a Provider stub for router tests. It performs no I/O, so the
// whole router suite runs with no network and no credentials (§41).
type fakeProvider struct {
	name      string
	caps      map[string]Capabilities
	capsErr   error
	healthErr error
	models    []Model
	// chat, when set, produces the stream for Chat.
	chat func(ctx context.Context, req ChatRequest) (<-chan ChatEvent, error)

	mu        sync.Mutex
	chatCalls []string
	healthHit int
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) Health(context.Context) error {
	f.mu.Lock()
	f.healthHit++
	f.mu.Unlock()
	return f.healthErr
}

func (f *fakeProvider) ListModels(context.Context) ([]Model, error) { return f.models, nil }

func (f *fakeProvider) Capabilities(_ context.Context, model string) (Capabilities, error) {
	if f.capsErr != nil {
		return nil, f.capsErr
	}
	return f.caps[model], nil
}

func (f *fakeProvider) Chat(ctx context.Context, req ChatRequest) (<-chan ChatEvent, error) {
	f.mu.Lock()
	f.chatCalls = append(f.chatCalls, req.Model)
	f.mu.Unlock()
	if f.chat != nil {
		return f.chat(ctx, req)
	}
	ch := make(chan ChatEvent, 2)
	ch <- ChatEvent{Type: EventDelta, Text: f.name}
	ch <- ChatEvent{Type: EventDone, Finish: FinishStop}
	close(ch)
	return ch, nil
}

func (f *fakeProvider) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.chatCalls...)
}

// streamOf builds a closed channel replaying evs.
func streamOf(evs ...ChatEvent) <-chan ChatEvent {
	ch := make(chan ChatEvent, len(evs))
	for _, ev := range evs {
		ch <- ev
	}
	close(ch)
	return ch
}

// fakeClock is a controllable time source for the health cache.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Unix(1700000000, 0)} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func retryable(msg string) error {
	return &Error{Category: ErrUnavailable, Message: msg}
}

func authFailure(msg string) error {
	return &Error{Category: ErrAuthentication, Message: msg}
}

func TestRegistry(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	local := &fakeProvider{name: "lemonade"}
	cloud := &fakeProvider{name: "openai"}

	if err := reg.Register(local); err != nil {
		t.Fatalf("Register(lemonade): %v", err)
	}
	if err := reg.Register(cloud); err != nil {
		t.Fatalf("Register(openai): %v", err)
	}

	tests := []struct {
		name    string
		p       Provider
		wantErr string
	}{
		{name: "nil provider", p: nil, wantErr: "nil provider"},
		{name: "unnamed provider", p: &fakeProvider{name: "  "}, wantErr: "no name"},
		{name: "duplicate name", p: &fakeProvider{name: "openai"}, wantErr: "already registered"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := reg.Register(tc.p)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Register() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}

	if got, ok := reg.Get("lemonade"); !ok || got != local {
		t.Fatalf("Get(lemonade) = %v, %v", got, ok)
	}
	if _, ok := reg.Get("ghost"); ok {
		t.Fatal("Get(ghost) reported a provider that was never registered")
	}
	if got, want := reg.Names(), []string{"lemonade", "openai"}; !equalStrings(got, want) {
		t.Fatalf("Names() = %v, want %v (registration order)", got, want)
	}
	if reg.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", reg.Len())
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// routingFixture builds a registry and config resembling the §9 example.
func routingFixture() (*Registry, RouterConfig) {
	lemonade := &fakeProvider{name: "lemonade", caps: map[string]Capabilities{
		"qwen3": Capabilities{}.Add(CapabilityStreaming, CapabilityTools),
	}}
	lmstudio := &fakeProvider{name: "lmstudio", caps: map[string]Capabilities{
		"qwen-vl": Capabilities{}.Add(CapabilityStreaming, CapabilityTools, CapabilityVision),
		"small":   Capabilities{}.Add(CapabilityStreaming),
	}}
	cloud := &fakeProvider{name: "openai", caps: map[string]Capabilities{
		"reasoner": Capabilities{}.Add(CapabilityStreaming, CapabilityTools, CapabilityReasoning, CapabilityVision),
	}}
	ollama := &fakeProvider{name: "ollama", caps: map[string]Capabilities{
		"llama3": Capabilities{}.Add(CapabilityStreaming, CapabilityTools),
	}}

	reg := NewRegistry(lemonade, lmstudio, cloud, ollama)
	cfg := RouterConfig{
		Classes: map[RouteClass]Target{
			ClassDefault:   {Provider: "lemonade", Model: "qwen3"},
			ClassVision:    {Provider: "lmstudio", Model: "qwen-vl"},
			ClassReasoning: {Provider: "openai", Model: "reasoner"},
			ClassFast:      {Provider: "ollama", Model: "llama3"},
		},
		Fallback: []string{"lemonade", "lmstudio", "ollama"},
		Models:   []Target{{Provider: "lmstudio", Model: "small"}},
	}
	return reg, cfg
}

func TestRouterClassResolution(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		sel        Selection
		wantTarget Target
		wantClass  RouteClass
		wantManual bool
		wantReason string
	}{
		{
			name:       "unset class uses default",
			sel:        Selection{},
			wantTarget: Target{Provider: "lemonade", Model: "qwen3"},
			wantClass:  ClassDefault,
			wantReason: "routing class default",
		},
		{
			name:       "explicit vision class",
			sel:        Selection{Class: ClassVision},
			wantTarget: Target{Provider: "lmstudio", Model: "qwen-vl"},
			wantClass:  ClassVision,
		},
		{
			name:       "explicit fast class",
			sel:        Selection{Class: ClassFast},
			wantTarget: Target{Provider: "ollama", Model: "llama3"},
			wantClass:  ClassFast,
		},
		{
			name:       "vision requirement infers the vision class",
			sel:        Selection{Required: Capabilities{CapabilityVision}},
			wantTarget: Target{Provider: "lmstudio", Model: "qwen-vl"},
			wantClass:  ClassVision,
		},
		{
			name:       "reasoning requirement infers the reasoning class",
			sel:        Selection{Required: Capabilities{CapabilityReasoning}},
			wantTarget: Target{Provider: "openai", Model: "reasoner"},
			wantClass:  ClassReasoning,
		},
		{
			name:       "unconfigured class degrades to default",
			sel:        Selection{Class: RouteClass("cheap")},
			wantTarget: Target{Provider: "lemonade", Model: "qwen3"},
			wantClass:  RouteClass("cheap"),
		},
		{
			name:       "manual provider wins over the class",
			sel:        Selection{Provider: "ollama", Class: ClassVision},
			wantTarget: Target{Provider: "ollama", Model: "llama3"},
			wantClass:  ClassVision,
			wantManual: true,
			wantReason: "provider selected manually",
		},
		{
			name:       "manual provider and model",
			sel:        Selection{Provider: "lmstudio", Model: "small"},
			wantTarget: Target{Provider: "lmstudio", Model: "small"},
			wantClass:  ClassDefault,
			wantManual: true,
		},
		{
			name:       "bare model resolves through the configured targets",
			sel:        Selection{Model: "llama3"},
			wantTarget: Target{Provider: "ollama", Model: "llama3"},
			wantClass:  ClassDefault,
			wantManual: true,
			wantReason: "model selected manually",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg, cfg := routingFixture()
			r := NewRouter(reg, cfg)

			p, dec, err := r.Resolve(context.Background(), tc.sel)
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if dec.Target != tc.wantTarget {
				t.Errorf("target = %v, want %v", dec.Target, tc.wantTarget)
			}
			if p.Name() != tc.wantTarget.Provider {
				t.Errorf("provider = %q, want %q", p.Name(), tc.wantTarget.Provider)
			}
			if dec.Class != tc.wantClass {
				t.Errorf("class = %q, want %q", dec.Class, tc.wantClass)
			}
			if dec.Manual != tc.wantManual {
				t.Errorf("manual = %v, want %v", dec.Manual, tc.wantManual)
			}
			if dec.Fellback {
				t.Error("fellback = true, want false for a first-candidate hit")
			}
			if tc.wantReason != "" && !strings.Contains(dec.Reason, tc.wantReason) {
				t.Errorf("reason = %q, want it to contain %q", dec.Reason, tc.wantReason)
			}
		})
	}
}

func TestRouterUnresolvableSelection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     RouterConfig
		sel     Selection
		wantErr string
	}{
		{
			name:    "no class configured at all",
			cfg:     RouterConfig{},
			sel:     Selection{},
			wantErr: `no routing target configured for class "default"`,
		},
		{
			name:    "ambiguous bare model",
			sel:     Selection{Model: "mystery"},
			wantErr: `model "mystery" is not configured for any provider`,
		},
		{
			name:    "manual provider is not registered",
			sel:     Selection{Provider: "ghost", Model: "m", NoFallback: true},
			wantErr: `provider "ghost" is not registered`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg, cfg := routingFixture()
			if tc.cfg.Classes != nil || tc.name == "no class configured at all" {
				cfg = tc.cfg
			}
			r := NewRouter(reg, cfg)

			_, _, err := r.Resolve(context.Background(), tc.sel)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Resolve() error = %v, want it to contain %q", err, tc.wantErr)
			}
			if _, ok := r.LastDecision(); !ok {
				t.Error("a failed resolution must still be recorded for debug output")
			}
		})
	}
}

func TestRouterCapabilityMismatchNamesAlternatives(t *testing.T) {
	t.Parallel()

	reg, cfg := routingFixture()
	// Pin the text-only default model but demand vision, with fallback off so
	// the mismatch is what surfaces.
	r := NewRouter(reg, cfg)

	_, dec, err := r.Resolve(context.Background(), Selection{
		Provider:   "lemonade",
		Model:      "qwen3",
		Class:      ClassDefault,
		Required:   Capabilities{CapabilityVision},
		NoFallback: true,
	})
	if err == nil {
		t.Fatal("Resolve() succeeded, want a capability error")
	}

	var unsupported *UnsupportedCapabilityError
	if !errors.As(err, &unsupported) {
		t.Fatalf("error %T does not unwrap to *UnsupportedCapabilityError", err)
	}
	if unsupported.Provider != "lemonade" || unsupported.Model != "qwen3" {
		t.Errorf("unsupported error names %s/%s, want lemonade/qwen3", unsupported.Provider, unsupported.Model)
	}
	if len(unsupported.Missing) != 1 || unsupported.Missing[0] != CapabilityVision {
		t.Errorf("missing = %v, want [vision]", unsupported.Missing)
	}

	var routed *CapabilityRoutingError
	if !errors.As(err, &routed) {
		t.Fatalf("error %T is not a *CapabilityRoutingError", err)
	}
	wantAlt := Target{Provider: "lmstudio", Model: "qwen-vl"}
	found := false
	for _, alt := range routed.Alternatives {
		if alt == wantAlt {
			found = true
		}
		if alt.Provider == "lemonade" {
			t.Errorf("alternatives include the failing provider's text-only model %v", alt)
		}
	}
	if !found {
		t.Errorf("alternatives = %v, want them to include %v", routed.Alternatives, wantAlt)
	}
	if msg := err.Error(); !strings.Contains(msg, "vision") || !strings.Contains(msg, "lmstudio/qwen-vl") {
		t.Errorf("error message %q must explain the missing capability and name an alternative", msg)
	}
	if len(dec.Attempts) != 1 || dec.Attempts[0].Err == nil {
		t.Errorf("attempts = %v, want the single failing candidate recorded", dec.Attempts)
	}
}

func TestRouterCapabilityMismatchWithNoAlternative(t *testing.T) {
	t.Parallel()

	reg := NewRegistry(&fakeProvider{name: "lemonade", caps: map[string]Capabilities{
		"qwen3": Capabilities{}.Add(CapabilityStreaming),
	}})
	r := NewRouter(reg, RouterConfig{
		Classes: map[RouteClass]Target{ClassDefault: {Provider: "lemonade", Model: "qwen3"}},
	})

	_, _, err := r.Resolve(context.Background(), Selection{Required: Capabilities{CapabilityVision}})
	if err == nil {
		t.Fatal("Resolve() succeeded, want a capability error")
	}
	if !strings.Contains(err.Error(), "no configured model provides them") {
		t.Errorf("error = %q, want it to say no configured model qualifies", err)
	}
}

func TestRouterCapabilityRoutesToACapableProvider(t *testing.T) {
	t.Parallel()

	reg, cfg := routingFixture()
	r := NewRouter(reg, cfg)

	// Pin the text-only provider but allow fallback: §8 point 4 says the
	// router may route automatically when configured to.
	_, dec, err := r.Resolve(context.Background(), Selection{
		Provider: "lemonade",
		Model:    "qwen3",
		Required: Capabilities{CapabilityVision},
	})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if want := (Target{Provider: "lmstudio", Model: "qwen-vl"}); dec.Target != want {
		t.Fatalf("target = %v, want %v", dec.Target, want)
	}
	if !dec.Fellback {
		t.Error("fellback = false, want true after the pinned model was rejected")
	}
}

func TestRouterFallbackOrdering(t *testing.T) {
	t.Parallel()

	reg, cfg := routingFixture()
	cfg.Fallback = []string{"lmstudio", "ollama"}
	r := NewRouter(reg, cfg)

	var tried []string
	dec, err := r.Do(context.Background(), Selection{}, func(_ context.Context, p Provider, model string) error {
		tried = append(tried, p.Name()+"/"+model)
		if p.Name() != "ollama" {
			return retryable(p.Name() + " is down")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}

	want := []string{"lemonade/qwen3", "lmstudio/qwen-vl", "ollama/llama3"}
	if !equalStrings(tried, want) {
		t.Fatalf("tried = %v, want %v", tried, want)
	}
	if got := (Target{Provider: "ollama", Model: "llama3"}); dec.Target != got {
		t.Errorf("target = %v, want %v", dec.Target, got)
	}
	if !dec.Fellback {
		t.Error("fellback = false, want true")
	}
	if len(dec.Attempts) != 2 {
		t.Fatalf("attempts = %v, want the two failures recorded", dec.Attempts)
	}
	if dec.Attempts[0].Target.Provider != "lemonade" || dec.Attempts[1].Target.Provider != "lmstudio" {
		t.Errorf("attempts recorded out of order: %v", dec.Attempts)
	}
	if !strings.Contains(dec.Reason, "fallback") {
		t.Errorf("reason = %q, want it to mention the fallback", dec.Reason)
	}
}

func TestRouterDoesNotFallBackOnNonRetryableErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
	}{
		{name: "authentication", err: authFailure("bad credentials")},
		{name: "invalid request", err: &Error{Category: ErrInvalidRequest, Message: "unknown model"}},
		{name: "unsupported capability", err: &Error{Category: ErrUnsupportedCapability, Message: "no tools"}},
		{name: "plain error", err: errors.New("something went wrong")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg, cfg := routingFixture()
			r := NewRouter(reg, cfg)

			var tried []string
			dec, err := r.Do(context.Background(), Selection{}, func(_ context.Context, p Provider, _ string) error {
				tried = append(tried, p.Name())
				return tc.err
			})
			if !errors.Is(err, tc.err) {
				t.Fatalf("Do() error = %v, want the original %v", err, tc.err)
			}
			if len(tried) != 1 {
				t.Fatalf("tried = %v, want only the primary candidate: a non-retryable failure repeats identically everywhere", tried)
			}
			if dec.Fellback {
				t.Error("fellback = true, want false")
			}
			if !strings.Contains(dec.Reason, "not retryable") {
				t.Errorf("reason = %q, want it to explain the stop", dec.Reason)
			}
		})
	}
}

func TestRouterExhaustsAllCandidates(t *testing.T) {
	t.Parallel()

	reg, cfg := routingFixture()
	r := NewRouter(reg, cfg)

	first := retryable("lemonade is down")
	dec, err := r.Do(context.Background(), Selection{}, func(_ context.Context, p Provider, _ string) error {
		if p.Name() == "lemonade" {
			return first
		}
		return retryable(p.Name() + " is down")
	})
	if !errors.Is(err, first) {
		t.Fatalf("Do() error = %v, want the first failure %v", err, first)
	}
	if !dec.Target.IsZero() {
		t.Errorf("target = %v, want unset after total failure", dec.Target)
	}
	if len(dec.Attempts) != 3 {
		t.Errorf("attempts = %v, want one per candidate", dec.Attempts)
	}
}

func TestRouterHealthCacheSkipsDownProviders(t *testing.T) {
	t.Parallel()

	clock := newClock()
	reg, cfg := routingFixture()
	cfg.Fallback = []string{"ollama"}
	cfg.Now = clock.now
	cfg.UnhealthyTTL = 30 * time.Second
	r := NewRouter(reg, cfg)

	r.MarkDown("lemonade", retryable("connection refused"))

	var tried []string
	run := func() {
		tried = nil
		if _, err := r.Do(context.Background(), Selection{}, func(_ context.Context, p Provider, _ string) error {
			tried = append(tried, p.Name())
			return nil
		}); err != nil {
			t.Fatalf("Do() error = %v", err)
		}
	}

	run()
	if !equalStrings(tried, []string{"ollama"}) {
		t.Fatalf("tried = %v, want the dead provider skipped entirely", tried)
	}

	// Still inside the TTL: the dead server must not be probed again.
	clock.advance(29 * time.Second)
	run()
	if !equalStrings(tried, []string{"ollama"}) {
		t.Fatalf("tried = %v, want the cached down verdict to still apply", tried)
	}

	// After the TTL the provider is worth another try.
	clock.advance(2 * time.Second)
	run()
	if !equalStrings(tried, []string{"lemonade"}) {
		t.Fatalf("tried = %v, want the expired verdict to allow a retry", tried)
	}
}

func TestRouterMarksProvidersDownAfterRetryableFailures(t *testing.T) {
	t.Parallel()

	clock := newClock()
	reg, cfg := routingFixture()
	cfg.Fallback = []string{"ollama"}
	cfg.Now = clock.now
	r := NewRouter(reg, cfg)

	var tried []string
	fn := func(_ context.Context, p Provider, _ string) error {
		tried = append(tried, p.Name())
		if p.Name() == "lemonade" {
			return retryable("connection refused")
		}
		return nil
	}
	if _, err := r.Do(context.Background(), Selection{}, fn); err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	if !equalStrings(tried, []string{"lemonade", "ollama"}) {
		t.Fatalf("tried = %v on the first call", tried)
	}

	tried = nil
	if _, err := r.Do(context.Background(), Selection{}, fn); err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	if !equalStrings(tried, []string{"ollama"}) {
		t.Fatalf("tried = %v, want the observed failure to suppress lemonade", tried)
	}

	snapshot := r.HealthSnapshot()
	if snapshot["lemonade"] == nil {
		t.Error("HealthSnapshot() should report lemonade as down")
	}
	if err, ok := snapshot["ollama"]; !ok || err != nil {
		t.Errorf("HealthSnapshot()[ollama] = %v, %v; want a healthy entry", err, ok)
	}
}

func TestRouterProbeHealth(t *testing.T) {
	t.Parallel()

	sick := &fakeProvider{name: "lemonade", healthErr: retryable("connection refused"),
		caps: map[string]Capabilities{"qwen3": {}}}
	well := &fakeProvider{name: "ollama", caps: map[string]Capabilities{"llama3": {}}}
	reg := NewRegistry(sick, well)

	r := NewRouter(reg, RouterConfig{
		Classes:     map[RouteClass]Target{ClassDefault: {Provider: "lemonade", Model: "qwen3"}},
		Fallback:    []string{"ollama"},
		Models:      []Target{{Provider: "ollama", Model: "llama3"}},
		ProbeHealth: true,
	})

	var tried []string
	dec, err := r.Do(context.Background(), Selection{}, func(_ context.Context, p Provider, _ string) error {
		tried = append(tried, p.Name())
		return nil
	})
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	if !equalStrings(tried, []string{"ollama"}) {
		t.Fatalf("tried = %v, want the failing probe to skip lemonade", tried)
	}
	if len(dec.Attempts) != 1 || !strings.Contains(dec.Attempts[0].Skipped, "health probe failed") {
		t.Errorf("attempts = %v, want the skipped probe recorded", dec.Attempts)
	}

	// A second routing pass must reuse the cached verdict rather than dialling
	// the dead server again.
	if _, err := r.Do(context.Background(), Selection{}, func(context.Context, Provider, string) error { return nil }); err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	sick.mu.Lock()
	hits := sick.healthHit
	sick.mu.Unlock()
	if hits != 1 {
		t.Errorf("health probes = %d, want 1: a dead local server must not be probed on every call", hits)
	}
}

func TestRouterNoFallbackPinsTheSelection(t *testing.T) {
	t.Parallel()

	reg, cfg := routingFixture()
	r := NewRouter(reg, cfg)

	var tried []string
	_, err := r.Do(context.Background(), Selection{Provider: "lemonade", NoFallback: true},
		func(_ context.Context, p Provider, _ string) error {
			tried = append(tried, p.Name())
			return retryable("down")
		})
	if err == nil {
		t.Fatal("Do() succeeded, want the pinned failure")
	}
	if !equalStrings(tried, []string{"lemonade"}) {
		t.Fatalf("tried = %v, want only the pinned provider", tried)
	}
}

func TestRouterDecisionRecording(t *testing.T) {
	t.Parallel()

	reg, cfg := routingFixture()
	cfg.HistorySize = 3
	r := NewRouter(reg, cfg)

	for _, class := range []RouteClass{ClassDefault, ClassVision, ClassReasoning, ClassFast} {
		if _, _, err := r.Resolve(context.Background(), Selection{Class: class}); err != nil {
			t.Fatalf("Resolve(%s) error = %v", class, err)
		}
	}

	history := r.Decisions()
	if len(history) != 3 {
		t.Fatalf("Decisions() length = %d, want the history bounded at 3", len(history))
	}
	if history[0].Class != ClassFast {
		t.Errorf("Decisions()[0].Class = %q, want the most recent (%q) first", history[0].Class, ClassFast)
	}
	if history[2].Class != ClassVision {
		t.Errorf("Decisions()[2].Class = %q, want the oldest retained entry", history[2].Class)
	}

	last, ok := r.LastDecision()
	if !ok {
		t.Fatal("LastDecision() reported no decision")
	}
	line := last.String()
	for _, want := range []string{"ollama/llama3", "class=fast"} {
		if !strings.Contains(line, want) {
			t.Errorf("Decision.String() = %q, want it to contain %q", line, want)
		}
	}
}

func TestDecisionStringIncludesAttempts(t *testing.T) {
	t.Parallel()

	dec := Decision{
		Target:   Target{Provider: "ollama", Model: "llama3"},
		Class:    ClassDefault,
		Manual:   true,
		Fellback: true,
		Reason:   "fallback #1 after earlier candidates failed",
		Required: Capabilities{CapabilityTools},
		Attempts: []Attempt{
			{Target: Target{Provider: "lemonade", Model: "qwen3"}, Skipped: "unhealthy: refused"},
			{Target: Target{Provider: "lmstudio", Model: "qwen-vl"}, Err: retryable("boom")},
		},
	}
	line := dec.String()
	for _, want := range []string{"route ollama/llama3", "manual", "fallback", "needs=tools",
		"lemonade/qwen3 skipped (unhealthy: refused)", "lmstudio/qwen-vl failed"} {
		if !strings.Contains(line, want) {
			t.Errorf("Decision.String() = %q, want it to contain %q", line, want)
		}
	}

	if got := (Decision{Class: ClassDefault}).String(); !strings.Contains(got, "route unresolved") {
		t.Errorf("unresolved Decision.String() = %q", got)
	}
}

func TestRouterChatFallsBackOnRetryableStreamError(t *testing.T) {
	t.Parallel()

	down := &fakeProvider{
		name: "lemonade",
		caps: map[string]Capabilities{"qwen3": Capabilities{}.Add(CapabilityStreaming)},
		chat: func(context.Context, ChatRequest) (<-chan ChatEvent, error) {
			// A post-dispatch failure is reported as a terminal EventError,
			// not as a Chat error; the router has to notice it there.
			return streamOf(ChatEvent{Type: EventError, Err: retryable("server overloaded"), Finish: FinishError}), nil
		},
	}
	up := &fakeProvider{name: "ollama", caps: map[string]Capabilities{"llama3": Capabilities{}.Add(CapabilityStreaming)}}

	r := NewRouter(NewRegistry(down, up), RouterConfig{
		Classes:  map[RouteClass]Target{ClassDefault: {Provider: "lemonade", Model: "qwen3"}},
		Fallback: []string{"ollama"},
		Models:   []Target{{Provider: "ollama", Model: "llama3"}},
	})

	stream, dec, err := r.Chat(context.Background(), Selection{}, ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		Stream:   true,
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if want := (Target{Provider: "ollama", Model: "llama3"}); dec.Target != want {
		t.Fatalf("target = %v, want %v", dec.Target, want)
	}

	var texts []string
	var finish FinishReason
	for ev := range stream {
		switch ev.Type {
		case EventDelta:
			texts = append(texts, ev.Text)
		case EventDone:
			finish = ev.Finish
		case EventError:
			t.Fatalf("unexpected error event: %v", ev.Err)
		}
	}
	if !equalStrings(texts, []string{"ollama"}) {
		t.Errorf("deltas = %v, want the fallback provider's stream replayed in full", texts)
	}
	if finish != FinishStop {
		t.Errorf("finish = %q, want %q", finish, FinishStop)
	}
	if got := up.calls(); !equalStrings(got, []string{"llama3"}) {
		t.Errorf("fallback provider called with %v, want the model configured for it", got)
	}
}

func TestRouterChatDoesNotFallBackOnAuthError(t *testing.T) {
	t.Parallel()

	authErr := authFailure("invalid api key")
	broken := &fakeProvider{
		name: "openai",
		caps: map[string]Capabilities{"reasoner": Capabilities{}.Add(CapabilityStreaming)},
		chat: func(context.Context, ChatRequest) (<-chan ChatEvent, error) {
			return streamOf(ChatEvent{Type: EventError, Err: authErr, Finish: FinishError}), nil
		},
	}
	spare := &fakeProvider{name: "ollama", caps: map[string]Capabilities{"llama3": {}}}

	r := NewRouter(NewRegistry(broken, spare), RouterConfig{
		Classes:  map[RouteClass]Target{ClassDefault: {Provider: "openai", Model: "reasoner"}},
		Fallback: []string{"ollama"},
		Models:   []Target{{Provider: "ollama", Model: "llama3"}},
	})

	_, _, err := r.Chat(context.Background(), Selection{}, ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if !errors.Is(err, authErr) {
		t.Fatalf("Chat() error = %v, want the authentication failure", err)
	}
	if got := spare.calls(); len(got) != 0 {
		t.Errorf("fallback provider was called %v times; a bad key fails identically everywhere", got)
	}
}

func TestRouterChatReplaysTheWholeStream(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{
		name: "lemonade",
		caps: map[string]Capabilities{"qwen3": Capabilities{}.Add(CapabilityStreaming)},
		chat: func(context.Context, ChatRequest) (<-chan ChatEvent, error) {
			return streamOf(
				ChatEvent{Type: EventDelta, Text: "one "},
				ChatEvent{Type: EventDelta, Text: "two"},
				ChatEvent{Type: EventUsage, Usage: &Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}},
				ChatEvent{Type: EventDone, Finish: FinishStop},
			), nil
		},
	}
	r := NewRouter(NewRegistry(p), RouterConfig{
		Classes: map[RouteClass]Target{ClassDefault: {Provider: "lemonade", Model: "qwen3"}},
	})

	stream, dec, err := r.Chat(context.Background(), Selection{}, ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if dec.Fellback {
		t.Error("fellback = true, want false")
	}

	var got []EventType
	var text strings.Builder
	for ev := range stream {
		got = append(got, ev.Type)
		if ev.Type == EventDelta {
			text.WriteString(ev.Text)
		}
	}
	want := []EventType{EventDelta, EventDelta, EventUsage, EventDone}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events = %v, want %v", got, want)
		}
	}
	if text.String() != "one two" {
		t.Errorf("text = %q, want %q", text.String(), "one two")
	}
}

func TestRouterChatRejectsAnEmptyStream(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{
		name: "lemonade",
		caps: map[string]Capabilities{"qwen3": {}},
		chat: func(context.Context, ChatRequest) (<-chan ChatEvent, error) {
			return streamOf(), nil
		},
	}
	r := NewRouter(NewRegistry(p), RouterConfig{
		Classes: map[RouteClass]Target{ClassDefault: {Provider: "lemonade", Model: "qwen3"}},
	})

	_, _, err := r.Chat(context.Background(), Selection{}, ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err == nil {
		t.Fatal("Chat() succeeded on an empty stream")
	}
	if cat, ok := CategoryOf(err); !ok || cat != ErrMalformedResponse {
		t.Errorf("category = %q, want %q", cat, ErrMalformedResponse)
	}
}

func TestRouterAlternatives(t *testing.T) {
	t.Parallel()

	reg, cfg := routingFixture()
	r := NewRouter(reg, cfg)

	got := r.Alternatives(context.Background(), CapabilityVision)
	want := []Target{{Provider: "lmstudio", Model: "qwen-vl"}, {Provider: "openai", Model: "reasoner"}}
	if len(got) != len(want) {
		t.Fatalf("Alternatives(vision) = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Alternatives(vision) = %v, want %v (configuration order)", got, want)
		}
	}

	if alts := r.Alternatives(context.Background()); alts != nil {
		t.Errorf("Alternatives() with no requirement = %v, want nil", alts)
	}
}

func TestRouterClassesAndFallbackAreCopies(t *testing.T) {
	t.Parallel()

	reg, cfg := routingFixture()
	r := NewRouter(reg, cfg)

	classes := r.Classes()
	classes[ClassDefault] = Target{Provider: "tampered"}
	fallback := r.Fallback()
	if len(fallback) > 0 {
		fallback[0] = "tampered"
	}

	if _, dec, err := r.Resolve(context.Background(), Selection{}); err != nil || dec.Target.Provider != "lemonade" {
		t.Fatalf("Resolve() = %v, %v; router state must not be mutable through its accessors", dec.Target, err)
	}
	if r.Registry() != reg {
		t.Error("Registry() did not return the registry the router was built with")
	}
}

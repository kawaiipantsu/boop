package stats

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/boop-dev/boop/internal/provider"
)

var testDay = time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)

// fixedClock returns a clock that always reports at, so day bucketing and
// uptime are deterministic.
func fixedClock(at time.Time) func() time.Time {
	return func() time.Time { return at }
}

func newTestTracker(t *testing.T, opts ...Option) *Tracker {
	t.Helper()
	base := []Option{WithClock(fixedClock(testDay)), WithLocation(time.UTC)}
	return New(append(base, opts...)...)
}

func TestTrackerAggregatesAcrossDimensions(t *testing.T) {
	t.Parallel()
	tr := newTestTracker(t)

	scope := Scope{SessionID: "s1", AgentID: "a1", Provider: "anthropic", Model: "claude-opus-5"}
	tr.RecordModelCall(ModelCall{
		Scope:    scope,
		Usage:    provider.Usage{PromptTokens: 1_000_000, CompletionTokens: 100_000},
		Duration: 2 * time.Second,
		At:       testDay,
	})
	tr.RecordToolCall(ToolInvocation{Scope: scope, Tool: "run", Duration: 300 * time.Millisecond, At: testDay})
	tr.RecordCommand(CommandRun{Scope: scope, Command: "go test ./...", ExitCode: 1, Duration: time.Second, At: testDay})
	tr.RecordMessage(MessageEvent{Scope: scope, Role: provider.RoleUser, At: testDay})

	snap := tr.Snapshot()
	dimensions := map[string]Aggregate{
		"totals":   snap.Totals,
		"session":  snap.Sessions["s1"],
		"agent":    snap.Agents["a1"],
		"provider": snap.Providers["anthropic"],
		"model":    snap.Models["anthropic/claude-opus-5"],
		"day":      snap.Days["2026-08-29"],
	}
	if len(snap.Sessions) != 1 || len(snap.Agents) != 1 || len(snap.Providers) != 1 || len(snap.Models) != 1 || len(snap.Days) != 1 {
		t.Fatalf("unexpected bucket counts: %+v", snap)
	}

	for name, a := range dimensions {
		if a.Counters.ModelCalls != 1 || a.Counters.ToolCalls != 1 || a.Counters.Commands != 1 || a.Counters.Messages != 1 {
			t.Fatalf("%s counters = %+v", name, a.Counters)
		}
		if a.Counters.CommandFailures != 1 {
			t.Fatalf("%s did not record the command failure: %+v", name, a.Counters)
		}
		if a.Tokens.Measured.Prompt != 1_000_000 || a.Tokens.Measured.Completion != 100_000 || a.Tokens.Measured.Total != 1_100_000 {
			t.Fatalf("%s tokens = %+v", name, a.Tokens)
		}
		if !a.Tokens.Exact() {
			t.Fatalf("%s should hold only measured tokens", name)
		}
		if a.Durations.Model.Duration() != 2*time.Second || a.Durations.Command.Duration() != time.Second || a.Durations.Tool.Duration() != 300*time.Millisecond {
			t.Fatalf("%s durations = %+v", name, a.Durations)
		}
		// claude-opus-5: $5/1M in, $25/1M out -> 5 + 2.5
		almostEqual(t, a.Cost.Measured, 7.5)
		if a.Cost.Estimated != 0 || a.Cost.UnpricedCalls != 0 || a.Cost.FreeCalls != 0 {
			t.Fatalf("%s cost = %+v", name, a.Cost)
		}
		if a.FirstSeen != testDay || a.LastSeen != testDay {
			t.Fatalf("%s timestamps = %v/%v", name, a.FirstSeen, a.LastSeen)
		}
	}

	if snap.Sessions["s1"].Key != "s1" || snap.Days["2026-08-29"].Key != "2026-08-29" {
		t.Fatal("bucket keys not set")
	}
	if snap.Totals.Key != "" {
		t.Fatalf("totals should carry no key, got %q", snap.Totals.Key)
	}
}

func TestTrackerPartialScopes(t *testing.T) {
	t.Parallel()
	tr := newTestTracker(t)

	// No agent: agent-less activity must not create an "" bucket.
	tr.RecordToolCall(ToolInvocation{Scope: Scope{SessionID: "s1"}, Tool: "read", At: testDay})
	// No session either.
	tr.RecordCommand(CommandRun{Command: "ls", At: testDay})

	snap := tr.Snapshot()
	if len(snap.Agents) != 0 || len(snap.Providers) != 0 || len(snap.Models) != 0 {
		t.Fatalf("empty scope fields created buckets: %+v", snap)
	}
	if len(snap.Sessions) != 1 {
		t.Fatalf("sessions = %+v", snap.Sessions)
	}
	if snap.Totals.Counters.ToolCalls != 1 || snap.Totals.Counters.Commands != 1 {
		t.Fatalf("totals = %+v", snap.Totals.Counters)
	}
	if snap.Sessions["s1"].Counters.Commands != 0 {
		t.Fatal("scope-less command leaked into the session bucket")
	}
}

func TestTrackerDayBucketing(t *testing.T) {
	t.Parallel()
	tr := newTestTracker(t)

	d1 := time.Date(2026, 8, 29, 23, 30, 0, 0, time.UTC)
	d2 := time.Date(2026, 8, 30, 0, 30, 0, 0, time.UTC)
	tr.RecordMessage(MessageEvent{Scope: Scope{SessionID: "s"}, At: d1})
	tr.RecordMessage(MessageEvent{Scope: Scope{SessionID: "s"}, At: d2})
	tr.RecordMessage(MessageEvent{Scope: Scope{SessionID: "s"}, At: d2})

	snap := tr.Snapshot()
	if got := snap.Days["2026-08-29"].Counters.Messages; got != 1 {
		t.Fatalf("day 29 messages = %d", got)
	}
	if got := snap.Days["2026-08-30"].Counters.Messages; got != 2 {
		t.Fatalf("day 30 messages = %d", got)
	}
	if snap.Sessions["s"].FirstSeen != d1 || snap.Sessions["s"].LastSeen != d2 {
		t.Fatalf("first/last seen = %v/%v", snap.Sessions["s"].FirstSeen, snap.Sessions["s"].LastSeen)
	}

	// A zero At falls back to the tracker clock rather than year 1.
	tr.RecordMessage(MessageEvent{Scope: Scope{SessionID: "s"}})
	if _, ok := tr.DayStats(testDay); !ok {
		t.Fatal("zero timestamp was not stamped with the tracker clock")
	}
}

func TestTrackerCounters(t *testing.T) {
	t.Parallel()
	scope := Scope{SessionID: "s", AgentID: "a"}

	tests := []struct {
		name   string
		record func(*Tracker)
		want   Counters
	}{
		{
			name:   "failed model call",
			record: func(tr *Tracker) { tr.RecordModelCall(ModelCall{Scope: scope, Failed: true, At: testDay}) },
			want:   Counters{ModelCalls: 1, ModelCallFailures: 1},
		},
		{
			name: "failed tool call",
			record: func(tr *Tracker) {
				tr.RecordToolCall(ToolInvocation{Scope: scope, Tool: "edit", Failed: true, At: testDay})
			},
			want: Counters{ToolCalls: 1, ToolFailures: 1},
		},
		{
			name:   "successful command",
			record: func(tr *Tracker) { tr.RecordCommand(CommandRun{Scope: scope, At: testDay}) },
			want:   Counters{Commands: 1},
		},
		{
			name:   "timed out command counts as a failure too",
			record: func(tr *Tracker) { tr.RecordCommand(CommandRun{Scope: scope, TimedOut: true, At: testDay}) },
			want:   Counters{Commands: 1, CommandFailures: 1, CommandTimeouts: 1},
		},
		{
			name:   "cancelled command counts as a failure",
			record: func(tr *Tracker) { tr.RecordCommand(CommandRun{Scope: scope, Cancelled: true, At: testDay}) },
			want:   Counters{Commands: 1, CommandFailures: 1},
		},
		{
			name: "repair loop iterations",
			record: func(tr *Tracker) {
				tr.RecordRepairIteration(RepairIteration{Scope: scope, Attempt: 1, At: testDay})
				tr.RecordRepairIteration(RepairIteration{Scope: scope, Attempt: 2, Succeeded: true, At: testDay})
			},
			want: Counters{RepairIterations: 2, RepairSuccesses: 1},
		},
		{
			name: "test runs split pass and fail",
			record: func(tr *Tracker) {
				tr.RecordTestRun(TestRun{Scope: scope, Suite: "unit", Passed: 10, Skipped: 1, At: testDay})
				tr.RecordTestRun(TestRun{Scope: scope, Suite: "unit", Passed: 8, Failed: 2, At: testDay})
			},
			want: Counters{TestRuns: 2, TestRunsFailed: 1, TestsPassed: 18, TestsFailed: 2, TestsSkipped: 1},
		},
		{
			name: "negative test counts are ignored",
			record: func(tr *Tracker) {
				tr.RecordTestRun(TestRun{Scope: scope, Passed: -3, Failed: -1, Skipped: -1, At: testDay})
			},
			want: Counters{TestRuns: 1},
		},
		{
			name: "agent lifecycle",
			record: func(tr *Tracker) {
				tr.RecordAgentSpawned(AgentEvent{Scope: scope, At: testDay})
				tr.RecordAgentCompleted(AgentEvent{Scope: scope, At: testDay})
				tr.RecordAgentSpawned(AgentEvent{Scope: scope, At: testDay})
				tr.RecordAgentCompleted(AgentEvent{Scope: scope, Failed: true, At: testDay})
			},
			want: Counters{AgentsSpawned: 2, AgentsCompleted: 2, AgentsFailed: 1},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := newTestTracker(t)
			tc.record(tr)
			if got := tr.Totals().Counters; got != tc.want {
				t.Fatalf("totals = %+v, want %+v", got, tc.want)
			}
			agg, ok := tr.SessionStats("s")
			if !ok || agg.Counters != tc.want {
				t.Fatalf("session = %+v (ok=%v), want %+v", agg.Counters, ok, tc.want)
			}
			agg, ok = tr.AgentStats("a")
			if !ok || agg.Counters != tc.want {
				t.Fatalf("agent = %+v (ok=%v), want %+v", agg.Counters, ok, tc.want)
			}
		})
	}
}

func TestTrackerMeasuredVersusEstimated(t *testing.T) {
	t.Parallel()
	scope := Scope{SessionID: "s", Provider: "anthropic", Model: "claude-opus-5"}
	usage := provider.Usage{PromptTokens: 1_000_000, CompletionTokens: 0}

	t.Run("measured", func(t *testing.T) {
		tr := newTestTracker(t)
		tr.RecordModelCall(ModelCall{Scope: scope, Usage: usage, At: testDay})
		a := tr.Totals()
		if a.Tokens.Measured.Prompt != 1_000_000 || !a.Tokens.Estimated.IsZero() {
			t.Fatalf("tokens = %+v", a.Tokens)
		}
		almostEqual(t, a.Cost.Measured, 5)
		almostEqual(t, a.Cost.Estimated, 0)
		u, _ := tr.Context().Usage("s")
		if u.Estimated {
			t.Fatal("measured call produced an estimated context reading")
		}
	})

	t.Run("estimated", func(t *testing.T) {
		tr := newTestTracker(t)
		tr.RecordEstimatedModelCall(ModelCall{Scope: scope, Usage: usage, At: testDay})
		a := tr.Totals()
		if a.Tokens.Estimated.Prompt != 1_000_000 || !a.Tokens.Measured.IsZero() {
			t.Fatalf("tokens = %+v", a.Tokens)
		}
		almostEqual(t, a.Cost.Estimated, 5)
		almostEqual(t, a.Cost.Measured, 0)
		if a.Tokens.Exact() {
			t.Fatal("estimated tokens must not be reported as exact")
		}
		u, _ := tr.Context().Usage("s")
		if !u.Estimated {
			t.Fatal("estimated call lost its provenance in the context tracker")
		}
	})

	t.Run("unreported usage adds no tokens", func(t *testing.T) {
		tr := newTestTracker(t)
		tr.RecordModelCall(ModelCall{Scope: scope, At: testDay})
		a := tr.Totals()
		if !a.Tokens.IsZero() {
			t.Fatalf("silent zero usage was recorded as a measurement: %+v", a.Tokens)
		}
		if a.Counters.ModelCalls != 1 {
			t.Fatal("call itself should still be counted")
		}
		if _, ok := tr.Context().Usage("s"); ok {
			t.Fatal("context occupancy invented from unreported usage")
		}
	})
}

// stubEstimator is a deterministic Estimator used to prove that the tracker
// really delegates estimation and that the substitution is visible in the
// snapshot.
type stubEstimator struct {
	prompt     int
	completion int
	calls      int
	mu         sync.Mutex
}

func (s *stubEstimator) Name() string { return "stub" }

func (s *stubEstimator) EstimateText(string, string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.completion
}

func (s *stubEstimator) EstimateRequest(string, provider.ChatRequest) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.prompt
}

func TestTrackerFallbackEstimation(t *testing.T) {
	t.Parallel()
	stub := &stubEstimator{prompt: 2_000_000, completion: 1_000_000}
	tr := newTestTracker(t, WithEstimator(stub))

	req := provider.ChatRequest{Model: "claude-opus-5", Messages: []provider.Message{{Content: strings.Repeat("x", 100)}}}
	scope := Scope{SessionID: "s", Provider: "anthropic", Model: "claude-opus-5"}

	if got := tr.RecordModelCallWithFallback(ModelCall{Scope: scope, At: testDay}, req, "hello"); got != SourceEstimated {
		t.Fatalf("source = %q, want estimated", got)
	}
	if stub.calls == 0 {
		t.Fatal("substituted estimator was not consulted")
	}

	a := tr.Totals()
	if a.Tokens.Estimated.Prompt != 2_000_000 || a.Tokens.Estimated.Completion != 1_000_000 {
		t.Fatalf("estimated tokens = %+v", a.Tokens.Estimated)
	}
	if !a.Tokens.Measured.IsZero() {
		t.Fatalf("measured bucket was polluted: %+v", a.Tokens.Measured)
	}
	// 2M prompt * $5 + 1M completion * $25
	almostEqual(t, a.Cost.Estimated, 35)
	almostEqual(t, a.Cost.Measured, 0)

	// A provider that does report usage must not be second-guessed.
	measured := provider.Usage{PromptTokens: 7, CompletionTokens: 3}
	if got := tr.RecordModelCallWithFallback(ModelCall{Scope: scope, Usage: measured, At: testDay}, req, "hello"); got != SourceMeasured {
		t.Fatalf("source = %q, want measured", got)
	}

	snap := tr.Snapshot()
	if snap.Estimator != "stub" {
		t.Fatalf("snapshot estimator = %q, want stub", snap.Estimator)
	}
	if snap.Totals.Tokens.Measured.Total != 10 {
		t.Fatalf("measured total = %d", snap.Totals.Tokens.Measured.Total)
	}
	if snap.Totals.Tokens.Estimated.Total != 3_000_000 {
		t.Fatalf("estimated total = %d", snap.Totals.Tokens.Estimated.Total)
	}

	// The distinction must survive serialisation.
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Snapshot
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Totals.Tokens.Measured.Total != 10 || back.Totals.Tokens.Estimated.Total != 3_000_000 {
		t.Fatalf("provenance lost in JSON: %+v", back.Totals.Tokens)
	}
	if back.Estimator != "stub" || back.Totals.Tokens.Exact() {
		t.Fatalf("round-tripped snapshot = %+v", back.Totals.Tokens)
	}
	almostEqual(t, back.Totals.Cost.Estimated, 35)
}

func TestTrackerCostReporting(t *testing.T) {
	t.Parallel()
	tr := newTestTracker(t)
	usage := provider.Usage{PromptTokens: 1_000_000, CompletionTokens: 1_000_000}

	tr.RecordModelCall(ModelCall{Scope: Scope{SessionID: "s", Provider: ProviderOllama, Model: "llama3.2"}, Usage: usage, At: testDay})
	tr.RecordModelCall(ModelCall{Scope: Scope{SessionID: "s", Provider: "openai", Model: "who-knows"}, Usage: usage, At: testDay})
	tr.RecordModelCall(ModelCall{Scope: Scope{SessionID: "s", Provider: "anthropic", Model: "claude-haiku-4-5"}, Usage: usage, At: testDay})

	a, _ := tr.SessionStats("s")
	if a.Cost.FreeCalls != 1 {
		t.Fatalf("local call not counted as free: %+v", a.Cost)
	}
	if a.Cost.UnpricedCalls != 1 {
		t.Fatalf("unknown model must be counted as unpriced, not free: %+v", a.Cost)
	}
	if a.Cost.Complete() {
		t.Fatal("a bucket containing an unpriced call is not complete")
	}
	almostEqual(t, a.Cost.Measured, 6) // haiku: 1 + 5
	almostEqual(t, a.Cost.Total(), 6)

	local, _ := tr.ProviderStats(ProviderOllama)
	if local.Cost.Measured != 0 || local.Cost.FreeCalls != 1 || !local.Cost.Complete() {
		t.Fatalf("local provider bucket = %+v", local.Cost)
	}
	if local.Tokens.Measured.Total != 2_000_000 {
		t.Fatal("local providers must still track tokens")
	}
	if a.Cost.Currency != DefaultCurrency {
		t.Fatalf("currency = %q", a.Cost.Currency)
	}
}

func TestTrackerCustomRates(t *testing.T) {
	t.Parallel()
	tbl := NewEmptyRateTable()
	tbl.Set(Rate{Provider: "acme", Model: "m1", InputPerMillion: 100, OutputPerMillion: 200})
	tr := newTestTracker(t, WithRates(tbl))

	tr.RecordModelCall(ModelCall{
		Scope: Scope{SessionID: "s", Provider: "acme", Model: "m1"},
		Usage: provider.Usage{PromptTokens: 1_000_000, CompletionTokens: 500_000},
		At:    testDay,
	})
	almostEqual(t, tr.Totals().Cost.Measured, 200)

	// Overriding at runtime affects subsequent calls only, which is what the
	// per-call pricing model implies.
	tr.Rates().Set(Rate{Provider: "acme", Model: "m1", InputPerMillion: 0, OutputPerMillion: 0})
	tr.RecordModelCall(ModelCall{
		Scope: Scope{SessionID: "s", Provider: "acme", Model: "m1"},
		Usage: provider.Usage{PromptTokens: 1_000_000},
		At:    testDay,
	})
	almostEqual(t, tr.Totals().Cost.Measured, 200)
	if tr.Totals().Cost.UnpricedCalls != 0 {
		t.Fatal("an explicit zero rate is priced, not unpriced")
	}
}

func TestTrackerContextWindowIntegration(t *testing.T) {
	t.Parallel()
	tr := newTestTracker(t)
	tr.Context().RegisterModels(provider.Model{Provider: "anthropic", ID: "claude-opus-5", ContextWindow: 1_000_000})

	tr.RecordModelCall(ModelCall{
		Scope: Scope{SessionID: "s", Provider: "anthropic", Model: "claude-opus-5"},
		Usage: provider.Usage{PromptTokens: 400_000, CompletionTokens: 100_000},
		At:    testDay,
	})

	snap := tr.Snapshot()
	u, ok := snap.Contexts["s"]
	if !ok {
		t.Fatal("snapshot missing context usage")
	}
	// Occupancy is prompt+completion: the floor for the next request.
	if u.Tokens != 500_000 || u.Utilisation != 0.5 || u.Remaining != 500_000 || !u.WindowKnown {
		t.Fatalf("context usage = %+v", u)
	}

	// An unregistered model yields a usable but window-less reading.
	tr.RecordModelCall(ModelCall{
		Scope: Scope{SessionID: "s2", Provider: ProviderOllama, Model: "mystery"},
		Usage: provider.Usage{PromptTokens: 1_000},
		At:    testDay,
	})
	u2 := tr.Snapshot().Contexts["s2"]
	if u2.WindowKnown || u2.Utilisation != 0 || u2.Tokens != 1_000 {
		t.Fatalf("unknown window usage = %+v", u2)
	}
}

func TestTrackerSnapshotIsAnIsolatedCopy(t *testing.T) {
	t.Parallel()
	tr := newTestTracker(t)
	scope := Scope{SessionID: "s", Provider: "anthropic", Model: "claude-opus-5"}
	tr.RecordMessage(MessageEvent{Scope: scope, At: testDay})

	snap := tr.Snapshot()
	tr.RecordMessage(MessageEvent{Scope: scope, At: testDay})

	if snap.Totals.Counters.Messages != 1 || snap.Sessions["s"].Counters.Messages != 1 {
		t.Fatalf("snapshot mutated after the fact: %+v", snap.Totals.Counters)
	}
	if tr.Totals().Counters.Messages != 2 {
		t.Fatal("tracker did not keep counting")
	}

	agg := snap.Sessions["s"]
	agg.Counters.Messages = 999
	if snap.Sessions["s"].Counters.Messages != 1 {
		t.Fatal("snapshot buckets are aliased")
	}
}

func TestTrackerReset(t *testing.T) {
	t.Parallel()
	now := testDay
	tr := New(WithClock(func() time.Time { return now }), WithLocation(time.UTC))
	tr.Context().SetWindow("anthropic", "claude-opus-5", 1_000_000)
	tr.RecordModelCall(ModelCall{
		Scope: Scope{SessionID: "s", Provider: "anthropic", Model: "claude-opus-5"},
		Usage: provider.Usage{PromptTokens: 10},
		At:    testDay,
	})

	now = testDay.Add(time.Hour)
	tr.Reset()

	snap := tr.Snapshot()
	if snap.Totals.Counters.ModelCalls != 0 || len(snap.Sessions) != 0 || len(snap.Days) != 0 || len(snap.Contexts) != 0 {
		t.Fatalf("Reset left state behind: %+v", snap)
	}
	if !snap.StartedAt.Equal(now) {
		t.Fatalf("StartedAt = %v, want %v", snap.StartedAt, now)
	}
	if _, known := tr.Context().Window("anthropic", "claude-opus-5"); !known {
		t.Fatal("Reset dropped registered context windows")
	}
}

func TestTrackerUptime(t *testing.T) {
	t.Parallel()
	now := testDay
	tr := New(WithClock(func() time.Time { return now }))
	now = testDay.Add(90 * time.Second)
	if got := tr.Uptime(); got != 90*time.Second {
		t.Fatalf("uptime = %v", got)
	}
	if got := tr.Snapshot().Uptime.Duration(); got != 90*time.Second {
		t.Fatalf("snapshot uptime = %v", got)
	}
}

func TestTrackerLookupMisses(t *testing.T) {
	t.Parallel()
	tr := newTestTracker(t)
	if _, ok := tr.SessionStats(""); ok {
		t.Fatal("empty key matched")
	}
	if _, ok := tr.SessionStats("nope"); ok {
		t.Fatal("unknown session matched")
	}
	if _, ok := tr.AgentStats("nope"); ok {
		t.Fatal("unknown agent matched")
	}
	if _, ok := tr.ProviderStats("nope"); ok {
		t.Fatal("unknown provider matched")
	}
	if _, ok := tr.ModelStats("nope", "nope"); ok {
		t.Fatal("unknown model matched")
	}
	if _, ok := tr.DayStats(testDay); ok {
		t.Fatal("unknown day matched")
	}
	if tr.Estimator().Name() == "" || tr.Rates() == nil || tr.Context() == nil {
		t.Fatal("accessors returned nothing useful")
	}
}

func TestTrackerOptionsIgnoreNils(t *testing.T) {
	t.Parallel()
	tr := New(WithClock(nil), WithLocation(nil), WithRates(nil), WithEstimator(nil))
	if tr.now == nil || tr.loc == nil || tr.rates == nil || tr.estimator == nil {
		t.Fatal("nil options clobbered a default")
	}
}

func TestTrackerConcurrentUpdates(t *testing.T) {
	t.Parallel()
	tr := newTestTracker(t)

	const (
		workers = 8
		rounds  = 200
	)
	sessions := []string{"s0", "s1", "s2"}
	providers := []struct{ name, model string }{
		{"anthropic", "claude-opus-5"},
		{ProviderOllama, "llama3.2"},
		{"openai", "unpriced-model"},
	}
	tr.Context().RegisterModels(provider.Model{Provider: "anthropic", ID: "claude-opus-5", ContextWindow: 1_000_000})

	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range rounds {
				p := providers[(w+i)%len(providers)]
				scope := Scope{
					SessionID: sessions[i%len(sessions)],
					AgentID:   "agent-" + string(rune('a'+w)),
					Provider:  p.name,
					Model:     p.model,
				}
				tr.RecordModelCall(ModelCall{Scope: scope, Usage: provider.Usage{PromptTokens: 10, CompletionTokens: 5}, Duration: time.Millisecond, At: testDay})
				tr.RecordEstimatedModelCall(ModelCall{Scope: scope, Usage: provider.Usage{PromptTokens: 100, CompletionTokens: 50}, At: testDay})
				tr.RecordToolCall(ToolInvocation{Scope: scope, Tool: "run", Duration: time.Millisecond, At: testDay})
				tr.RecordCommand(CommandRun{Scope: scope, ExitCode: i % 2, Duration: time.Millisecond, At: testDay})
				tr.RecordRepairIteration(RepairIteration{Scope: scope, Attempt: i, Succeeded: i%3 == 0, At: testDay})
				tr.RecordTestRun(TestRun{Scope: scope, Passed: 1, Failed: i % 2, Duration: time.Millisecond, At: testDay})
				tr.RecordMessage(MessageEvent{Scope: scope, At: testDay})
				tr.RecordAgentSpawned(AgentEvent{Scope: scope, At: testDay})
				tr.RecordAgentCompleted(AgentEvent{Scope: scope, At: testDay})
			}
		}(w)
	}
	// Concurrent readers, to catch races between Snapshot and the writers.
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range rounds {
				_ = tr.Snapshot()
				_, _ = tr.SessionStats("s1")
				_ = tr.Totals()
			}
		}()
	}
	wg.Wait()

	const total = workers * rounds
	got := tr.Totals().Counters
	want := Counters{
		Messages:         total,
		ModelCalls:       2 * total,
		ToolCalls:        total,
		Commands:         total,
		CommandFailures:  total / 2,
		RepairIterations: total,
		TestRuns:         total,
		TestRunsFailed:   total / 2,
		TestsPassed:      total,
		TestsFailed:      total / 2,
		AgentsSpawned:    total,
		AgentsCompleted:  total,
	}
	want.RepairSuccesses = got.RepairSuccesses // depends on the round pattern only
	if got != want {
		t.Fatalf("totals = %+v, want %+v", got, want)
	}
	if got.RepairSuccesses == 0 || got.RepairSuccesses >= total {
		t.Fatalf("repair successes = %d", got.RepairSuccesses)
	}
	if tokens := tr.Totals().Tokens; tokens.Measured.Total != 15*total || tokens.Estimated.Total != 150*total {
		t.Fatalf("tokens = %+v", tokens)
	}
	if d := tr.Totals().Durations.Model.Duration(); d != time.Duration(total)*time.Millisecond {
		t.Fatalf("model duration = %v", d)
	}

	snap := tr.Snapshot()
	if len(snap.Sessions) != len(sessions) || len(snap.Agents) != workers || len(snap.Providers) != len(providers) {
		t.Fatalf("dimension cardinality wrong: %d/%d/%d", len(snap.Sessions), len(snap.Agents), len(snap.Providers))
	}
	var sum int
	for _, a := range snap.Sessions {
		sum += a.Counters.Messages
	}
	if sum != total {
		t.Fatalf("session messages sum = %d, want %d", sum, total)
	}
	if snap.Totals.Cost.UnpricedCalls == 0 || snap.Totals.Cost.FreeCalls == 0 || snap.Totals.Cost.Measured == 0 {
		t.Fatalf("cost totals look wrong: %+v", snap.Totals.Cost)
	}
}

func TestAggregateMerge(t *testing.T) {
	t.Parallel()
	early := testDay
	late := testDay.Add(2 * time.Hour)

	a := Aggregate{
		FirstSeen: late,
		LastSeen:  late,
		Counters:  Counters{Messages: 1, ToolCalls: 2},
		Durations: Durations{Model: Duration(time.Second)},
		Cost:      CostTotals{Measured: 1, UnpricedCalls: 1},
	}
	a.Tokens.Add(SourceMeasured, Tokens{Prompt: 5, Total: 5})

	b := Aggregate{
		FirstSeen: early,
		LastSeen:  early,
		Counters:  Counters{Messages: 2, AgentsSpawned: 1},
		Durations: Durations{Model: Duration(time.Second), Test: Duration(time.Minute)},
		Cost:      CostTotals{Currency: "USD", Estimated: 2, FreeCalls: 3},
	}
	b.Tokens.Add(SourceEstimated, Tokens{Prompt: 7, Total: 7})

	a.Merge(b)

	if a.FirstSeen != early || a.LastSeen != late {
		t.Fatalf("merged window = %v..%v", a.FirstSeen, a.LastSeen)
	}
	if a.Counters.Messages != 3 || a.Counters.ToolCalls != 2 || a.Counters.AgentsSpawned != 1 {
		t.Fatalf("counters = %+v", a.Counters)
	}
	if a.Durations.Model.Duration() != 2*time.Second || a.Durations.Test.Duration() != time.Minute {
		t.Fatalf("durations = %+v", a.Durations)
	}
	if a.Tokens.Measured.Total != 5 || a.Tokens.Estimated.Total != 7 {
		t.Fatalf("tokens = %+v", a.Tokens)
	}
	if a.Cost.Currency != "USD" || a.Cost.Measured != 1 || a.Cost.Estimated != 2 || a.Cost.FreeCalls != 3 || a.Cost.UnpricedCalls != 1 {
		t.Fatalf("cost = %+v", a.Cost)
	}

	// Merging into a zero aggregate adopts the other's window.
	var zero Aggregate
	zero.Merge(b)
	if zero.FirstSeen != early || zero.LastSeen != early {
		t.Fatalf("zero merge window = %v..%v", zero.FirstSeen, zero.LastSeen)
	}
}

func TestDurationJSON(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"zero", 0, "0"},
		{"one millisecond", time.Millisecond, "1"},
		{"sub-millisecond", 1500 * time.Nanosecond, "0.0015"},
		{"seconds", 2500 * time.Millisecond, "2500"},
		{"negative", -time.Millisecond, "-1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(Duration(tc.d))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(raw) != tc.want {
				t.Fatalf("marshal = %s, want %s", raw, tc.want)
			}
			var back Duration
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if back.Duration() != tc.d {
				t.Fatalf("round trip = %v, want %v", back.Duration(), tc.d)
			}
		})
	}

	var bad Duration
	if err := json.Unmarshal([]byte(`"nope"`), &bad); err == nil {
		t.Fatal("expected an error for a non-numeric duration")
	}
	if got := Duration(1500 * time.Millisecond).String(); got != "1.5s" {
		t.Fatalf("String() = %q", got)
	}
}

func TestSnapshotJSONShape(t *testing.T) {
	t.Parallel()
	tr := newTestTracker(t)
	tr.RecordModelCall(ModelCall{
		Scope: Scope{SessionID: "s", AgentID: "a", Provider: "anthropic", Model: "claude-opus-5"},
		Usage: provider.Usage{PromptTokens: 100, CompletionTokens: 20},
		At:    testDay,
	})

	raw, err := json.Marshal(tr.Snapshot())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"generated_at", "started_at", "uptime_ms", "estimator", "currency", "totals", "sessions", "agents", "providers", "models", "days", "contexts"} {
		if _, ok := generic[key]; !ok {
			t.Fatalf("snapshot JSON missing %q: %s", key, raw)
		}
	}
	totals, ok := generic["totals"].(map[string]any)
	if !ok {
		t.Fatalf("totals is not an object: %s", raw)
	}
	tokens, ok := totals["tokens"].(map[string]any)
	if !ok {
		t.Fatalf("tokens is not an object: %s", raw)
	}
	if _, ok := tokens["measured"]; !ok {
		t.Fatal("tokens.measured missing from JSON")
	}
	if _, ok := tokens["estimated"]; !ok {
		t.Fatal("tokens.estimated missing from JSON")
	}
	if !strings.Contains(string(raw), `"anthropic/claude-opus-5"`) {
		t.Fatalf("model dimension key missing: %s", raw)
	}
}

func TestModelDimensionKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		prov, model, want string
	}{
		{"anthropic", "claude-opus-5", "anthropic/claude-opus-5"},
		{"", "claude-opus-5", "claude-opus-5"},
		{"anthropic", "", ""},
		{"", "", ""},
	}
	for _, tc := range tests {
		if got := modelDimension(tc.prov, tc.model); got != tc.want {
			t.Fatalf("modelDimension(%q, %q) = %q, want %q", tc.prov, tc.model, got, tc.want)
		}
	}

	tr := newTestTracker(t)
	tr.RecordModelCall(ModelCall{Scope: Scope{Model: "bare-model"}, At: testDay})
	if _, ok := tr.ModelStats("", "bare-model"); !ok {
		t.Fatal("provider-less model was not aggregated")
	}
}

func TestRecordModelCallWithFallbackUsesRequestModel(t *testing.T) {
	t.Parallel()
	stub := &stubEstimator{prompt: 11, completion: 7}
	tr := newTestTracker(t, WithEstimator(stub))

	req := provider.ChatRequest{Model: "claude-opus-5"}
	// Scope carries no model; the request's model must be used for both the
	// estimate and, via the scope, left untouched for aggregation.
	src := tr.RecordModelCallWithFallback(ModelCall{Scope: Scope{SessionID: "s"}, At: testDay}, req, "hi")
	if src != SourceEstimated {
		t.Fatalf("source = %q", src)
	}
	a, _ := tr.SessionStats("s")
	if a.Tokens.Estimated.Prompt != 11 || a.Tokens.Estimated.Completion != 7 {
		t.Fatalf("tokens = %+v", a.Tokens.Estimated)
	}
	if a.Cost.UnpricedCalls != 1 {
		t.Fatalf("a model-less scope cannot be priced: %+v", a.Cost)
	}
}

package stats

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/boop-dev/boop/internal/provider"
)

func TestTokensFromUsage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		usage provider.Usage
		want  Tokens
	}{
		{
			name:  "total derived when the provider omits it",
			usage: provider.Usage{PromptTokens: 10, CompletionTokens: 5},
			want:  Tokens{Prompt: 10, Completion: 5, Total: 15},
		},
		{
			name:  "reported total is preserved even when it disagrees",
			usage: provider.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 99},
			want:  Tokens{Prompt: 10, Completion: 5, Total: 99},
		},
		{
			name:  "cached tokens are a subset of prompt tokens",
			usage: provider.Usage{PromptTokens: 10, CachedTokens: 40},
			want:  Tokens{Prompt: 10, Completion: 0, Total: 10, Cached: 10},
		},
		{
			name:  "negative counts are clamped",
			usage: provider.Usage{PromptTokens: -5, CompletionTokens: -1, TotalTokens: -9},
			want:  Tokens{},
		},
		{
			name:  "zero usage stays zero",
			usage: provider.Usage{},
			want:  Tokens{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := TokensFromUsage(tc.usage); got != tc.want {
				t.Fatalf("TokensFromUsage() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestUsageReported(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		usage provider.Usage
		want  bool
	}{
		{"empty usage is not a measurement of zero", provider.Usage{}, false},
		{"prompt only", provider.Usage{PromptTokens: 1}, true},
		{"completion only", provider.Usage{CompletionTokens: 1}, true},
		{"total only", provider.Usage{TotalTokens: 1}, true},
		{"cached alone is not enough", provider.Usage{CachedTokens: 3}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := UsageReported(tc.usage); got != tc.want {
				t.Fatalf("UsageReported() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTokenCountsKeepProvenanceSeparate(t *testing.T) {
	t.Parallel()
	var c TokenCounts
	if !c.Exact() || !c.IsZero() {
		t.Fatal("zero TokenCounts should be exact and empty")
	}

	c.Add(SourceMeasured, Tokens{Prompt: 10, Completion: 2, Total: 12})
	if !c.Exact() {
		t.Fatal("measured-only counts must be exact")
	}
	c.Add(SourceEstimated, Tokens{Prompt: 100, Completion: 20, Total: 120})
	if c.Exact() {
		t.Fatal("counts containing an estimate must not claim exactness")
	}
	if c.Measured.Total != 12 || c.Estimated.Total != 120 {
		t.Fatalf("buckets bled into each other: %+v", c)
	}
	if got := c.Approximate(); got.Total != 132 {
		t.Fatalf("Approximate().Total = %d, want 132", got.Total)
	}

	// An unknown source must be treated as an estimate, never a measurement.
	var d TokenCounts
	d.Add(TokenSource("nonsense"), Tokens{Total: 5})
	if d.Measured.Total != 0 || d.Estimated.Total != 5 {
		t.Fatalf("unknown source was not downgraded to estimated: %+v", d)
	}
}

func TestHeuristicEstimator(t *testing.T) {
	t.Parallel()
	est := HeuristicEstimator{}

	if est.Name() == "" {
		t.Fatal("estimator must name itself")
	}
	if got := est.EstimateText("m", ""); got != 0 {
		t.Fatalf("empty text = %d, want 0", got)
	}
	if got := est.EstimateText("m", "a"); got != 1 {
		t.Fatalf("single char = %d, want 1 (never round down to zero)", got)
	}
	if got := est.EstimateText("m", strings.Repeat("a", 400)); got != 100 {
		t.Fatalf("400 chars = %d, want 100", got)
	}

	// Multi-byte text is counted by rune, not by byte.
	multi := strings.Repeat("æ", 400)
	if got := est.EstimateText("m", multi); got != 100 {
		t.Fatalf("400 runes = %d, want 100", got)
	}

	tuned := HeuristicEstimator{CharsPerToken: 2, PerMessageTokens: 1, ReplyPrimingTokens: 1, PerToolTokens: 1, NonTextPartTokens: 7}
	if got := tuned.EstimateText("m", strings.Repeat("a", 10)); got != 5 {
		t.Fatalf("tuned ratio = %d, want 5", got)
	}

	req := provider.ChatRequest{
		Model: "m",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: strings.Repeat("a", 8)},
			{Role: provider.RoleUser, Parts: []provider.ContentPart{
				{Kind: provider.PartText, Text: strings.Repeat("a", 4)},
				{Kind: provider.PartImage, MIMEType: "image/png"},
			}},
		},
	}
	// priming 1 + (1 + 4) + (1 + 2 + 7) = 16
	if got := tuned.EstimateRequest("m", req); got != 16 {
		t.Fatalf("EstimateRequest() = %d, want 16", got)
	}

	withTools := req
	withTools.Tools = []provider.ToolDefinition{{
		Name:        "run",
		Description: "",
		Schema:      map[string]any{"type": "object"},
	}}
	if tuned.EstimateRequest("m", withTools) <= 16 {
		t.Fatal("tool declarations must add to the prompt estimate")
	}
}

func TestEstimateUsage(t *testing.T) {
	t.Parallel()
	req := provider.ChatRequest{Model: "m", Messages: []provider.Message{{Content: strings.Repeat("a", 40)}}}

	got := EstimateUsage(nil, "m", req, strings.Repeat("b", 40))
	if got.PromptTokens == 0 || got.CompletionTokens == 0 {
		t.Fatalf("nil estimator should fall back to the default: %+v", got)
	}
	if got.TotalTokens != got.PromptTokens+got.CompletionTokens {
		t.Fatalf("total mismatch: %+v", got)
	}
}

func TestContextUtilisation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		tokens    int
		window    int
		wantFrac  float64
		wantKnown bool
	}{
		{"unknown window", 1000, 0, 0, false},
		{"negative window is unknown", 1000, -1, 0, false},
		{"half full", 512, 1024, 0.5, true},
		{"empty context", 0, 1024, 0, true},
		{"over the limit is reported as over", 2048, 1024, 2, true},
		{"negative tokens clamp to zero", -5, 1024, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			frac, known := ContextUtilisation(tc.tokens, tc.window)
			if known != tc.wantKnown {
				t.Fatalf("known = %v, want %v", known, tc.wantKnown)
			}
			if math.Abs(frac-tc.wantFrac) > 1e-9 {
				t.Fatalf("fraction = %v, want %v", frac, tc.wantFrac)
			}
		})
	}
}

func TestContextTracker(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	ct := NewContextTracker()

	if _, ok := ct.Usage("nope"); ok {
		t.Fatal("unknown session reported usage")
	}

	// Observed before any window is known: utilisation must stay unknown.
	ct.Observe("s1", "ollama", "llama3.2", 4000, SourceEstimated, at)
	u, ok := ct.Usage("s1")
	if !ok {
		t.Fatal("session usage missing")
	}
	if u.WindowKnown || u.ContextWindow != 0 || u.Utilisation != 0 || u.Remaining != 0 {
		t.Fatalf("unknown window must not fabricate utilisation: %+v", u)
	}
	if !u.Estimated {
		t.Fatal("estimated occupancy lost its provenance")
	}
	if u.Exceeds(0.0) {
		t.Fatal("Exceeds must be false while the window is unknown")
	}

	// Registering the window afterwards fixes up the reading.
	ct.RegisterModels(
		provider.Model{Provider: "ollama", ID: "llama3.2", ContextWindow: 8000},
		provider.Model{Provider: "ollama", ID: "no-window"},
	)
	if _, known := ct.Window("ollama", "no-window"); known {
		t.Fatal("a model advertising no window must not be registered as zero")
	}
	u, _ = ct.Usage("s1")
	if !u.WindowKnown || u.ContextWindow != 8000 {
		t.Fatalf("window not applied: %+v", u)
	}
	if math.Abs(u.Utilisation-0.5) > 1e-9 || u.Remaining != 4000 {
		t.Fatalf("utilisation = %v remaining = %d", u.Utilisation, u.Remaining)
	}
	if !u.Exceeds(0.5) || u.Exceeds(0.9) {
		t.Fatalf("Exceeds threshold behaviour wrong for %+v", u)
	}

	// Over-full contexts are reported as over 100%, not clamped.
	ct.Observe("s1", "ollama", "llama3.2", 9000, SourceMeasured, at)
	u, _ = ct.Usage("s1")
	if u.Utilisation <= 1 || u.Remaining != 0 || u.Estimated {
		t.Fatalf("over-full context: %+v", u)
	}

	// A window set to zero removes the entry rather than recording zero.
	ct.SetWindow("ollama", "llama3.2", 0)
	if _, known := ct.Window("ollama", "llama3.2"); known {
		t.Fatal("zero window should delete the entry")
	}

	ct.Observe("", "p", "m", 10, SourceMeasured, at)
	if len(ct.Snapshot()) != 1 {
		t.Fatalf("empty session id should be ignored: %+v", ct.Snapshot())
	}

	ct.Forget("s1")
	if _, ok := ct.Usage("s1"); ok {
		t.Fatal("Forget did not drop the session")
	}

	ct.SetWindow("p", "m", 100)
	ct.Observe("s2", "p", "m", 10, SourceMeasured, at)
	ct.Reset()
	if len(ct.Snapshot()) != 0 {
		t.Fatal("Reset did not clear occupancy")
	}
	if _, known := ct.Window("p", "m"); !known {
		t.Fatal("Reset must keep registered windows")
	}
}

func TestContextUsageJSON(t *testing.T) {
	t.Parallel()
	ct := NewContextTracker()
	ct.SetWindow("anthropic", "claude-opus-5", 1_000_000)
	ct.Observe("s1", "anthropic", "claude-opus-5", 250_000, SourceMeasured, time.Now())
	u, _ := ct.Usage("s1")

	raw, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back ContextUsage
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Utilisation != 0.25 || !back.WindowKnown || back.Estimated {
		t.Fatalf("round-trip lost information: %+v", back)
	}
}

func TestHeuristicEstimatorToolCallsAndOverrides(t *testing.T) {
	t.Parallel()
	est := HeuristicEstimator{CharsPerToken: 4, PerMessageTokens: 1, PerToolTokens: 3, NonTextPartTokens: 5}

	msgs := []provider.Message{{
		Role: provider.RoleAssistant,
		Name: "tool-user",
		ToolCalls: []provider.ToolCall{{
			ID:        "1",
			Name:      "run",
			Arguments: `{"command":"ls"}`,
		}},
	}}
	// message 1 + name ceil(9/4)=3 + toolcall (1 + ceil(3/4)=1 + ceil(16/4)=4)
	if got := est.EstimateMessages("m", msgs); got != 10 {
		t.Fatalf("EstimateMessages() = %d, want 10", got)
	}

	tools := []provider.ToolDefinition{{Name: "run", Description: "runs"}}
	// per-tool 3 + ceil(3/4)=1 + ceil(4/4)=1
	if got := est.EstimateTools("m", tools); got != 5 {
		t.Fatalf("EstimateTools() = %d, want 5", got)
	}
	// A schema that cannot be marshalled must not panic or corrupt the count.
	bad := []provider.ToolDefinition{{Name: "run", Schema: map[string]any{"x": make(chan int)}}}
	if got := est.EstimateTools("m", bad); got != 4 {
		t.Fatalf("unmarshalable schema = %d, want 4", got)
	}
}

func TestContextTrackerClampsNegativeTokens(t *testing.T) {
	t.Parallel()
	ct := NewContextTracker()
	ct.SetWindow("p", "m", 100)
	ct.Observe("s", "p", "m", -50, SourceMeasured, time.Now())
	u, ok := ct.Usage("s")
	if !ok || u.Tokens != 0 || u.Remaining != 100 || u.Utilisation != 0 {
		t.Fatalf("negative occupancy = %+v", u)
	}
}

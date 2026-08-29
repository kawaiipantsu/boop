package stats

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/provider"
)

func almostEqual(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestRateTableCost(t *testing.T) {
	t.Parallel()

	custom := NewRateTable()
	custom.Set(
		Rate{Provider: "openai", Model: "fake-model", InputPerMillion: 1, OutputPerMillion: 2},
		Rate{Model: "gateway-model", InputPerMillion: 10, OutputPerMillion: 20},
		Rate{Provider: "openai", Model: "cached-model", InputPerMillion: 10, OutputPerMillion: 20, CachedInputPerMillion: 1},
		Rate{Provider: "anthropic", Model: "claude-opus-5", InputPerMillion: 1, OutputPerMillion: 1, Source: "user override"},
	)

	tests := []struct {
		name      string
		table     *RateTable
		provider  string
		model     string
		usage     provider.Usage
		wantOK    bool
		wantLocal bool
		wantIn    float64
		wantOut   float64
	}{
		{
			name:     "bundled anthropic rate",
			table:    NewRateTable(),
			provider: "anthropic",
			model:    "claude-opus-5",
			usage:    provider.Usage{PromptTokens: 1_000_000, CompletionTokens: 100_000},
			wantOK:   true,
			wantIn:   5,
			wantOut:  2.5,
		},
		{
			name:     "model id matching is case insensitive",
			table:    NewRateTable(),
			provider: "Anthropic",
			model:    "Claude-Sonnet-5",
			usage:    provider.Usage{PromptTokens: 500_000, CompletionTokens: 500_000},
			wantOK:   true,
			wantIn:   1,
			wantOut:  5,
		},
		{
			name:     "unknown model declines rather than guessing",
			table:    NewRateTable(),
			provider: "openai",
			model:    "some-unreleased-model",
			usage:    provider.Usage{PromptTokens: 1_000_000, CompletionTokens: 1_000_000},
			wantOK:   false,
		},
		{
			name:     "unknown provider for a known model id declines",
			table:    NewRateTable(),
			provider: "mystery-cloud",
			model:    "claude-opus-5",
			usage:    provider.Usage{PromptTokens: 1_000_000},
			wantOK:   false,
		},
		{
			name:      "lemonade is free",
			table:     NewRateTable(),
			provider:  ProviderLemonade,
			model:     "Llama-3.1-8B",
			usage:     provider.Usage{PromptTokens: 9_000_000, CompletionTokens: 9_000_000},
			wantOK:    true,
			wantLocal: true,
		},
		{
			name:      "lmstudio is free",
			table:     NewRateTable(),
			provider:  ProviderLMStudio,
			model:     "qwen3-coder",
			usage:     provider.Usage{PromptTokens: 1_000_000},
			wantOK:    true,
			wantLocal: true,
		},
		{
			name:      "ollama is free",
			table:     NewRateTable(),
			provider:  ProviderOllama,
			model:     "llama3.2",
			usage:     provider.Usage{PromptTokens: 1_000_000},
			wantOK:    true,
			wantLocal: true,
		},
		{
			name:      "local rule survives an empty table",
			table:     NewEmptyRateTable(),
			provider:  ProviderOllama,
			model:     "llama3.2",
			usage:     provider.Usage{PromptTokens: 1_000_000},
			wantOK:    true,
			wantLocal: true,
		},
		{
			name:     "empty table prices nothing else",
			table:    NewEmptyRateTable(),
			provider: "anthropic",
			model:    "claude-opus-5",
			usage:    provider.Usage{PromptTokens: 1_000_000},
			wantOK:   false,
		},
		{
			name:     "user override wins over bundled rate",
			table:    custom,
			provider: "anthropic",
			model:    "claude-opus-5",
			usage:    provider.Usage{PromptTokens: 1_000_000, CompletionTokens: 1_000_000},
			wantOK:   true,
			wantIn:   1,
			wantOut:  1,
		},
		{
			name:     "wildcard entry matches any provider",
			table:    custom,
			provider: "some-gateway",
			model:    "gateway-model",
			usage:    provider.Usage{PromptTokens: 2_000_000, CompletionTokens: 1_000_000},
			wantOK:   true,
			wantIn:   20,
			wantOut:  20,
		},
		{
			name:     "cached prompt tokens use the cached rate",
			table:    custom,
			provider: "openai",
			model:    "cached-model",
			usage:    provider.Usage{PromptTokens: 1_000_000, CachedTokens: 900_000, CompletionTokens: 0},
			wantOK:   true,
			wantIn:   0.1*10 + 0.9*1,
			wantOut:  0,
		},
		{
			name:     "cached tokens fall back to the input rate when unpriced",
			table:    custom,
			provider: "openai",
			model:    "fake-model",
			usage:    provider.Usage{PromptTokens: 1_000_000, CachedTokens: 1_000_000},
			wantOK:   true,
			wantIn:   1,
			wantOut:  0,
		},
		{
			name:     "zero usage costs zero but is still priced",
			table:    NewRateTable(),
			provider: "anthropic",
			model:    "claude-haiku-4-5",
			usage:    provider.Usage{},
			wantOK:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.table.Cost(tc.usage, tc.provider, tc.model)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (cost %+v)", ok, tc.wantOK, got)
			}
			if got.Local != tc.wantLocal {
				t.Fatalf("Local = %v, want %v", got.Local, tc.wantLocal)
			}
			if !ok {
				if got.Total != 0 {
					t.Fatalf("unpriced cost must stay zero-valued, got %+v", got)
				}
				return
			}
			almostEqual(t, got.Input, tc.wantIn)
			almostEqual(t, got.Output, tc.wantOut)
			almostEqual(t, got.Total, tc.wantIn+tc.wantOut)
			if got.Currency != DefaultCurrency {
				t.Fatalf("currency = %q, want %q", got.Currency, DefaultCurrency)
			}
		})
	}
}

func TestRateTableMutation(t *testing.T) {
	t.Parallel()
	tbl := NewRateTable()

	if _, ok := tbl.Lookup("anthropic", "claude-opus-5"); !ok {
		t.Fatal("expected bundled rate for claude-opus-5")
	}
	if !tbl.Remove("anthropic", "claude-opus-5") {
		t.Fatal("Remove reported no entry")
	}
	if tbl.Remove("anthropic", "claude-opus-5") {
		t.Fatal("Remove of a missing entry reported success")
	}
	if _, ok := tbl.Lookup("anthropic", "claude-opus-5"); ok {
		t.Fatal("rate survived removal")
	}

	tbl.Set(Rate{Model: "   "}, Rate{Provider: "x", Model: "y", InputPerMillion: 1})
	for _, r := range tbl.Rates() {
		if r.Model == "" {
			t.Fatal("empty model was stored")
		}
	}

	rates := tbl.Rates()
	for i := 1; i < len(rates); i++ {
		prev, cur := rates[i-1], rates[i]
		if prev.Provider > cur.Provider || (prev.Provider == cur.Provider && prev.Model > cur.Model) {
			t.Fatalf("Rates not sorted: %q/%q before %q/%q", prev.Provider, prev.Model, cur.Provider, cur.Model)
		}
	}
}

func TestRateTableLocalProviders(t *testing.T) {
	t.Parallel()
	tbl := NewRateTable()

	want := []string{ProviderLemonade, ProviderLMStudio, ProviderOllama}
	got := tbl.LocalProviders()
	if len(got) != len(want) {
		t.Fatalf("LocalProviders() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("LocalProviders() = %v, want %v", got, want)
		}
	}

	if tbl.IsLocal("my-rig") {
		t.Fatal("unregistered provider reported as local")
	}
	tbl.MarkLocal("My-Rig", "  ", "")
	if !tbl.IsLocal("my-rig") {
		t.Fatal("MarkLocal did not register the provider")
	}
	c, ok := tbl.Cost(provider.Usage{PromptTokens: 1_000_000}, "MY-RIG", "anything")
	if !ok || !c.Local || c.Total != 0 {
		t.Fatalf("custom local provider cost = %+v, ok = %v", c, ok)
	}
}

func TestRateTableCurrency(t *testing.T) {
	t.Parallel()
	tbl := NewRateTable()
	if tbl.Currency() != DefaultCurrency {
		t.Fatalf("default currency = %q", tbl.Currency())
	}
	tbl.SetCurrency("  ")
	if tbl.Currency() != DefaultCurrency {
		t.Fatalf("blank currency should fall back, got %q", tbl.Currency())
	}
	tbl.SetCurrency("EUR")
	c, _ := tbl.Cost(provider.Usage{PromptTokens: 1}, "anthropic", "claude-opus-5")
	if c.Currency != "EUR" {
		t.Fatalf("currency = %q, want EUR", c.Currency)
	}
}

func TestBuiltinRatesAreSane(t *testing.T) {
	t.Parallel()
	for _, r := range builtinRates() {
		if r.Provider == "" || r.Model == "" {
			t.Fatalf("bundled rate missing identity: %+v", r)
		}
		if r.InputPerMillion <= 0 || r.OutputPerMillion <= 0 {
			t.Fatalf("bundled rate must be positive: %+v", r)
		}
		if r.OutputPerMillion < r.InputPerMillion {
			t.Fatalf("output cheaper than input looks like a transposition: %+v", r)
		}
		if r.Source == "" {
			t.Fatalf("bundled rate must document its source: %+v", r)
		}
	}
}

func TestCostJSON(t *testing.T) {
	t.Parallel()
	tbl := NewRateTable()

	local, _ := tbl.Cost(provider.Usage{PromptTokens: 1000}, ProviderOllama, "llama3.2")
	raw, err := json.Marshal(local)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), `"rate"`) {
		t.Fatalf("free calls must not carry a rate: %s", raw)
	}
	if !strings.Contains(string(raw), `"local":true`) {
		t.Fatalf("local flag missing: %s", raw)
	}

	priced, _ := tbl.Cost(provider.Usage{PromptTokens: 1_000_000}, "anthropic", "claude-opus-5")
	raw, err = json.Marshal(priced)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"rate"`) {
		t.Fatalf("priced cost must expose the applied rate: %s", raw)
	}
	var back Cost
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Rate.Model != "claude-opus-5" || back.Total != priced.Total {
		t.Fatalf("round trip = %+v", back)
	}
}

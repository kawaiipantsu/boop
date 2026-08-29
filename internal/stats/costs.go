package stats

import (
	"sort"
	"strings"
	"sync"

	"github.com/boop-dev/boop/internal/provider"
)

// DefaultCurrency is the currency the bundled rates are expressed in.
const DefaultCurrency = "USD"

// Local provider names that never cost anything to call. Running models on
// your own hardware for free is the point of Boop's local-first stance, so
// these are a rule of the product rather than a pricing entry.
const (
	ProviderLemonade = "lemonade"
	ProviderLMStudio = "lmstudio"
	ProviderOllama   = "ollama"
)

// Rate is the price of one model, expressed per million tokens.
//
// An empty Provider makes the entry a wildcard that matches the model ID under
// any provider, which is how a user prices a model reached through a gateway.
type Rate struct {
	Provider string `json:"provider,omitempty" yaml:"provider,omitempty"`
	Model    string `json:"model" yaml:"model"`
	// InputPerMillion prices prompt tokens.
	InputPerMillion float64 `json:"input_per_million" yaml:"input_per_million"`
	// OutputPerMillion prices completion tokens.
	OutputPerMillion float64 `json:"output_per_million" yaml:"output_per_million"`
	// CachedInputPerMillion prices the prompt tokens a provider served from
	// its own cache. Zero means "not priced separately": cached tokens are
	// then billed at InputPerMillion, which over-states cost for providers
	// that discount cache reads.
	CachedInputPerMillion float64 `json:"cached_input_per_million,omitempty" yaml:"cached_input_per_million,omitempty"`
	// Source documents where the numbers came from, so a stale entry can be
	// traced back and re-checked.
	Source string `json:"source,omitempty" yaml:"source,omitempty"`
}

// Cost is the money attributable to one exchange.
type Cost struct {
	Provider string  `json:"provider"`
	Model    string  `json:"model"`
	Currency string  `json:"currency"`
	Input    float64 `json:"input"`
	Output   float64 `json:"output"`
	Total    float64 `json:"total"`
	// Local reports that the zero cost comes from the local-provider rule
	// rather than from a rate that happens to be zero.
	Local bool `json:"local"`
	// Rate is the entry that was applied; zero-valued for local providers.
	Rate Rate `json:"rate,omitzero"`
}

// builtinRates is a small convenience table, not authoritative pricing.
//
// Vendor prices change without notice and this binary does not phone home, so
// treat every number here as possibly stale and never as a bill. It covers
// only models whose public list price is documented and well known; anything
// else must be supplied by the user through RateTable.Set. Amazon Bedrock,
// Google Vertex AI and other resellers charge their own rates and are
// deliberately absent.
//
// Source: Anthropic first-party API list prices, USD per million tokens,
// recorded 2026-06-24.
func builtinRates() []Rate {
	const src = "anthropic api list price, recorded 2026-06-24"
	return []Rate{
		{Provider: "anthropic", Model: "claude-fable-5", InputPerMillion: 10, OutputPerMillion: 50, Source: src},
		{Provider: "anthropic", Model: "claude-opus-5", InputPerMillion: 5, OutputPerMillion: 25, Source: src},
		{Provider: "anthropic", Model: "claude-opus-4-8", InputPerMillion: 5, OutputPerMillion: 25, Source: src},
		{Provider: "anthropic", Model: "claude-opus-4-7", InputPerMillion: 5, OutputPerMillion: 25, Source: src},
		{Provider: "anthropic", Model: "claude-opus-4-6", InputPerMillion: 5, OutputPerMillion: 25, Source: src},
		{Provider: "anthropic", Model: "claude-sonnet-5", InputPerMillion: 2, OutputPerMillion: 10, Source: src},
		{Provider: "anthropic", Model: "claude-sonnet-4-6", InputPerMillion: 3, OutputPerMillion: 15, Source: src},
		{Provider: "anthropic", Model: "claude-haiku-4-5", InputPerMillion: 1, OutputPerMillion: 5, Source: src},
	}
}

// RateTable maps provider and model to a price. It is safe for concurrent use.
//
// Lookups are case-insensitive and try the exact provider/model pair first,
// then a wildcard entry for the model alone. A miss is reported as a miss:
// pricing a model we do not know about would be guessing at money.
type RateTable struct {
	mu       sync.RWMutex
	currency string
	rates    map[string]Rate
	local    map[string]struct{}
}

// NewRateTable returns a table preloaded with the bundled convenience rates
// and the known-local providers.
func NewRateTable() *RateTable {
	t := NewEmptyRateTable()
	t.Set(builtinRates()...)
	return t
}

// NewEmptyRateTable returns a table with no prices at all.
//
// The local-provider rule still applies: local inference is free regardless of
// what any table says, so callers that replace the bundled rates wholesale do
// not accidentally start charging for Ollama.
func NewEmptyRateTable() *RateTable {
	return &RateTable{
		currency: DefaultCurrency,
		rates:    make(map[string]Rate),
		local: map[string]struct{}{
			ProviderLemonade: {},
			ProviderLMStudio: {},
			ProviderOllama:   {},
		},
	}
}

func rateKey(prov, model string) string {
	return strings.ToLower(strings.TrimSpace(prov)) + "\x00" + strings.ToLower(strings.TrimSpace(model))
}

// Currency returns the currency all rates in the table are expressed in.
func (t *RateTable) Currency() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.currency
}

// SetCurrency overrides the currency label. It converts nothing; it only
// changes how the numbers are reported.
func (t *RateTable) SetCurrency(code string) {
	code = strings.TrimSpace(code)
	if code == "" {
		code = DefaultCurrency
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.currency = code
}

// Set inserts or replaces rates. This is the user-override mechanism: a rate
// loaded from configuration wins over a bundled one with the same key.
// Entries with an empty Model are ignored.
func (t *RateTable) Set(rates ...Rate) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, r := range rates {
		if strings.TrimSpace(r.Model) == "" {
			continue
		}
		t.rates[rateKey(r.Provider, r.Model)] = r
	}
}

// Remove deletes a rate. It reports whether an entry was present.
func (t *RateTable) Remove(prov, model string) bool {
	key := rateKey(prov, model)
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.rates[key]
	delete(t.rates, key)
	return ok
}

// Lookup returns the rate for a model. ok is false when the model is unpriced.
func (t *RateTable) Lookup(prov, model string) (Rate, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if r, ok := t.rates[rateKey(prov, model)]; ok {
		return r, true
	}
	// Wildcard: an entry stored with no provider prices the model everywhere.
	r, ok := t.rates[rateKey("", model)]
	return r, ok
}

// Rates returns every entry, sorted by provider then model, for display and
// for round-tripping a user's overrides back to configuration.
func (t *RateTable) Rates() []Rate {
	t.mu.RLock()
	out := make([]Rate, 0, len(t.rates))
	for _, r := range t.rates {
		out = append(out, r)
	}
	t.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// MarkLocal registers additional providers as cost-free, for users running
// their own OpenAI-compatible endpoint under a custom name.
func (t *RateTable) MarkLocal(names ...string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if n != "" {
			t.local[n] = struct{}{}
		}
	}
}

// IsLocal reports whether calls to a provider are free by rule.
func (t *RateTable) IsLocal(prov string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	_, ok := t.local[strings.ToLower(strings.TrimSpace(prov))]
	return ok
}

// LocalProviders returns the sorted set of cost-free providers.
func (t *RateTable) LocalProviders() []string {
	t.mu.RLock()
	out := make([]string, 0, len(t.local))
	for n := range t.local {
		out = append(out, n)
	}
	t.mu.RUnlock()
	sort.Strings(out)
	return out
}

// Cost prices one exchange.
//
// ok is false when the model has no rate and the provider is not local: the
// cost is then unknown, which callers must render as "unknown" rather than as
// zero. Reporting a confident wrong number about money is worse than declining
// to report one.
//
// Cached prompt tokens are billed at Rate.CachedInputPerMillion when it is set
// and at the ordinary input price otherwise.
func (t *RateTable) Cost(u provider.Usage, prov, model string) (Cost, bool) {
	tokens := TokensFromUsage(u)
	currency := t.Currency()

	if t.IsLocal(prov) {
		return Cost{
			Provider: prov,
			Model:    model,
			Currency: currency,
			Local:    true,
		}, true
	}

	rate, ok := t.Lookup(prov, model)
	if !ok {
		return Cost{Provider: prov, Model: model, Currency: currency}, false
	}

	billedPrompt := tokens.Prompt
	var input float64
	if rate.CachedInputPerMillion > 0 && tokens.Cached > 0 {
		billedPrompt -= tokens.Cached
		input += perMillion(tokens.Cached, rate.CachedInputPerMillion)
	}
	input += perMillion(billedPrompt, rate.InputPerMillion)
	output := perMillion(tokens.Completion, rate.OutputPerMillion)

	return Cost{
		Provider: prov,
		Model:    model,
		Currency: currency,
		Input:    input,
		Output:   output,
		Total:    input + output,
		Rate:     rate,
	}, true
}

func perMillion(tokens int, price float64) float64 {
	if tokens <= 0 || price == 0 {
		return 0
	}
	return float64(tokens) / 1_000_000 * price
}

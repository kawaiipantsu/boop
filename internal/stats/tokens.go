package stats

import (
	"encoding/json"
	"math"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/boop-dev/boop/internal/provider"
)

// TokenSource records where a token count came from.
//
// The distinction is load-bearing: Boop must never present a locally derived
// guess as if the provider had reported it. Every token count in this package
// is filed under one of these two sources and stays separated all the way into
// the JSON snapshot.
type TokenSource string

const (
	// SourceMeasured marks counts reported by the provider itself.
	SourceMeasured TokenSource = "measured"
	// SourceEstimated marks counts produced locally by an Estimator because
	// the provider reported no usage.
	SourceEstimated TokenSource = "estimated"
)

// Tokens is a raw token count for one accounting direction.
type Tokens struct {
	Prompt     int `json:"prompt"`
	Completion int `json:"completion"`
	Total      int `json:"total"`
	// Cached is the portion of Prompt served from a provider-side cache. It is
	// a subset of Prompt, not an addition to it.
	Cached int `json:"cached,omitempty"`
}

// IsZero reports whether nothing has been counted.
func (t Tokens) IsZero() bool { return t == Tokens{} }

// Add accumulates o into t.
func (t *Tokens) Add(o Tokens) {
	t.Prompt += o.Prompt
	t.Completion += o.Completion
	t.Total += o.Total
	t.Cached += o.Cached
}

// TokensFromUsage converts a provider.Usage into Tokens, deriving Total when
// the provider left it at zero. Negative fields are clamped to zero because a
// negative token count is always an adapter bug and must not corrupt totals.
func TokensFromUsage(u provider.Usage) Tokens {
	t := Tokens{
		Prompt:     max(0, u.PromptTokens),
		Completion: max(0, u.CompletionTokens),
		Total:      max(0, u.TotalTokens),
		Cached:     max(0, u.CachedTokens),
	}
	if t.Total == 0 {
		t.Total = t.Prompt + t.Completion
	}
	if t.Cached > t.Prompt {
		t.Cached = t.Prompt
	}
	return t
}

// UsageReported reports whether a provider actually returned token accounting.
//
// Adapters that cannot report usage leave the struct zero-valued, and a zero
// usage is indistinguishable from "no tokens", so callers must treat it as
// unreported rather than as a measurement of zero.
func UsageReported(u provider.Usage) bool {
	return u.PromptTokens != 0 || u.CompletionTokens != 0 || u.TotalTokens != 0
}

// TokenCounts keeps measured and estimated tokens apart.
//
// The two fields are never merged internally. Callers that genuinely do not
// care about provenance can call Approximate, which is explicitly named so the
// loss of accuracy is visible at the call site.
type TokenCounts struct {
	// Measured holds counts reported by providers.
	Measured Tokens `json:"measured"`
	// Estimated holds counts produced by an Estimator. Display these with a
	// "~" or an equivalent marker; they are not measurements.
	Estimated Tokens `json:"estimated"`
}

// Add accumulates t into the bucket named by src. An unrecognised source is
// treated as estimated: over-claiming precision is the worse failure.
func (c *TokenCounts) Add(src TokenSource, t Tokens) {
	if src == SourceMeasured {
		c.Measured.Add(t)
		return
	}
	c.Estimated.Add(t)
}

// Exact reports whether every counted token was measured, i.e. whether the
// numbers may be shown without an approximation marker.
func (c TokenCounts) Exact() bool { return c.Estimated.IsZero() }

// Approximate sums measured and estimated tokens. The result is only as good
// as the estimator that contributed to it; check Exact before presenting it as
// fact.
func (c TokenCounts) Approximate() Tokens {
	t := c.Measured
	t.Add(c.Estimated)
	return t
}

// IsZero reports whether nothing at all has been counted.
func (c TokenCounts) IsZero() bool { return c.Measured.IsZero() && c.Estimated.IsZero() }

// Estimator approximates token counts for content a provider did not measure.
//
// Implementations must be safe for concurrent use. Boop ships a crude
// character-ratio heuristic; a deployment that cares about accuracy can plug in
// a real tokenizer without touching the rest of the stats package.
type Estimator interface {
	// Name identifies the estimator so a snapshot can state what produced its
	// estimated numbers.
	Name() string
	// EstimateText approximates the tokens in a single string. The model is
	// passed because tokenizers are model-specific; the default heuristic
	// ignores it.
	EstimateText(model, text string) int
	// EstimateRequest approximates the prompt tokens a request will occupy,
	// including message framing and tool schemas.
	EstimateRequest(model string, req provider.ChatRequest) int
}

// Heuristic defaults. These are deliberately crude; see HeuristicEstimator.
const (
	defaultCharsPerToken   = 4.0
	defaultPerMessage      = 4
	defaultPerTool         = 8
	defaultReplyPriming    = 3
	defaultNonTextPartCost = 800
)

// HeuristicEstimator approximates tokens from character counts.
//
// It assumes roughly four characters per token, which is a reasonable fit for
// English prose with common BPE vocabularies and a poor fit for code, CJK text
// and base64. It exists so the UI can show *something* for providers that omit
// usage, not so anyone can bill against it. The zero value is usable and
// applies the package defaults.
type HeuristicEstimator struct {
	// CharsPerToken is the assumed compression ratio; <= 0 selects 4.0.
	CharsPerToken float64
	// PerMessageTokens accounts for role and delimiter framing added around
	// every message by chat templates; <= 0 selects 4.
	PerMessageTokens int
	// PerToolTokens accounts for the framing around each tool declaration;
	// <= 0 selects 8.
	PerToolTokens int
	// ReplyPrimingTokens accounts for the tokens a template appends to prime
	// the assistant turn; <= 0 selects 3.
	ReplyPrimingTokens int
	// NonTextPartTokens is charged for each image or document part. Real cost
	// varies from a few hundred to several thousand tokens depending on
	// resolution and provider, so this is a flat placeholder: override it, or
	// supply a different Estimator, when the number matters.
	NonTextPartTokens int
}

// Compile-time proof that the zero value satisfies the interface.
var _ Estimator = HeuristicEstimator{}

// DefaultEstimator returns the built-in heuristic estimator.
func DefaultEstimator() Estimator { return HeuristicEstimator{} }

// Name implements Estimator.
func (h HeuristicEstimator) Name() string { return "heuristic/chars" }

func (h HeuristicEstimator) charsPerToken() float64 {
	if h.CharsPerToken > 0 {
		return h.CharsPerToken
	}
	return defaultCharsPerToken
}

func (h HeuristicEstimator) perMessage() int {
	if h.PerMessageTokens > 0 {
		return h.PerMessageTokens
	}
	return defaultPerMessage
}

func (h HeuristicEstimator) perTool() int {
	if h.PerToolTokens > 0 {
		return h.PerToolTokens
	}
	return defaultPerTool
}

func (h HeuristicEstimator) replyPriming() int {
	if h.ReplyPrimingTokens > 0 {
		return h.ReplyPrimingTokens
	}
	return defaultReplyPriming
}

func (h HeuristicEstimator) nonTextPart() int {
	if h.NonTextPartTokens > 0 {
		return h.NonTextPartTokens
	}
	return defaultNonTextPartCost
}

// EstimateText implements Estimator. It counts runes rather than bytes so that
// multi-byte text is not wildly over-counted, and never returns zero for a
// non-empty string.
func (h HeuristicEstimator) EstimateText(_, text string) int {
	if text == "" {
		return 0
	}
	n := math.Ceil(float64(utf8.RuneCountInString(text)) / h.charsPerToken())
	if n < 1 {
		return 1
	}
	return int(n)
}

// EstimateMessages approximates the tokens occupied by a message list,
// including per-message framing, tool calls and non-text parts.
func (h HeuristicEstimator) EstimateMessages(model string, msgs []provider.Message) int {
	total := 0
	for _, m := range msgs {
		total += h.perMessage()
		total += h.EstimateText(model, m.Content)
		total += h.EstimateText(model, m.Name)
		for _, p := range m.Parts {
			switch p.Kind {
			case provider.PartText:
				total += h.EstimateText(model, p.Text)
			default:
				total += h.nonTextPart()
			}
		}
		for _, tc := range m.ToolCalls {
			total += h.perMessage()
			total += h.EstimateText(model, tc.Name)
			total += h.EstimateText(model, tc.Arguments)
		}
	}
	return total
}

// EstimateTools approximates the tokens occupied by tool declarations.
func (h HeuristicEstimator) EstimateTools(model string, tools []provider.ToolDefinition) int {
	total := 0
	for _, td := range tools {
		total += h.perTool()
		total += h.EstimateText(model, td.Name)
		total += h.EstimateText(model, td.Description)
		if td.Schema != nil {
			if raw, err := json.Marshal(td.Schema); err == nil {
				total += h.EstimateText(model, string(raw))
			}
		}
	}
	return total
}

// EstimateRequest implements Estimator.
func (h HeuristicEstimator) EstimateRequest(model string, req provider.ChatRequest) int {
	return h.replyPriming() +
		h.EstimateMessages(model, req.Messages) +
		h.EstimateTools(model, req.Tools)
}

// EstimateUsage builds a provider.Usage from an Estimator.
//
// The result is an estimate by construction. Record it with
// Tracker.RecordEstimatedModelCall; passing it to RecordModelCall would file a
// guess as a measurement.
func EstimateUsage(est Estimator, model string, req provider.ChatRequest, completion string) provider.Usage {
	if est == nil {
		est = DefaultEstimator()
	}
	prompt := est.EstimateRequest(model, req)
	out := est.EstimateText(model, completion)
	return provider.Usage{
		PromptTokens:     prompt,
		CompletionTokens: out,
		TotalTokens:      prompt + out,
	}
}

// ContextUsage describes how much of a model's context window is occupied.
//
// It answers the question the UI actually needs to ask before the next request:
// "is there room left?". Tokens therefore reflects prompt plus completion of
// the last exchange, which is the floor for the next request's prompt.
type ContextUsage struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	// ContextWindow is the model's advertised window in tokens. Zero means the
	// provider never advertised one.
	ContextWindow int `json:"context_window"`
	// WindowKnown is false when the window is unknown; Utilisation and
	// Remaining carry no meaning in that case and are left at zero.
	WindowKnown bool `json:"window_known"`
	// Tokens is the occupancy after the recorded exchange.
	Tokens int `json:"tokens"`
	// Estimated reports that Tokens came from an Estimator rather than from
	// the provider. Never render an estimated utilisation as a hard limit.
	Estimated bool `json:"estimated"`
	// Utilisation is Tokens/ContextWindow. It is deliberately not clamped:
	// a value above 1 means the next request would be rejected.
	Utilisation float64 `json:"utilisation"`
	// Remaining is ContextWindow-Tokens, floored at zero.
	Remaining int `json:"remaining"`
	// At is when the occupancy was observed.
	At time.Time `json:"at"`
}

// Exceeds reports whether utilisation has reached threshold (0..1). It always
// returns false when the window is unknown, because a warning derived from an
// unknown limit would be noise.
func (u ContextUsage) Exceeds(threshold float64) bool {
	return u.WindowKnown && u.Utilisation >= threshold
}

// ContextUtilisation computes the fraction of window that tokens occupy.
//
// known is false when window is zero or negative, i.e. unknown; callers must
// check it rather than treating a 0 fraction as "plenty of room".
func ContextUtilisation(tokens, window int) (fraction float64, known bool) {
	if window <= 0 {
		return 0, false
	}
	if tokens < 0 {
		tokens = 0
	}
	return float64(tokens) / float64(window), true
}

type contextState struct {
	provider  string
	model     string
	tokens    int
	estimated bool
	at        time.Time
}

// ContextTracker records per-session context occupancy against known model
// context windows. It is safe for concurrent use.
//
// Windows are registered separately from occupancy so that a window discovered
// late (ListModels races the first request) still corrects earlier readings:
// utilisation is computed on read, never on write.
type ContextTracker struct {
	mu      sync.RWMutex
	windows map[string]int
	current map[string]contextState
}

// NewContextTracker returns an empty tracker.
func NewContextTracker() *ContextTracker {
	return &ContextTracker{
		windows: make(map[string]int),
		current: make(map[string]contextState),
	}
}

func modelKey(prov, model string) string {
	return strings.ToLower(strings.TrimSpace(prov)) + "/" + strings.ToLower(strings.TrimSpace(model))
}

// SetWindow records a model's context window in tokens. A window <= 0 removes
// the entry, keeping "unknown" distinct from "zero".
func (c *ContextTracker) SetWindow(prov, model string, window int) {
	key := modelKey(prov, model)
	c.mu.Lock()
	defer c.mu.Unlock()
	if window <= 0 {
		delete(c.windows, key)
		return
	}
	c.windows[key] = window
}

// RegisterModels records the context windows advertised by ListModels. Models
// that advertise no window are skipped rather than recorded as zero.
func (c *ContextTracker) RegisterModels(models ...provider.Model) {
	for _, m := range models {
		if m.ContextWindow > 0 {
			c.SetWindow(m.Provider, m.ID, m.ContextWindow)
		}
	}
}

// Window returns the recorded context window for a model. known is false when
// no window has been registered.
func (c *ContextTracker) Window(prov, model string) (window int, known bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	w, ok := c.windows[modelKey(prov, model)]
	return w, ok
}

// Observe records the context occupancy of a session after an exchange.
//
// src determines whether the resulting ContextUsage is flagged as estimated;
// pass SourceMeasured only for provider-reported counts.
func (c *ContextTracker) Observe(sessionID, prov, model string, tokens int, src TokenSource, at time.Time) {
	if sessionID == "" {
		return
	}
	if tokens < 0 {
		tokens = 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current[sessionID] = contextState{
		provider:  prov,
		model:     model,
		tokens:    tokens,
		estimated: src != SourceMeasured,
		at:        at,
	}
}

// Forget drops a session's context state, e.g. after /clear.
func (c *ContextTracker) Forget(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.current, sessionID)
}

// Usage returns the current context usage for a session. ok is false when the
// session has had no observed exchange.
func (c *ContextTracker) Usage(sessionID string) (ContextUsage, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	st, ok := c.current[sessionID]
	if !ok {
		return ContextUsage{}, false
	}
	return c.usageLocked(st), true
}

func (c *ContextTracker) usageLocked(st contextState) ContextUsage {
	u := ContextUsage{
		Provider:  st.provider,
		Model:     st.model,
		Tokens:    st.tokens,
		Estimated: st.estimated,
		At:        st.at,
	}
	window := c.windows[modelKey(st.provider, st.model)]
	frac, known := ContextUtilisation(st.tokens, window)
	if !known {
		return u
	}
	u.ContextWindow = window
	u.WindowKnown = true
	u.Utilisation = frac
	u.Remaining = max(0, window-st.tokens)
	return u
}

// Snapshot returns the current context usage of every known session.
func (c *ContextTracker) Snapshot() map[string]ContextUsage {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]ContextUsage, len(c.current))
	for id, st := range c.current {
		out[id] = c.usageLocked(st)
	}
	return out
}

// Reset drops all observed occupancy while keeping registered windows, which
// are provider facts rather than session state.
func (c *ContextTracker) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current = make(map[string]contextState)
}

// Package stats aggregates Boop's runtime usage: tokens, cost, tool and
// command activity, repair iterations, tests and wall-clock time.
//
// The tracker is deliberately in-memory and persistence-free. Everything
// exported here is JSON-serialisable because one Snapshot feeds both the
// /stats TUI command and the WebUI /api/stats endpoint, and a later store
// layer can persist that same snapshot without this package depending on
// SQLite or on the session runtime.
//
// Two rules run through the whole package. Measured numbers (reported by a
// provider) and estimated numbers (derived locally) are counted separately and
// stay separate all the way into the JSON. Unknown cost is reported as unknown
// rather than as zero.
package stats

import (
	"encoding/json"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/kawaiipantsu/boop/internal/provider"
)

// dayFormat keys the calendar-day dimension.
const dayFormat = "2006-01-02"

// Duration is a time.Duration that marshals as a number of milliseconds.
//
// Raw nanoseconds are unreadable in the WebUI JSON and Go's default encoding
// of time.Duration is an opaque integer, so snapshots carry milliseconds and
// name the fields accordingly.
type Duration time.Duration

// Duration returns the value as a time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// String formats the value like time.Duration does.
func (d Duration) String() string { return time.Duration(d).String() }

// MarshalJSON writes the duration as fractional milliseconds.
func (d Duration) MarshalJSON() ([]byte, error) {
	ms := float64(d) / float64(time.Millisecond)
	return []byte(strconv.FormatFloat(ms, 'f', -1, 64)), nil
}

// UnmarshalJSON reads fractional milliseconds back, rounding to whole
// nanoseconds.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var ms float64
	if err := json.Unmarshal(b, &ms); err != nil {
		return err
	}
	*d = Duration(math.Round(ms * float64(time.Millisecond)))
	return nil
}

func (d *Duration) add(v time.Duration) {
	if v > 0 {
		*d += Duration(v)
	}
}

// Counters holds the discrete event counts of one aggregation bucket.
type Counters struct {
	Messages          int `json:"messages"`
	ModelCalls        int `json:"model_calls"`
	ModelCallFailures int `json:"model_call_failures"`

	ToolCalls    int `json:"tool_calls"`
	ToolFailures int `json:"tool_failures"`

	Commands        int `json:"commands"`
	CommandFailures int `json:"command_failures"`
	CommandTimeouts int `json:"command_timeouts"`

	// RepairIterations counts passes through the error-repair loop; this is
	// the "retries" figure of the spec's session statistics.
	RepairIterations int `json:"repair_iterations"`
	// RepairSuccesses counts repair iterations that ended in a working
	// command, so the UI can show a repair success rate.
	RepairSuccesses int `json:"repair_successes"`

	TestRuns       int `json:"test_runs"`
	TestRunsFailed int `json:"test_runs_failed"`
	TestsPassed    int `json:"tests_passed"`
	TestsFailed    int `json:"tests_failed"`
	TestsSkipped   int `json:"tests_skipped"`

	AgentsSpawned   int `json:"agents_spawned"`
	AgentsCompleted int `json:"agents_completed"`
	AgentsFailed    int `json:"agents_failed"`
}

func (c *Counters) add(o Counters) {
	c.Messages += o.Messages
	c.ModelCalls += o.ModelCalls
	c.ModelCallFailures += o.ModelCallFailures
	c.ToolCalls += o.ToolCalls
	c.ToolFailures += o.ToolFailures
	c.Commands += o.Commands
	c.CommandFailures += o.CommandFailures
	c.CommandTimeouts += o.CommandTimeouts
	c.RepairIterations += o.RepairIterations
	c.RepairSuccesses += o.RepairSuccesses
	c.TestRuns += o.TestRuns
	c.TestRunsFailed += o.TestRunsFailed
	c.TestsPassed += o.TestsPassed
	c.TestsFailed += o.TestsFailed
	c.TestsSkipped += o.TestsSkipped
	c.AgentsSpawned += o.AgentsSpawned
	c.AgentsCompleted += o.AgentsCompleted
	c.AgentsFailed += o.AgentsFailed
}

// Durations holds accumulated wall-clock time per activity, in milliseconds
// once serialised.
type Durations struct {
	Model   Duration `json:"model_ms"`
	Tool    Duration `json:"tool_ms"`
	Command Duration `json:"command_ms"`
	Test    Duration `json:"test_ms"`
}

// CostTotals is the money side of one aggregation bucket.
//
// Measured and Estimated are kept apart, and calls we could not price at all
// are counted rather than folded in as zero: a snapshot with UnpricedCalls > 0
// is incomplete, not free.
type CostTotals struct {
	Currency string `json:"currency,omitempty"`
	// Measured is spend computed from provider-reported token counts.
	Measured float64 `json:"measured"`
	// Estimated is spend computed from estimated token counts. Display it with
	// an approximation marker.
	Estimated float64 `json:"estimated"`
	// FreeCalls counts calls to local providers, which cost nothing by rule.
	FreeCalls int `json:"free_calls"`
	// UnpricedCalls counts calls whose model has no rate. Their cost is
	// unknown, not zero.
	UnpricedCalls int `json:"unpriced_calls"`
}

// Total sums measured and estimated spend. Check Complete before presenting it
// as the whole bill.
func (c CostTotals) Total() float64 { return c.Measured + c.Estimated }

// Complete reports whether every priced-provider call had a known rate.
func (c CostTotals) Complete() bool { return c.UnpricedCalls == 0 }

// Aggregate is the accumulated activity of one dimension value: a session, a
// provider, a model, an agent, a calendar day, or the all-time total.
type Aggregate struct {
	// Key is the dimension value, e.g. a session ID or "2026-08-29". It is
	// empty on the all-time total.
	Key       string      `json:"key,omitempty"`
	FirstSeen time.Time   `json:"first_seen,omitzero"`
	LastSeen  time.Time   `json:"last_seen,omitzero"`
	Tokens    TokenCounts `json:"tokens"`
	Counters  Counters    `json:"counters"`
	Durations Durations   `json:"durations"`
	Cost      CostTotals  `json:"cost"`
}

// Scope identifies the buckets an event belongs to. Empty fields are simply
// not aggregated on that dimension, so a tool call with no agent contributes
// to the session but not to any agent.
type Scope struct {
	SessionID string `json:"session_id,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
}

// ModelCall describes one completed model exchange.
type ModelCall struct {
	Scope
	// Usage is the token accounting for the exchange. Whether it is treated as
	// measured or estimated is decided by which Record method is called, never
	// by inspecting this value.
	Usage provider.Usage
	// Duration is the wall-clock time of the request, first byte to last.
	Duration time.Duration
	// Failed marks an exchange that terminated in EventError.
	Failed bool
	// At defaults to the tracker clock when zero.
	At time.Time
}

// ToolInvocation describes one completed tool call.
type ToolInvocation struct {
	Scope
	Tool     string
	Duration time.Duration
	Failed   bool
	At       time.Time
}

// CommandRun describes one command executed by the run tool. Its fields mirror
// execution.RunResult without importing it, keeping stats free of the
// execution layer.
type CommandRun struct {
	Scope
	Command   string
	ExitCode  int
	Duration  time.Duration
	TimedOut  bool
	Cancelled bool
	At        time.Time
}

// Failed reports whether the run counts against the failure counters, matching
// execution.RunResult.Success inverted.
func (c CommandRun) Failed() bool {
	return c.ExitCode != 0 || c.TimedOut || c.Cancelled
}

// RepairIteration describes one pass of the error-repair loop.
type RepairIteration struct {
	Scope
	Command string
	// Attempt is the 1-based iteration number within the loop.
	Attempt int
	// Succeeded marks the iteration that produced a working command.
	Succeeded bool
	At        time.Time
}

// TestRun describes one completed test execution.
type TestRun struct {
	Scope
	Suite    string
	Passed   int
	Failed   int
	Skipped  int
	Duration time.Duration
	At       time.Time
}

// MessageEvent records a message added to a conversation.
type MessageEvent struct {
	Scope
	Role provider.Role
	At   time.Time
}

// AgentEvent records an agent lifecycle transition.
type AgentEvent struct {
	Scope
	// Failed marks a completion that ended in error; ignored on spawn.
	Failed bool
	At     time.Time
}

// Option configures a Tracker.
type Option func(*Tracker)

// WithClock replaces the time source, which makes tests deterministic.
func WithClock(now func() time.Time) Option {
	return func(t *Tracker) {
		if now != nil {
			t.now = now
		}
	}
}

// WithLocation sets the time zone used to bucket events into calendar days.
// Days are local by default because a user reading /stats thinks in local
// dates, not UTC ones.
func WithLocation(loc *time.Location) Option {
	return func(t *Tracker) {
		if loc != nil {
			t.loc = loc
		}
	}
}

// WithRates replaces the cost rate table.
func WithRates(rates *RateTable) Option {
	return func(t *Tracker) {
		if rates != nil {
			t.rates = rates
		}
	}
}

// WithEstimator replaces the token estimator used by RecordModelCallWithFallback.
func WithEstimator(est Estimator) Option {
	return func(t *Tracker) {
		if est != nil {
			t.estimator = est
		}
	}
}

// Tracker aggregates usage across sessions, providers, models, agents and
// calendar days. It is safe for concurrent use by the scheduler's agents.
//
// A single mutex guards every dimension. The critical sections are a handful
// of integer additions, and correlated updates across six buckets must be
// atomic anyway, so finer-grained locking would buy contention rather than
// throughput.
type Tracker struct {
	now func() time.Time
	loc *time.Location

	rates     *RateTable
	estimator Estimator
	ctx       *ContextTracker

	mu        sync.RWMutex
	started   time.Time
	totals    Aggregate
	sessions  map[string]*Aggregate
	agents    map[string]*Aggregate
	providers map[string]*Aggregate
	models    map[string]*Aggregate
	days      map[string]*Aggregate
}

// New returns a Tracker with the bundled rate table, the heuristic estimator
// and the local clock.
func New(opts ...Option) *Tracker {
	t := &Tracker{
		now:       time.Now,
		loc:       time.Local,
		rates:     NewRateTable(),
		estimator: DefaultEstimator(),
		ctx:       NewContextTracker(),
	}
	for _, opt := range opts {
		opt(t)
	}
	t.started = t.now()
	t.resetBuckets()
	return t
}

func (t *Tracker) resetBuckets() {
	t.totals = Aggregate{}
	t.sessions = make(map[string]*Aggregate)
	t.agents = make(map[string]*Aggregate)
	t.providers = make(map[string]*Aggregate)
	t.models = make(map[string]*Aggregate)
	t.days = make(map[string]*Aggregate)
}

// Rates returns the tracker's rate table so callers can apply user overrides.
func (t *Tracker) Rates() *RateTable { return t.rates }

// Estimator returns the estimator in use.
func (t *Tracker) Estimator() Estimator { return t.estimator }

// Context returns the context-window tracker, on which the model router should
// register advertised context windows.
func (t *Tracker) Context() *ContextTracker { return t.ctx }

// StartedAt reports when counting began.
func (t *Tracker) StartedAt() time.Time {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.started
}

// Uptime reports elapsed wall-clock time since counting began.
func (t *Tracker) Uptime() time.Duration {
	return t.now().Sub(t.StartedAt())
}

// Reset clears every counter and restarts the elapsed-time clock. Registered
// model context windows survive because they are provider facts, not usage.
func (t *Tracker) Reset() {
	t.mu.Lock()
	t.started = t.now()
	t.resetBuckets()
	t.mu.Unlock()
	t.ctx.Reset()
}

// modelDimension keys the per-model dimension. Models are namespaced by
// provider because the same model ID can be served by several backends at very
// different prices.
func modelDimension(prov, model string) string {
	if model == "" {
		return ""
	}
	if prov == "" {
		return model
	}
	return prov + "/" + model
}

// apply mutates every bucket the scope belongs to, under one lock.
func (t *Tracker) apply(s Scope, at time.Time, fn func(*Aggregate)) {
	if at.IsZero() {
		at = t.now()
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	targets := make([]*Aggregate, 0, 6)
	targets = append(targets, &t.totals)
	targets = appendBucket(targets, t.sessions, s.SessionID)
	targets = appendBucket(targets, t.agents, s.AgentID)
	targets = appendBucket(targets, t.providers, s.Provider)
	targets = appendBucket(targets, t.models, modelDimension(s.Provider, s.Model))
	targets = appendBucket(targets, t.days, at.In(t.loc).Format(dayFormat))

	for _, a := range targets {
		if a.FirstSeen.IsZero() || at.Before(a.FirstSeen) {
			a.FirstSeen = at
		}
		if at.After(a.LastSeen) {
			a.LastSeen = at
		}
		fn(a)
	}
}

func appendBucket(dst []*Aggregate, m map[string]*Aggregate, key string) []*Aggregate {
	if key == "" {
		return dst
	}
	a, ok := m[key]
	if !ok {
		a = &Aggregate{Key: key}
		m[key] = a
	}
	return append(dst, a)
}

// RecordModelCall records an exchange whose Usage was reported by the
// provider; the tokens are filed as measured.
//
// When the provider reported nothing the call is still counted, but no tokens
// are added — an unreported usage is not a measurement of zero. Use
// RecordEstimatedModelCall or RecordModelCallWithFallback to attach an
// estimate instead.
func (t *Tracker) RecordModelCall(c ModelCall) {
	src := SourceMeasured
	if !UsageReported(c.Usage) {
		c.Usage = provider.Usage{}
	}
	t.recordModelCall(c, src)
}

// RecordEstimatedModelCall records an exchange whose Usage came from an
// Estimator. The tokens are filed as estimated and never mix with measured
// ones. There is deliberately no flag on ModelCall that could be left unset
// and silently promote a guess to a measurement.
func (t *Tracker) RecordEstimatedModelCall(c ModelCall) {
	t.recordModelCall(c, SourceEstimated)
}

// RecordModelCallWithFallback records provider-reported usage when there is
// any, and otherwise estimates the exchange from the request and the completion
// text. It returns the source that was used so the caller can label its own UI.
func (t *Tracker) RecordModelCallWithFallback(c ModelCall, req provider.ChatRequest, completion string) TokenSource {
	if UsageReported(c.Usage) {
		t.recordModelCall(c, SourceMeasured)
		return SourceMeasured
	}
	model := c.Model
	if model == "" {
		model = req.Model
	}
	c.Usage = EstimateUsage(t.estimator, model, req, completion)
	t.recordModelCall(c, SourceEstimated)
	return SourceEstimated
}

func (t *Tracker) recordModelCall(c ModelCall, src TokenSource) {
	tokens := TokensFromUsage(c.Usage)
	cost, priced := t.rates.Cost(c.Usage, c.Provider, c.Model)
	currency := cost.Currency

	t.apply(c.Scope, c.At, func(a *Aggregate) {
		a.Counters.ModelCalls++
		if c.Failed {
			a.Counters.ModelCallFailures++
		}
		a.Durations.Model.add(c.Duration)
		if !tokens.IsZero() {
			a.Tokens.Add(src, tokens)
		}
		if a.Cost.Currency == "" {
			a.Cost.Currency = currency
		}
		switch {
		case !priced:
			a.Cost.UnpricedCalls++
		case cost.Local:
			a.Cost.FreeCalls++
		case src == SourceMeasured:
			a.Cost.Measured += cost.Total
		default:
			a.Cost.Estimated += cost.Total
		}
	})

	if c.SessionID != "" && !tokens.IsZero() {
		at := c.At
		if at.IsZero() {
			at = t.now()
		}
		// Occupancy after the exchange is the floor for the next prompt, which
		// is what the UI must warn against.
		t.ctx.Observe(c.SessionID, c.Provider, c.Model, tokens.Prompt+tokens.Completion, src, at)
	}
}

// RecordMessage counts a message added to a conversation.
func (t *Tracker) RecordMessage(m MessageEvent) {
	t.apply(m.Scope, m.At, func(a *Aggregate) {
		a.Counters.Messages++
	})
}

// RecordToolCall counts a completed tool invocation and its duration.
func (t *Tracker) RecordToolCall(i ToolInvocation) {
	t.apply(i.Scope, i.At, func(a *Aggregate) {
		a.Counters.ToolCalls++
		if i.Failed {
			a.Counters.ToolFailures++
		}
		a.Durations.Tool.add(i.Duration)
	})
}

// RecordCommand counts a command executed through the run tool. A non-zero
// exit code, a timeout or a cancellation all count as a failure, matching the
// execution layer's definition of success.
func (t *Tracker) RecordCommand(c CommandRun) {
	failed := c.Failed()
	t.apply(c.Scope, c.At, func(a *Aggregate) {
		a.Counters.Commands++
		if failed {
			a.Counters.CommandFailures++
		}
		if c.TimedOut {
			a.Counters.CommandTimeouts++
		}
		a.Durations.Command.add(c.Duration)
	})
}

// RecordRepairIteration counts one pass of the error-repair loop.
func (t *Tracker) RecordRepairIteration(r RepairIteration) {
	t.apply(r.Scope, r.At, func(a *Aggregate) {
		a.Counters.RepairIterations++
		if r.Succeeded {
			a.Counters.RepairSuccesses++
		}
	})
}

// RecordTestRun counts a test execution and its pass/fail/skip split.
func (t *Tracker) RecordTestRun(r TestRun) {
	t.apply(r.Scope, r.At, func(a *Aggregate) {
		a.Counters.TestRuns++
		if r.Failed > 0 {
			a.Counters.TestRunsFailed++
		}
		a.Counters.TestsPassed += max(0, r.Passed)
		a.Counters.TestsFailed += max(0, r.Failed)
		a.Counters.TestsSkipped += max(0, r.Skipped)
		a.Durations.Test.add(r.Duration)
	})
}

// RecordAgentSpawned counts an agent created by the scheduler.
func (t *Tracker) RecordAgentSpawned(e AgentEvent) {
	t.apply(e.Scope, e.At, func(a *Aggregate) {
		a.Counters.AgentsSpawned++
	})
}

// RecordAgentCompleted counts an agent that reached a terminal state.
func (t *Tracker) RecordAgentCompleted(e AgentEvent) {
	t.apply(e.Scope, e.At, func(a *Aggregate) {
		a.Counters.AgentsCompleted++
		if e.Failed {
			a.Counters.AgentsFailed++
		}
	})
}

// Snapshot is a JSON-serialisable view of the tracker, shared by /stats and
// GET /api/stats.
type Snapshot struct {
	GeneratedAt time.Time `json:"generated_at"`
	StartedAt   time.Time `json:"started_at"`
	Uptime      Duration  `json:"uptime_ms"`
	// Estimator names the estimator that produced every Tokens.Estimated and
	// Cost.Estimated figure below, so the UI can attribute the guess.
	Estimator string `json:"estimator"`
	Currency  string `json:"currency"`

	Totals    Aggregate            `json:"totals"`
	Sessions  map[string]Aggregate `json:"sessions"`
	Agents    map[string]Aggregate `json:"agents"`
	Providers map[string]Aggregate `json:"providers"`
	Models    map[string]Aggregate `json:"models"`
	// Days is keyed YYYY-MM-DD in the tracker's location.
	Days map[string]Aggregate `json:"days"`
	// Contexts is the live context-window occupancy per session.
	Contexts map[string]ContextUsage `json:"contexts"`
}

// Snapshot returns a deep copy of the current statistics. Aggregate contains
// no reference types, so copying the maps by value is a complete copy and the
// result is immune to further updates.
func (t *Tracker) Snapshot() Snapshot {
	now := t.now()

	t.mu.RLock()
	s := Snapshot{
		GeneratedAt: now,
		StartedAt:   t.started,
		Uptime:      Duration(now.Sub(t.started)),
		Estimator:   t.estimator.Name(),
		Currency:    t.rates.Currency(),
		Totals:      t.totals,
		Sessions:    copyBuckets(t.sessions),
		Agents:      copyBuckets(t.agents),
		Providers:   copyBuckets(t.providers),
		Models:      copyBuckets(t.models),
		Days:        copyBuckets(t.days),
	}
	t.mu.RUnlock()

	s.Contexts = t.ctx.Snapshot()
	return s
}

func copyBuckets(m map[string]*Aggregate) map[string]Aggregate {
	out := make(map[string]Aggregate, len(m))
	for k, v := range m {
		out[k] = *v
	}
	return out
}

// SessionStats returns the aggregate for one session. ok is false when the
// session has recorded nothing.
func (t *Tracker) SessionStats(id string) (Aggregate, bool) {
	return t.lookup(t.sessions, id)
}

// AgentStats returns the aggregate for one agent.
func (t *Tracker) AgentStats(id string) (Aggregate, bool) {
	return t.lookup(t.agents, id)
}

// ProviderStats returns the aggregate for one provider.
func (t *Tracker) ProviderStats(name string) (Aggregate, bool) {
	return t.lookup(t.providers, name)
}

// ModelStats returns the aggregate for one provider/model pair.
func (t *Tracker) ModelStats(prov, model string) (Aggregate, bool) {
	return t.lookup(t.models, modelDimension(prov, model))
}

// DayStats returns the aggregate for one calendar day in the tracker's
// location.
func (t *Tracker) DayStats(day time.Time) (Aggregate, bool) {
	return t.lookup(t.days, day.In(t.loc).Format(dayFormat))
}

// Totals returns the all-time aggregate.
func (t *Tracker) Totals() Aggregate {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.totals
}

func (t *Tracker) lookup(m map[string]*Aggregate, key string) (Aggregate, bool) {
	if key == "" {
		return Aggregate{}, false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	a, ok := m[key]
	if !ok {
		return Aggregate{}, false
	}
	return *a, true
}

// Merge folds another aggregate into a, which lets a caller combine buckets
// (for example every day in a week) without reaching into the tracker.
func (a *Aggregate) Merge(o Aggregate) {
	if a.FirstSeen.IsZero() || (!o.FirstSeen.IsZero() && o.FirstSeen.Before(a.FirstSeen)) {
		a.FirstSeen = o.FirstSeen
	}
	if o.LastSeen.After(a.LastSeen) {
		a.LastSeen = o.LastSeen
	}
	a.Tokens.Measured.Add(o.Tokens.Measured)
	a.Tokens.Estimated.Add(o.Tokens.Estimated)
	a.Counters.add(o.Counters)
	a.Durations.Model += o.Durations.Model
	a.Durations.Tool += o.Durations.Tool
	a.Durations.Command += o.Durations.Command
	a.Durations.Test += o.Durations.Test
	if a.Cost.Currency == "" {
		a.Cost.Currency = o.Cost.Currency
	}
	a.Cost.Measured += o.Cost.Measured
	a.Cost.Estimated += o.Cost.Estimated
	a.Cost.FreeCalls += o.Cost.FreeCalls
	a.Cost.UnpricedCalls += o.Cost.UnpricedCalls
}

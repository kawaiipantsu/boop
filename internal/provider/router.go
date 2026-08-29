package provider

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// This file implements PROJECT.md §9 model routing on top of the Provider
// interface. It deliberately imports no adapter package: the router must stay
// usable with any backend, including the fakes the test harness (§42) builds.

// RouteClass names one of the configurable routing classes from §9.
//
// The four constants below are the classes Boop understands out of the box;
// a configuration may define additional classes and select them by name.
type RouteClass string

const (
	// ClassDefault is used when a request names no class and needs no
	// capability that implies one.
	ClassDefault RouteClass = "default"
	// ClassVision is used for requests carrying images.
	ClassVision RouteClass = "vision"
	// ClassReasoning is used for requests that want a thinking model.
	ClassReasoning RouteClass = "reasoning"
	// ClassFast is used for latency-sensitive or throwaway work.
	ClassFast RouteClass = "fast"
)

// knownClassOrder fixes the iteration order of the built-in classes so that
// alternative listings and debug output are reproducible; Go map iteration is
// randomized and would otherwise make status output jitter between calls.
var knownClassOrder = []RouteClass{ClassDefault, ClassVision, ClassReasoning, ClassFast}

// Target is a provider/model pair, the unit a routing class resolves to.
type Target struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// String renders the target as provider/model for logs and status output.
func (t Target) String() string {
	switch {
	case t.Provider == "" && t.Model == "":
		return "<unset>"
	case t.Model == "":
		return t.Provider + "/<no model>"
	default:
		return t.Provider + "/" + t.Model
	}
}

// IsZero reports whether the target names neither a provider nor a model.
func (t Target) IsZero() bool { return t.Provider == "" && t.Model == "" }

// Registry holds the providers Boop has been configured with, by name.
//
// It is separate from Router because the same set of providers is also used by
// model listings, the status view and the agent runtime, none of which route.
type Registry struct {
	mu     sync.RWMutex
	byName map[string]Provider
	order  []string
}

// NewRegistry builds a registry, registering the given providers in order.
// Duplicate or unnamed providers are skipped; use Register when you need the
// error.
func NewRegistry(providers ...Provider) *Registry {
	r := &Registry{byName: make(map[string]Provider, len(providers))}
	for _, p := range providers {
		_ = r.Register(p)
	}
	return r
}

// Register adds p under p.Name().
func (r *Registry) Register(p Provider) error {
	if p == nil {
		return fmt.Errorf("provider registry: cannot register a nil provider")
	}
	name := strings.TrimSpace(p.Name())
	if name == "" {
		return fmt.Errorf("provider registry: provider has no name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byName == nil {
		r.byName = make(map[string]Provider)
	}
	if _, dup := r.byName[name]; dup {
		return fmt.Errorf("provider registry: %q is already registered", name)
	}
	r.byName[name] = p
	r.order = append(r.order, name)
	return nil
}

// Get returns the provider registered under name.
func (r *Registry) Get(name string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.byName[strings.TrimSpace(name)]
	return p, ok
}

// Names lists registered provider names in registration order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.order...)
}

// Len reports how many providers are registered.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byName)
}

// Router defaults.
const (
	// DefaultHealthTTL is how long a successful health probe is trusted.
	DefaultHealthTTL = 10 * time.Second
	// DefaultUnhealthyTTL is how long a failed probe suppresses a provider.
	// It is longer than DefaultHealthTTL so a dead local server (the common
	// case: Ollama not running) is not re-dialled on every single call.
	DefaultUnhealthyTTL = 30 * time.Second
	// DefaultDecisionHistory bounds the recorded routing decisions.
	DefaultDecisionHistory = 32
)

// RouterConfig is the §9 routing configuration in resolved form.
//
// It mirrors the YAML in §9 but holds no config types, so internal/config owns
// parsing and this package owns behaviour.
type RouterConfig struct {
	// Classes maps a routing class to the provider/model that serves it.
	Classes map[RouteClass]Target
	// Fallback is an ordered list of provider names tried when the selected
	// provider fails retryably.
	Fallback []string
	// Models lists additional configured provider/model pairs. They are not
	// routed to directly, but they are offered as alternatives when a
	// capability is missing (§8) and they supply a model id for a fallback
	// provider that no class names.
	Models []Target
	// HealthTTL caches a healthy verdict. Zero means DefaultHealthTTL.
	HealthTTL time.Duration
	// UnhealthyTTL caches a down verdict. Zero means DefaultUnhealthyTTL.
	UnhealthyTTL time.Duration
	// ProbeHealth makes the router call Provider.Health for candidates whose
	// health is not cached. It is off by default because probing costs a
	// round trip on the hot path; with it off the router still skips
	// providers it has already seen fail.
	ProbeHealth bool
	// HistorySize bounds recorded decisions. Zero means
	// DefaultDecisionHistory.
	HistorySize int
	// Now is a clock seam for tests. Zero means time.Now.
	Now func() time.Time
}

// Selection describes what a caller wants routed.
//
// Resolution order follows §9: an explicit Provider (and optionally Model)
// wins, then the routing class, then the default class.
type Selection struct {
	// Provider pins a provider by name (manual selection).
	Provider string
	// Model pins a model id. With no Provider it is resolved against the
	// configured targets.
	Model string
	// Class selects a routing class. Empty means infer from Required, then
	// ClassDefault.
	Class RouteClass
	// Required lists capabilities the task genuinely needs (§8).
	Required Capabilities
	// NoFallback pins the selection: a failure is reported rather than
	// silently served by a different provider.
	NoFallback bool
}

// Attempt records one candidate the router considered.
type Attempt struct {
	Target Target `json:"target"`
	// Skipped explains why the candidate was never tried; empty when it was.
	Skipped string `json:"skipped,omitempty"`
	// Err is the failure the candidate returned, if it was tried and failed.
	Err error `json:"-"`
}

// String renders the attempt for debug output.
func (a Attempt) String() string {
	switch {
	case a.Skipped != "":
		return fmt.Sprintf("%s skipped (%s)", a.Target, a.Skipped)
	case a.Err != nil:
		return fmt.Sprintf("%s failed (%s)", a.Target, a.Err)
	default:
		return fmt.Sprintf("%s ok", a.Target)
	}
}

// Decision records why the router chose what it chose.
//
// §9 requires routing decisions to be visible in debug/status output, so every
// resolution produces one of these whether it succeeded or not.
type Decision struct {
	// Target is the chosen provider/model, zero when nothing could serve the
	// request.
	Target Target `json:"target"`
	// Class is the routing class that was applied.
	Class RouteClass `json:"class"`
	// Manual is true when the caller named a provider or model explicitly.
	Manual bool `json:"manual"`
	// Fellback is true when the chosen target was not the first candidate.
	Fellback bool `json:"fellback"`
	// Reason is a one-line human explanation.
	Reason string `json:"reason"`
	// Required echoes the capabilities the request demanded.
	Required Capabilities `json:"required,omitempty"`
	// Attempts lists every candidate considered, in order.
	Attempts []Attempt `json:"attempts,omitempty"`
	At       time.Time `json:"at"`
}

// String renders the decision as a single debug/status line.
func (d Decision) String() string {
	var b strings.Builder
	if d.Target.IsZero() {
		b.WriteString("route unresolved")
	} else {
		b.WriteString("route " + d.Target.String())
	}
	b.WriteString(" [class=" + string(d.Class))
	if d.Manual {
		b.WriteString(" manual")
	}
	if d.Fellback {
		b.WriteString(" fallback")
	}
	if len(d.Required) > 0 {
		b.WriteString(" needs=" + strings.Join(d.Required.Strings(), ","))
	}
	b.WriteString("]")
	if d.Reason != "" {
		b.WriteString(": " + d.Reason)
	}
	for _, a := range d.Attempts {
		b.WriteString("; " + a.String())
	}
	return b.String()
}

// CapabilityRoutingError reports that the routed model cannot do what the task
// needs, and names the configured models that can.
//
// §8 requires the missing capability to be explained and compatible models
// offered rather than the request failing opaquely. It wraps
// *UnsupportedCapabilityError, so callers that only care about the category can
// keep using errors.As on that type.
type CapabilityRoutingError struct {
	*UnsupportedCapabilityError
	// Alternatives are configured targets that do have every required
	// capability, in configuration order.
	Alternatives []Target
}

func (e *CapabilityRoutingError) Error() string {
	base := e.UnsupportedCapabilityError.Error()
	if len(e.Alternatives) == 0 {
		return base + "; no configured model provides them"
	}
	names := make([]string, len(e.Alternatives))
	for i, t := range e.Alternatives {
		names[i] = t.String()
	}
	return base + "; configured alternatives: " + strings.Join(names, ", ")
}

// Unwrap exposes the normalized capability error to errors.As/Is.
func (e *CapabilityRoutingError) Unwrap() error { return e.UnsupportedCapabilityError }

// healthEntry is a cached health verdict. A nil Err means healthy.
type healthEntry struct {
	err error
	at  time.Time
}

// Router resolves a Selection to a provider/model pair and runs work against
// it, falling back down the configured provider list on retryable failures.
//
// A Router is safe for concurrent use.
type Router struct {
	reg *Registry

	classes      map[RouteClass]Target
	classOrder   []RouteClass
	fallback     []string
	models       []Target
	healthTTL    time.Duration
	unhealthyTTL time.Duration
	probe        bool
	now          func() time.Time

	mu       sync.Mutex
	health   map[string]healthEntry
	history  []Decision
	histSize int
}

// NewRouter builds a router over reg using cfg.
func NewRouter(reg *Registry, cfg RouterConfig) *Router {
	if reg == nil {
		reg = NewRegistry()
	}
	r := &Router{
		reg:          reg,
		classes:      make(map[RouteClass]Target, len(cfg.Classes)),
		fallback:     dedupeStrings(cfg.Fallback),
		models:       append([]Target(nil), cfg.Models...),
		healthTTL:    cfg.HealthTTL,
		unhealthyTTL: cfg.UnhealthyTTL,
		probe:        cfg.ProbeHealth,
		now:          cfg.Now,
		health:       make(map[string]healthEntry),
		histSize:     cfg.HistorySize,
	}
	for class, target := range cfg.Classes {
		r.classes[class] = target
	}
	r.classOrder = orderedClasses(r.classes)
	if r.healthTTL <= 0 {
		r.healthTTL = DefaultHealthTTL
	}
	if r.unhealthyTTL <= 0 {
		r.unhealthyTTL = DefaultUnhealthyTTL
	}
	if r.histSize <= 0 {
		r.histSize = DefaultDecisionHistory
	}
	if r.now == nil {
		r.now = time.Now
	}
	return r
}

// orderedClasses lists configured classes with the built-in ones first, then
// any custom classes sorted, so output never depends on map ordering.
func orderedClasses(classes map[RouteClass]Target) []RouteClass {
	out := make([]RouteClass, 0, len(classes))
	seen := make(map[RouteClass]struct{}, len(classes))
	for _, c := range knownClassOrder {
		if _, ok := classes[c]; ok {
			out = append(out, c)
			seen[c] = struct{}{}
		}
	}
	var extra []RouteClass
	for c := range classes {
		if _, ok := seen[c]; !ok {
			extra = append(extra, c)
		}
	}
	sort.Slice(extra, func(i, j int) bool { return extra[i] < extra[j] })
	return append(out, extra...)
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// Registry exposes the providers this router routes over.
func (r *Router) Registry() *Registry { return r.reg }

// Classes returns a copy of the configured routing classes, for status output.
func (r *Router) Classes() map[RouteClass]Target {
	out := make(map[RouteClass]Target, len(r.classes))
	for c, t := range r.classes {
		out[c] = t
	}
	return out
}

// Fallback returns the configured fallback provider order.
func (r *Router) Fallback() []string { return append([]string(nil), r.fallback...) }

// Resolve picks a provider/model for sel without performing any work.
//
// It applies the same class resolution, health filtering and capability check
// that Do applies, so a UI can show the model that would be used — and the
// reason it was picked — before a request is sent.
func (r *Router) Resolve(ctx context.Context, sel Selection) (Provider, Decision, error) {
	var chosen Provider
	dec, err := r.Do(ctx, sel, func(_ context.Context, p Provider, _ string) error {
		chosen = p
		return nil
	})
	if err != nil {
		return nil, dec, err
	}
	return chosen, dec, nil
}

// Do runs fn against the routed provider, walking the fallback list when a
// candidate fails retryably.
//
// fn receives the resolved provider and the model id chosen for it, because a
// fallback provider generally serves a different model than the primary.
//
// Fallback stops immediately on a non-retryable error. IsRetryable already
// encodes that authentication and invalid-request failures are not worth
// repeating: retrying a bad key against four providers just produces four
// identical failures and hides the real one.
func (r *Router) Do(ctx context.Context, sel Selection, fn func(context.Context, Provider, string) error) (Decision, error) {
	class, manual := classFor(sel)
	dec := Decision{Class: class, Manual: manual, Required: sel.Required, At: r.now()}

	candidates, err := r.candidates(sel, class)
	if err != nil {
		dec.Reason = err.Error()
		r.record(dec)
		return dec, err
	}

	var firstErr error
	note := func(e error) {
		if firstErr == nil {
			firstErr = e
		}
	}

	for i, target := range candidates {
		p, ok := r.reg.Get(target.Provider)
		if !ok {
			dec.Attempts = append(dec.Attempts, Attempt{Target: target, Skipped: "provider not registered"})
			note(r.routingError(ErrInvalidRequest, target, fmt.Sprintf("provider %q is not registered", target.Provider)))
			continue
		}
		if target.Model == "" {
			dec.Attempts = append(dec.Attempts, Attempt{Target: target, Skipped: "no model configured for this provider"})
			note(r.routingError(ErrInvalidRequest, target, fmt.Sprintf("no model configured for provider %q", target.Provider)))
			continue
		}
		if down, why := r.cachedDown(target.Provider); down {
			dec.Attempts = append(dec.Attempts, Attempt{Target: target, Skipped: "unhealthy: " + why})
			note(r.routingError(ErrUnavailable, target, fmt.Sprintf("%s is not responding: %s", target.Provider, why)))
			continue
		}
		if r.probe {
			if herr := r.Health(ctx, target.Provider); herr != nil {
				dec.Attempts = append(dec.Attempts, Attempt{Target: target, Skipped: "health probe failed: " + herr.Error()})
				note(herr)
				continue
			}
		}
		if len(sel.Required) > 0 {
			if cerr := r.checkCapabilities(ctx, p, target, sel.Required); cerr != nil {
				dec.Attempts = append(dec.Attempts, Attempt{Target: target, Err: cerr})
				note(cerr)
				continue
			}
		}
		if fn != nil {
			if ferr := fn(ctx, p, target.Model); ferr != nil {
				dec.Attempts = append(dec.Attempts, Attempt{Target: target, Err: ferr})
				note(ferr)
				if !IsRetryable(ferr) {
					dec.Target = target
					dec.Fellback = i > 0
					dec.Reason = "stopped at " + target.String() + ": failure is not retryable"
					r.record(dec)
					return dec, ferr
				}
				r.MarkDown(target.Provider, ferr)
				continue
			}
		}

		dec.Target = target
		dec.Fellback = i > 0
		dec.Reason = r.reasonFor(sel, class, target, i)
		r.MarkUp(target.Provider)
		r.record(dec)
		return dec, nil
	}

	if firstErr == nil {
		firstErr = r.routingError(ErrUnavailable, Target{}, "no configured provider could serve this request")
	}
	dec.Reason = "no candidate could serve the request"
	r.record(dec)
	return dec, firstErr
}

// Chat routes a chat request and returns the resulting event stream.
//
// To decide whether a failure is worth falling back from, Chat waits for the
// first event of the stream: adapters report post-dispatch failures as a
// terminal EventError rather than as a Chat error, so there is no other signal.
// That costs the time to first token on the primary provider, and is the price
// of automatic fallback; callers that cannot pay it should Resolve and call the
// provider directly.
func (r *Router) Chat(ctx context.Context, sel Selection, req ChatRequest) (<-chan ChatEvent, Decision, error) {
	var stream <-chan ChatEvent
	dec, err := r.Do(ctx, sel, func(ctx context.Context, p Provider, model string) error {
		attempt := req
		attempt.Model = model

		raw, err := p.Chat(ctx, attempt)
		if err != nil {
			return err
		}

		select {
		case first, ok := <-raw:
			if !ok {
				return &Error{
					Category: ErrMalformedResponse,
					Provider: p.Name(),
					Model:    model,
					Message:  fmt.Sprintf("%s closed the stream without sending an event", p.Name()),
				}
			}
			if first.Type == EventError && first.Err != nil {
				go drainEvents(raw)
				return first.Err
			}
			stream = replayEvents(ctx, first, raw)
			return nil
		case <-ctx.Done():
			go drainEvents(raw)
			return &Error{
				Category: ErrCancelled,
				Provider: p.Name(),
				Model:    model,
				Message:  "request cancelled",
				Err:      ctx.Err(),
			}
		}
	})
	if err != nil {
		return nil, dec, err
	}
	return stream, dec, nil
}

// replayEvents re-emits the peeked first event ahead of the rest of the stream
// so the caller sees an unmodified event sequence.
func replayEvents(ctx context.Context, first ChatEvent, rest <-chan ChatEvent) <-chan ChatEvent {
	out := make(chan ChatEvent, 16)
	go func() {
		defer close(out)
		select {
		case out <- first:
		case <-ctx.Done():
			return
		}
		for ev := range rest {
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// drainEvents releases an abandoned stream so the adapter goroutine can finish.
func drainEvents(ch <-chan ChatEvent) {
	for range ch {
	}
}

// classFor determines the routing class and whether the caller selected
// manually.
//
// An unnamed class is inferred from the required capabilities, so a request
// carrying an image lands on the vision class without the caller restating it.
func classFor(sel Selection) (RouteClass, bool) {
	manual := strings.TrimSpace(sel.Provider) != "" || strings.TrimSpace(sel.Model) != ""
	if sel.Class != "" {
		return sel.Class, manual
	}
	switch {
	case sel.Required.Has(CapabilityVision):
		return ClassVision, manual
	case sel.Required.Has(CapabilityReasoning):
		return ClassReasoning, manual
	default:
		return ClassDefault, manual
	}
}

// candidates builds the ordered list of targets to try.
func (r *Router) candidates(sel Selection, class RouteClass) ([]Target, error) {
	primary, err := r.primary(sel, class)
	if err != nil {
		return nil, err
	}

	out := []Target{primary}
	if sel.NoFallback {
		return out, nil
	}
	for _, name := range r.fallback {
		if name == primary.Provider {
			continue
		}
		out = append(out, Target{Provider: name, Model: r.modelFor(name, class, sel.Model)})
	}
	return out, nil
}

// primary resolves the first candidate: manual selection, then the class, then
// the default class.
func (r *Router) primary(sel Selection, class RouteClass) (Target, error) {
	provider := strings.TrimSpace(sel.Provider)
	model := strings.TrimSpace(sel.Model)

	if provider != "" {
		if model == "" {
			model = r.modelFor(provider, class, "")
		}
		return Target{Provider: provider, Model: model}, nil
	}

	if model != "" {
		// A bare model id: find the provider that was configured with it,
		// and accept a single-provider setup as unambiguous.
		if t, ok := r.targetForModel(model); ok {
			return t, nil
		}
		if names := r.reg.Names(); len(names) == 1 {
			return Target{Provider: names[0], Model: model}, nil
		}
		return Target{}, r.routingError(ErrInvalidRequest, Target{Model: model},
			fmt.Sprintf("model %q is not configured for any provider; name a provider explicitly", model))
	}

	if t, ok := r.classes[class]; ok && t.Provider != "" {
		return t, nil
	}
	// An unconfigured class degrades to the default class rather than
	// failing: a user asking for "fast" on a setup that never defined it
	// wants an answer, not a configuration lecture.
	if class != ClassDefault {
		if t, ok := r.classes[ClassDefault]; ok && t.Provider != "" {
			return t, nil
		}
	}
	return Target{}, r.routingError(ErrInvalidRequest, Target{},
		fmt.Sprintf("no routing target configured for class %q and no default class is set", class))
}

// targetForModel finds a configured target by model id.
func (r *Router) targetForModel(model string) (Target, bool) {
	for _, class := range r.classOrder {
		if t := r.classes[class]; t.Model == model && t.Provider != "" {
			return t, true
		}
	}
	for _, t := range r.models {
		if t.Model == model && t.Provider != "" {
			return t, true
		}
	}
	return Target{}, false
}

// modelFor picks the model a fallback provider should serve.
//
// Preference order: the model this provider serves for the requested class, any
// other class it serves, an explicitly configured model, then the model the
// caller asked for — some providers do share model ids, and trying is better
// than skipping the provider outright.
func (r *Router) modelFor(providerName string, class RouteClass, requested string) string {
	if t, ok := r.classes[class]; ok && t.Provider == providerName && t.Model != "" {
		return t.Model
	}
	for _, c := range r.classOrder {
		if t := r.classes[c]; t.Provider == providerName && t.Model != "" {
			return t.Model
		}
	}
	for _, t := range r.models {
		if t.Provider == providerName && t.Model != "" {
			return t.Model
		}
	}
	return requested
}

// checkCapabilities enforces §8: a model is only used for work it can do.
func (r *Router) checkCapabilities(ctx context.Context, p Provider, target Target, required Capabilities) error {
	caps, err := p.Capabilities(ctx, target.Model)
	if err != nil {
		return err
	}
	missing := caps.Missing(required...)
	if len(missing) == 0 {
		return nil
	}
	return &CapabilityRoutingError{
		UnsupportedCapabilityError: &UnsupportedCapabilityError{
			Provider: target.Provider,
			Model:    target.Model,
			Missing:  missing,
		},
		Alternatives: r.alternativesExcept(ctx, target, required),
	}
}

// Alternatives lists configured targets that support every required
// capability, so a UI can offer a model switch (§8).
func (r *Router) Alternatives(ctx context.Context, required ...Capability) []Target {
	return r.alternativesExcept(ctx, Target{}, required)
}

// alternativesExcept is Alternatives with one target left out — the one that
// just failed the check.
//
// Only configured targets are examined. Enumerating every model of every
// provider would be a network fan-out on an error path, and §8 asks for
// "compatible configured models".
func (r *Router) alternativesExcept(ctx context.Context, skip Target, required []Capability) []Target {
	if len(required) == 0 {
		return nil
	}
	var out []Target
	seen := make(map[Target]struct{})
	for _, t := range r.configuredTargets() {
		if t == skip {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		p, ok := r.reg.Get(t.Provider)
		if !ok || t.Model == "" {
			continue
		}
		if down, _ := r.cachedDown(t.Provider); down {
			continue
		}
		caps, err := p.Capabilities(ctx, t.Model)
		if err != nil {
			continue
		}
		if caps.HasAll(required...) {
			out = append(out, t)
		}
	}
	return out
}

// configuredTargets lists every provider/model pair the configuration names,
// class targets first, in deterministic order.
func (r *Router) configuredTargets() []Target {
	out := make([]Target, 0, len(r.classes)+len(r.models))
	for _, c := range r.classOrder {
		out = append(out, r.classes[c])
	}
	return append(out, r.models...)
}

// reasonFor explains a successful choice in one line.
func (r *Router) reasonFor(sel Selection, class RouteClass, target Target, index int) string {
	var b strings.Builder
	switch {
	case sel.Provider != "" && index == 0:
		b.WriteString("provider selected manually")
	case sel.Model != "" && sel.Provider == "" && index == 0:
		b.WriteString("model selected manually")
	case index == 0:
		b.WriteString("routing class " + string(class))
	default:
		b.WriteString(fmt.Sprintf("fallback #%d after earlier candidates failed", index))
	}
	if len(sel.Required) > 0 {
		b.WriteString("; capabilities satisfied: " + strings.Join(Capabilities(sel.Required).Strings(), ","))
	}
	return b.String()
}

// routingError builds a normalized error for a routing failure.
func (r *Router) routingError(category ErrorCategory, target Target, message string) *Error {
	return &Error{
		Category: category,
		Provider: target.Provider,
		Model:    target.Model,
		Message:  message,
		Detail:   "model router",
	}
}

// Health probes a provider and caches the verdict.
//
// The cache is what keeps a stopped local server from being dialled on every
// request; a healthy verdict is cached for a shorter window so recovery is
// noticed quickly.
func (r *Router) Health(ctx context.Context, name string) error {
	if entry, ok := r.cachedHealth(name); ok {
		return entry.err
	}
	p, ok := r.reg.Get(name)
	if !ok {
		return r.routingError(ErrInvalidRequest, Target{Provider: name}, fmt.Sprintf("provider %q is not registered", name))
	}
	err := p.Health(ctx)
	r.storeHealth(name, err)
	return err
}

// MarkDown records an observed failure so later routing skips the provider
// until UnhealthyTTL elapses.
func (r *Router) MarkDown(name string, err error) {
	if err == nil {
		err = r.routingError(ErrUnavailable, Target{Provider: name}, fmt.Sprintf("%s reported a failure", name))
	}
	r.storeHealth(name, err)
}

// MarkUp records that a provider answered successfully.
func (r *Router) MarkUp(name string) { r.storeHealth(name, nil) }

// HealthSnapshot reports the cached health verdicts, for status output.
// A nil value means the provider was last seen healthy.
func (r *Router) HealthSnapshot() map[string]error {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	out := make(map[string]error, len(r.health))
	for name, entry := range r.health {
		if r.expired(entry, now) {
			continue
		}
		out[name] = entry.err
	}
	return out
}

func (r *Router) storeHealth(name string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.health[name] = healthEntry{err: err, at: r.now()}
}

func (r *Router) cachedHealth(name string) (healthEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.health[name]
	if !ok || r.expired(entry, r.now()) {
		return healthEntry{}, false
	}
	return entry, true
}

// cachedDown reports whether a fresh cache entry says the provider is down.
func (r *Router) cachedDown(name string) (bool, string) {
	entry, ok := r.cachedHealth(name)
	if !ok || entry.err == nil {
		return false, ""
	}
	return true, entry.err.Error()
}

func (r *Router) expired(entry healthEntry, now time.Time) bool {
	ttl := r.healthTTL
	if entry.err != nil {
		ttl = r.unhealthyTTL
	}
	return now.Sub(entry.at) >= ttl
}

// record appends a decision to the bounded history.
func (r *Router) record(dec Decision) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.history = append(r.history, dec)
	if len(r.history) > r.histSize {
		r.history = append(r.history[:0], r.history[len(r.history)-r.histSize:]...)
	}
}

// Decisions returns the recorded routing decisions, most recent first (§9).
func (r *Router) Decisions() []Decision {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Decision, 0, len(r.history))
	for i := len(r.history) - 1; i >= 0; i-- {
		out = append(out, r.history[i])
	}
	return out
}

// LastDecision returns the most recent routing decision.
func (r *Router) LastDecision() (Decision, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.history) == 0 {
		return Decision{}, false
	}
	return r.history[len(r.history)-1], true
}

package permissions

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Errors returned by the approval Broker.
var (
	// ErrBrokerClosed is returned when the session is shutting down; pending
	// requests fail closed rather than being treated as approved.
	ErrBrokerClosed = errors.New("permissions: approval broker is closed")
	// ErrNoSuchApproval is returned by Resolve for an unknown or already
	// answered request.
	ErrNoSuchApproval = errors.New("permissions: no such pending approval")
)

// GrantScope describes how far an approval reaches.
type GrantScope string

const (
	// ScopeOnce approves a single request and nothing else.
	ScopeOnce GrantScope = "once"
	// ScopeSessionCategory approves every later action in the same category
	// for the rest of the session.
	ScopeSessionCategory GrantScope = "session.category"
	// ScopeSessionCommand approves later requests that are byte-for-byte
	// identical to this one for the rest of the session.
	ScopeSessionCommand GrantScope = "session.command"
)

// Grant is a remembered "allow for session" decision.
//
// Grants live in memory only. They are never written to disk and never
// survive a restart: a standing permission the user cannot see is exactly the
// kind of quiet privilege escalation the permission engine exists to prevent.
type Grant struct {
	Scope    GrantScope `json:"scope"`
	Category Category   `json:"category"`
	// Label is a human description for the UI, such as the command line that
	// was approved.
	Label     string    `json:"label"`
	GrantedAt time.Time `json:"granted_at"`

	key string
}

// PendingApproval is one outstanding request awaiting a user decision.
type PendingApproval struct {
	ID     string `json:"id"`
	Action Action `json:"action"`
	// Decision is the evaluator verdict that produced this request; its
	// Reason is what the UI should show alongside the action.
	Decision    Decision  `json:"decision"`
	RequestedAt time.Time `json:"requested_at"`
}

// ApprovalEventKind names a change to the approval queue.
type ApprovalEventKind string

const (
	// ApprovalAdded reports a new pending request.
	ApprovalAdded ApprovalEventKind = "added"
	// ApprovalResolved reports that a request was answered.
	ApprovalResolved ApprovalEventKind = "resolved"
	// ApprovalCancelled reports that a request went away without an answer,
	// because the caller's context ended or the broker closed.
	ApprovalCancelled ApprovalEventKind = "cancelled"
)

// ApprovalEvent notifies attached frontends of queue changes so the TUI,
// WebUI and CLI stay in sync (§50).
type ApprovalEvent struct {
	Kind     ApprovalEventKind `json:"kind"`
	Approval PendingApproval   `json:"approval"`
	Approved bool              `json:"approved,omitempty"`
	// Scope is the scope actually applied, which may be narrower than the
	// scope requested when the action was too dangerous to remember.
	Scope GrantScope `json:"scope,omitempty"`
}

// Broker mediates approvals between the core and whichever frontend is
// attached.
//
// The core blocks in Request; a frontend lists Pending and calls Resolve.
// Several frontends may be attached at once, which is why resolution is
// keyed by ID and broadcast to every subscriber. Broker is safe for
// concurrent use.
type Broker struct {
	mu      sync.Mutex
	closed  bool
	pending map[string]*pendingRequest
	order   []string
	grants  map[string]Grant
	subs    map[int]chan ApprovalEvent
	nextSub int

	baseCtx context.Context
	timeout time.Duration
	newID   func() string
}

type pendingRequest struct {
	approval PendingApproval
	result   chan approvalResult
}

type approvalResult struct {
	approved bool
	err      error
}

// BrokerOption configures a Broker.
type BrokerOption func(*Broker)

// WithContext sets the context used by Approve, which has no context of its
// own. Binding the session context here means shutdown cancels approvals that
// nobody is going to answer.
func WithContext(ctx context.Context) BrokerOption {
	return func(b *Broker) {
		if ctx != nil {
			b.baseCtx = ctx
		}
	}
}

// WithTimeout bounds how long Approve waits for a user. Zero means wait
// indefinitely, which is the right default for an interactive session.
func WithTimeout(d time.Duration) BrokerOption {
	return func(b *Broker) { b.timeout = d }
}

// WithIDFunc overrides request ID generation. It exists for deterministic
// tests; production uses UUIDs.
func WithIDFunc(fn func() string) BrokerOption {
	return func(b *Broker) {
		if fn != nil {
			b.newID = fn
		}
	}
}

// NewBroker returns an empty Broker.
func NewBroker(opts ...BrokerOption) *Broker {
	b := &Broker{
		pending: make(map[string]*pendingRequest),
		grants:  make(map[string]Grant),
		subs:    make(map[int]chan ApprovalEvent),
		baseCtx: context.Background(),
		newID:   func() string { return uuid.NewString() },
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Request publishes an approval request and blocks until it is answered, the
// context ends, or the broker closes. It returns true only on explicit
// approval.
//
// A matching session grant answers immediately without troubling the user.
func (b *Broker) Request(ctx context.Context, action Action) (bool, error) {
	return b.RequestDecision(ctx, action, Decision{Outcome: OutcomeConfirm})
}

// RequestDecision is Request with the evaluator's decision attached, so the
// approval UI can show why approval is being asked for.
func (b *Broker) RequestDecision(ctx context.Context, action Action, decision Decision) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return false, ErrBrokerClosed
	}
	if _, ok := b.matchGrantLocked(action); ok {
		b.mu.Unlock()
		return true, nil
	}
	req := &pendingRequest{
		approval: PendingApproval{
			ID:          b.newID(),
			Action:      action,
			Decision:    decision,
			RequestedAt: time.Now(),
		},
		result: make(chan approvalResult, 1),
	}
	id := req.approval.ID
	b.pending[id] = req
	b.order = append(b.order, id)
	b.publishLocked(ApprovalEvent{Kind: ApprovalAdded, Approval: req.approval})
	b.mu.Unlock()

	select {
	case res := <-req.result:
		return res.approved, res.err
	case <-ctx.Done():
		// The answer may have arrived in the same instant the context ended;
		// prefer a real decision over the cancellation.
		b.mu.Lock()
		_, stillPending := b.pending[id]
		if stillPending {
			b.removeLocked(id)
			b.publishLocked(ApprovalEvent{Kind: ApprovalCancelled, Approval: req.approval})
		}
		b.mu.Unlock()
		if !stillPending {
			select {
			case res := <-req.result:
				return res.approved, res.err
			default:
			}
		}
		return false, ctx.Err()
	}
}

// Approve implements Approver, so the Broker can be handed to the core
// directly while frontends serve the queue.
func (b *Broker) Approve(action Action) (bool, error) {
	ctx := b.baseCtx
	if b.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, b.timeout)
		defer cancel()
	}
	return b.Request(ctx, action)
}

// Pending lists outstanding requests, oldest first.
func (b *Broker) Pending() []PendingApproval {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]PendingApproval, 0, len(b.pending))
	for _, id := range b.order {
		if req, ok := b.pending[id]; ok {
			out = append(out, req.approval)
		}
	}
	return out
}

// Get returns one pending request by ID.
func (b *Broker) Get(id string) (PendingApproval, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	req, ok := b.pending[id]
	if !ok {
		return PendingApproval{}, false
	}
	return req.approval, true
}

// Resolve answers a pending request once.
func (b *Broker) Resolve(id string, approved bool) error {
	return b.ResolveWithScope(id, approved, ScopeOnce)
}

// ResolveWithScope answers a pending request and optionally remembers the
// answer for the rest of the session ("always for session" in the UI).
//
// A session scope is only honoured for approvals that are safe to repeat
// unattended: production-affecting or critical-risk actions are always
// downgraded to a single approval, because "yes, forever" is not a decision a
// user should be able to make by accident about production. The scope that
// was actually applied is reported on the resulting ApprovalEvent.
func (b *Broker) ResolveWithScope(id string, approved bool, scope GrantScope) error {
	b.mu.Lock()
	req, ok := b.pending[id]
	if !ok {
		b.mu.Unlock()
		return ErrNoSuchApproval
	}
	b.removeLocked(id)

	applied := ScopeOnce
	if approved && scope != ScopeOnce && CanGrantSession(req.approval.Action) {
		applied = scope
		b.addGrantLocked(req.approval.Action, scope)
	}
	req.result <- approvalResult{approved: approved}
	b.publishLocked(ApprovalEvent{
		Kind:     ApprovalResolved,
		Approval: req.approval,
		Approved: approved,
		Scope:    applied,
	})
	b.mu.Unlock()
	return nil
}

// Close cancels every pending request and rejects new ones. Pending callers
// receive ErrBrokerClosed rather than a silent approval.
func (b *Broker) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for _, id := range b.order {
		req, ok := b.pending[id]
		if !ok {
			continue
		}
		delete(b.pending, id)
		req.result <- approvalResult{err: ErrBrokerClosed}
		b.publishLocked(ApprovalEvent{Kind: ApprovalCancelled, Approval: req.approval})
	}
	b.order = nil
	for id, ch := range b.subs {
		close(ch)
		delete(b.subs, id)
	}
}

// Subscribe returns a channel of approval queue changes plus a function that
// unsubscribes and is safe to call more than once.
//
// Sends are non-blocking: a frontend that stops reading loses events rather
// than stalling the core, so Pending remains the source of truth after any
// reconnect.
func (b *Broker) Subscribe() (<-chan ApprovalEvent, func()) {
	ch := make(chan ApprovalEvent, 32)
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
	id := b.nextSub
	b.nextSub++
	b.subs[id] = ch
	b.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			if sub, ok := b.subs[id]; ok {
				delete(b.subs, id)
				close(sub)
			}
			b.mu.Unlock()
		})
	}
}

// ---------------------------------------------------------------------------
// Session memory
// ---------------------------------------------------------------------------

// CanGrantSession reports whether an action may be remembered for the
// session. Production work and critical risk always require a fresh decision.
func CanGrantSession(action Action) bool {
	if isProduction(action) {
		return false
	}
	return action.Risk != RiskCritical
}

// AllowCategoryForSession remembers approval for a whole category until the
// session ends. It reports whether the grant was accepted; production changes
// never are.
func (b *Broker) AllowCategoryForSession(cat Category) bool {
	if cat == CatProductionChange || cat == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.grants[categoryGrantKey(cat)] = Grant{
		Scope:     ScopeSessionCategory,
		Category:  cat,
		Label:     categoryLabel(cat),
		GrantedAt: time.Now(),
		key:       categoryGrantKey(cat),
	}
	return true
}

// AllowActionForSession remembers approval for repeats of exactly this
// action. Matching is exact - same tool, category and detail - because
// anything looser would approve commands the user never saw.
func (b *Broker) AllowActionForSession(action Action) bool {
	if !CanGrantSession(action) {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.addGrantLocked(action, ScopeSessionCommand)
	return true
}

// SessionGrants lists the standing grants, so a frontend can show the user
// what they have already waved through.
func (b *Broker) SessionGrants() []Grant {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Grant, 0, len(b.grants))
	for _, g := range b.grants {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GrantedAt.Equal(out[j].GrantedAt) {
			return out[i].key < out[j].key
		}
		return out[i].GrantedAt.Before(out[j].GrantedAt)
	})
	return out
}

// ClearSessionGrants forgets every "allow for session" decision.
func (b *Broker) ClearSessionGrants() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.grants = make(map[string]Grant)
}

// RevokeSessionGrant forgets one grant, identified by category for category
// grants or by the action for command grants.
func (b *Broker) RevokeSessionGrant(g Grant) bool {
	key := g.key
	if key == "" {
		if g.Scope == ScopeSessionCategory {
			key = categoryGrantKey(g.Category)
		} else {
			return false
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.grants[key]; !ok {
		return false
	}
	delete(b.grants, key)
	return true
}

func (b *Broker) addGrantLocked(action Action, scope GrantScope) {
	switch scope {
	case ScopeSessionCategory:
		if action.Category == "" || action.Category == CatProductionChange {
			return
		}
		key := categoryGrantKey(action.Category)
		b.grants[key] = Grant{
			Scope:     ScopeSessionCategory,
			Category:  action.Category,
			Label:     categoryLabel(action.Category),
			GrantedAt: time.Now(),
			key:       key,
		}
	case ScopeSessionCommand:
		key := actionGrantKey(action)
		b.grants[key] = Grant{
			Scope:     ScopeSessionCommand,
			Category:  action.Category,
			Label:     grantLabel(action),
			GrantedAt: time.Now(),
			key:       key,
		}
	}
}

func (b *Broker) matchGrantLocked(action Action) (Grant, bool) {
	if !CanGrantSession(action) {
		return Grant{}, false
	}
	if g, ok := b.grants[actionGrantKey(action)]; ok {
		return g, true
	}
	if g, ok := b.grants[categoryGrantKey(action.Category)]; ok {
		return g, true
	}
	return Grant{}, false
}

func (b *Broker) removeLocked(id string) {
	delete(b.pending, id)
	for i, existing := range b.order {
		if existing == id {
			b.order = append(b.order[:i], b.order[i+1:]...)
			break
		}
	}
}

func (b *Broker) publishLocked(ev ApprovalEvent) {
	for _, ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

func categoryGrantKey(cat Category) string { return "category\x00" + string(cat) }

func actionGrantKey(action Action) string {
	detail := action.Detail
	if detail == "" {
		detail = action.Summary
	}
	return strings.Join([]string{"action", action.Tool, string(action.Category), detail}, "\x00")
}

func grantLabel(action Action) string {
	if action.Detail != "" {
		return action.Detail
	}
	if action.Summary != "" {
		return action.Summary
	}
	return categoryLabel(action.Category)
}

// ---------------------------------------------------------------------------
// Simple approvers
// ---------------------------------------------------------------------------

// ApproverFunc adapts a function to the Approver interface, so a plain CLI can
// supply a prompt without defining a type.
type ApproverFunc func(Action) (bool, error)

// Approve implements Approver.
func (f ApproverFunc) Approve(action Action) (bool, error) { return f(action) }

// DenyAll returns an Approver that refuses everything. It is the correct
// default for non-interactive automation (§52), where nobody is present to
// answer and silence must not mean yes.
func DenyAll() Approver {
	return ApproverFunc(func(Action) (bool, error) { return false, nil })
}

// AllowAll returns an Approver that accepts everything. It exists for tests
// and for explicitly unattended runs; it must never be the default.
func AllowAll() Approver {
	return ApproverFunc(func(Action) (bool, error) { return true, nil })
}

// Package agent implements Boop's agent runtime and task scheduler (§10, §11).
//
// Three things live here and nothing else does:
//
//   - Agent — a first-class runtime object with a validated status lifecycle
//     that publishes every change onto the event bus, so the TUI and the WebUI
//     can render live fleet state without polling.
//   - Scheduler — bounded, dependency-aware, write-conflict-aware concurrent
//     execution of tasks.
//   - Coordinator — ownership of the fleet: spawn, list, stop, wait, aggregate.
//
// The package deliberately does not reimplement the think/act/repair cycle.
// That is app.Loop's job; a worker runs one, over an isolated context and a
// restricted tool set (see context.go).
package agent

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/kawaiipantsu/boop/internal/app"
)

// AgentStatus is an agent's lifecycle state. The set is fixed by §10.
type AgentStatus string

// The agent statuses defined in §10.
const (
	// StatusIdle is a spawned agent that has not begun work.
	StatusIdle AgentStatus = "idle"
	// StatusPlanning is decomposing an objective into tasks.
	StatusPlanning AgentStatus = "planning"
	// StatusThinking is waiting on a model response.
	StatusThinking AgentStatus = "thinking"
	// StatusWorking is the general "doing its task" state.
	StatusWorking AgentStatus = "working"
	// StatusWaiting is blocked on something external, typically an approval.
	StatusWaiting AgentStatus = "waiting"
	// StatusRunning is executing a command through the run tool.
	StatusRunning AgentStatus = "running"
	// StatusTesting is validating its own work.
	StatusTesting AgentStatus = "testing"
	// StatusBlocked is unable to proceed, typically because a dependency
	// failed. It is not terminal: the blockage may clear.
	StatusBlocked AgentStatus = "blocked"
	// StatusError is terminal: the agent failed.
	StatusError AgentStatus = "error"
	// StatusComplete is terminal: the agent finished its task.
	StatusComplete AgentStatus = "complete"
	// StatusCancelled is terminal: the agent was stopped.
	StatusCancelled AgentStatus = "cancelled"
)

// statusOrder is every status in the order §10 lists them. It backs Valid and
// gives frontends a stable enumeration to render.
var statusOrder = []AgentStatus{
	StatusIdle, StatusPlanning, StatusThinking, StatusWorking, StatusWaiting,
	StatusRunning, StatusTesting, StatusBlocked, StatusError, StatusComplete,
	StatusCancelled,
}

// AllStatuses returns every valid status in specification order.
func AllStatuses() []AgentStatus { return append([]AgentStatus(nil), statusOrder...) }

// Valid reports whether s is one of the statuses §10 defines.
func (s AgentStatus) Valid() bool {
	for _, known := range statusOrder {
		if s == known {
			return true
		}
	}
	return false
}

// Terminal reports whether s ends the agent's life. A terminal agent never
// transitions again — that is the rule that stops a finished agent being
// resurrected into "working" by a late event.
func (s AgentStatus) Terminal() bool {
	switch s {
	case StatusError, StatusComplete, StatusCancelled:
		return true
	}
	return false
}

// Active reports whether s means the agent is occupying a concurrency slot.
func (s AgentStatus) Active() bool { return s.Valid() && !s.Terminal() && s != StatusIdle }

// CanTransitionTo reports whether s may become next.
//
// The rules are deliberately few, because inventing a precise state machine for
// eleven overlapping "kinds of busy" would reject legitimate sequences (an
// agent may think, run, test, then think again). What must never happen is
// exactly two things:
//
//   - a terminal agent changing state at all, and
//   - an agent that has started returning to idle.
func (s AgentStatus) CanTransitionTo(next AgentStatus) bool {
	if !s.Valid() || !next.Valid() {
		return false
	}
	if s == next {
		return true // a no-op repeat is harmless
	}
	if s.Terminal() {
		return false
	}
	return next != StatusIdle
}

// ErrInvalidTransition reports a rejected status change.
var ErrInvalidTransition = errors.New("invalid agent status transition")

// Agent is a first-class runtime object (§10).
//
// The exported fields are the shape the specification fixes. They are written
// under an internal lock, so anything reading an agent that may be running must
// go through Snapshot or the accessor methods rather than touching the fields —
// a live Agent is shared between the coordinator, the worker goroutine and
// whatever frontend is rendering it.
type Agent struct {
	ID         string
	Name       string
	Task       string
	Provider   string
	Model      string
	Status     AgentStatus
	ParentID   string
	StartedAt  time.Time
	FinishedAt time.Time

	mu     sync.RWMutex
	depth  int
	rootID string
	output string
	err    error

	bus       *app.Bus
	sessionID string
	now       func() time.Time
}

// AgentSpec describes an agent to create.
type AgentSpec struct {
	// ID overrides the generated identifier. Normally empty.
	ID string
	// Name is the short label shown in /agents list.
	Name string
	// Task is the scoped objective this agent owns.
	Task string
	// Provider and Model pin the agent's model. Empty inherits the session's.
	Provider string
	Model    string
	// ParentID is the spawning agent, empty for a top-level agent.
	ParentID string
	// Depth is the agent's nesting level; top-level agents are depth 1. It is
	// carried on the agent so the recursion cap is a property of the tree
	// rather than of a counter someone has to remember to decrement.
	Depth int
	// RootID identifies the agent tree. Empty means the agent is its own root.
	RootID string
	// Status is the initial status; empty means idle.
	Status AgentStatus
	// Bus receives lifecycle events. Nil disables publishing.
	Bus *app.Bus
	// SessionID labels published events.
	SessionID string
	// Now supplies timestamps; nil uses time.Now.
	Now func() time.Time
	// Silent suppresses the app.EventAgentCreated that NewAgent otherwise
	// publishes, leaving it to the caller's Announce. The coordinator sets it
	// because publishing is synchronous: a subscriber that calls back into the
	// coordinator while the coordinator held its own lock would deadlock.
	Silent bool
}

// NewAgent creates an agent and publishes app.EventAgentCreated.
func NewAgent(spec AgentSpec) *Agent {
	now := spec.Now
	if now == nil {
		now = time.Now
	}
	status := spec.Status
	if status == "" || !status.Valid() {
		status = StatusIdle
	}
	id := spec.ID
	if id == "" {
		id = uuid.NewString()
	}
	root := spec.RootID
	if root == "" {
		root = id
	}
	depth := spec.Depth
	if depth < 1 {
		depth = 1
	}
	a := &Agent{
		ID:        id,
		Name:      spec.Name,
		Task:      spec.Task,
		Provider:  spec.Provider,
		Model:     spec.Model,
		Status:    status,
		ParentID:  spec.ParentID,
		StartedAt: now(),
		depth:     depth,
		rootID:    root,
		bus:       spec.Bus,
		sessionID: spec.SessionID,
		now:       now,
	}
	if a.Name == "" {
		a.Name = shortID(a.ID)
	}
	if !spec.Silent {
		a.Announce()
	}
	return a
}

// Announce publishes app.EventAgentCreated for an agent built with Silent set.
// Calling it twice publishes twice; the coordinator calls it exactly once,
// after it has released its own lock.
func (a *Agent) Announce() {
	a.mu.RLock()
	info := a.snapshotLocked()
	a.mu.RUnlock()
	a.publish(app.EventAgentCreated, info)
}

// Depth returns the agent's nesting level; top-level agents are depth 1.
func (a *Agent) Depth() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.depth
}

// RootID returns the identifier of the agent tree this agent belongs to.
func (a *Agent) RootID() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.rootID
}

// SessionID returns the identifier of the session this agent belongs to.
func (a *Agent) SessionID() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.sessionID
}

// State returns the current status. It is named State rather than Status
// because the specification already fixes Status as a field name.
func (a *Agent) State() AgentStatus {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Status
}

// Err returns the failure that ended the agent, or nil.
func (a *Agent) Err() error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.err
}

// Output returns the agent's reported result.
func (a *Agent) Output() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.output
}

// SetStatus moves the agent to next, publishing app.EventAgentStatusChanged.
//
// It returns an error wrapping ErrInvalidTransition when the move is not
// allowed; the current status is then unchanged.
func (a *Agent) SetStatus(next AgentStatus) error {
	return a.transition(next, "", nil)
}

// Complete marks the agent finished and records what it produced.
func (a *Agent) Complete(output string) error { return a.transition(StatusComplete, output, nil) }

// Fail marks the agent failed and records why.
func (a *Agent) Fail(err error) error { return a.transition(StatusError, "", err) }

// Cancel marks the agent stopped. Cancelling an already-terminal agent is a
// no-op rather than an error: stopping something that just finished is a race
// the caller cannot avoid and should not have to handle.
func (a *Agent) Cancel() error {
	a.mu.RLock()
	terminal := a.Status.Terminal()
	a.mu.RUnlock()
	if terminal {
		return nil
	}
	return a.transition(StatusCancelled, "", nil)
}

func (a *Agent) transition(next AgentStatus, output string, cause error) error {
	a.mu.Lock()
	from := a.Status
	if !from.CanTransitionTo(next) {
		a.mu.Unlock()
		return fmt.Errorf("%w: agent %s cannot move from %s to %s", ErrInvalidTransition, shortID(a.ID), from, next)
	}
	a.Status = next
	if output != "" {
		a.output = output
	}
	if cause != nil {
		a.err = cause
	}
	if next.Terminal() && a.FinishedAt.IsZero() {
		a.FinishedAt = a.now()
	}
	change := StatusChange{
		AgentID: a.ID,
		Name:    a.Name,
		From:    from,
		To:      next,
		At:      a.now(),
	}
	if cause != nil {
		change.Error = cause.Error()
	}
	unchanged := from == next
	a.mu.Unlock()

	if !unchanged {
		a.publish(app.EventAgentStatusChanged, change)
	}
	return nil
}

// StatusChange is the payload of app.EventAgentStatusChanged.
type StatusChange struct {
	AgentID string      `json:"agent_id"`
	Name    string      `json:"name,omitempty"`
	From    AgentStatus `json:"from"`
	To      AgentStatus `json:"to"`
	Error   string      `json:"error,omitempty"`
	At      time.Time   `json:"at"`
}

// Duration is how long the agent has been alive, or how long it lived.
func (a *Agent) Duration() time.Duration {
	a.mu.RLock()
	defer a.mu.RUnlock()
	end := a.FinishedAt
	if end.IsZero() {
		end = a.now()
	}
	return end.Sub(a.StartedAt)
}

// Snapshot returns a copy safe to serialise, log or hand to a frontend.
func (a *Agent) Snapshot() AgentInfo {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.snapshotLocked()
}

func (a *Agent) snapshotLocked() AgentInfo {
	info := AgentInfo{
		ID:         a.ID,
		Name:       a.Name,
		Task:       a.Task,
		Provider:   a.Provider,
		Model:      a.Model,
		Status:     a.Status,
		ParentID:   a.ParentID,
		RootID:     a.rootID,
		Depth:      a.depth,
		StartedAt:  a.StartedAt,
		FinishedAt: a.FinishedAt,
		Output:     a.output,
	}
	if a.err != nil {
		info.Error = a.err.Error()
	}
	end := a.FinishedAt
	if end.IsZero() {
		end = a.now()
	}
	info.Duration = end.Sub(a.StartedAt)
	return info
}

func (a *Agent) publish(t app.EventType, payload any) {
	if a.bus == nil {
		return
	}
	a.bus.Publish(app.Event{Type: t, SessionID: a.sessionID, AgentID: a.ID, Payload: payload})
}

// shortID trims a UUID to something readable in a TUI header and in
// `/agents stop <id>`, which nobody wants to type in full.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

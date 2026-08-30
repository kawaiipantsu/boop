package agent

import (
	"context"
	"errors"

	"github.com/kawaiipantsu/boop/internal/app"
	"github.com/kawaiipantsu/boop/internal/tools"
)

// TaskRunner carries out one agent's task.
//
// The coordinator depends on this interface rather than on app.Loop directly,
// which is what lets the whole scheduler be tested without a provider, a model
// or the network.
type TaskRunner interface {
	RunTask(ctx context.Context, a *Agent, brief Brief) (TaskOutcome, error)
}

// TaskRunnerFunc adapts a function to TaskRunner.
type TaskRunnerFunc func(ctx context.Context, a *Agent, brief Brief) (TaskOutcome, error)

// RunTask implements TaskRunner.
func (f TaskRunnerFunc) RunTask(ctx context.Context, a *Agent, brief Brief) (TaskOutcome, error) {
	return f(ctx, a, brief)
}

// LoopFactory builds the think/act/repair loop a worker runs.
//
// It is a factory rather than a shared loop because each worker needs its own
// tool registry and its own event label; the argument is the identifier the
// loop should stamp on the events it emits.
type LoopFactory func(sessionID string) *app.Loop

// LoopRunner is the production TaskRunner: one app.Loop per worker, over an
// isolated starting history and a restricted tool registry.
//
// It does not reimplement the loop. Everything about model calls, permission
// evaluation, tool execution and error repair stays in app.Loop; this type only
// decides what the worker is allowed to see and touch.
type LoopRunner struct {
	// Loops builds the loop. Required.
	Loops LoopFactory
	// Tools is the full registry to restrict from. Nil uses the registry the
	// factory's loop already carries.
	Tools *tools.Registry
	// SystemPrompt overrides the embedded worker prompt.
	SystemPrompt string
	// MaxIterations overrides the loop's tool-iteration bound for workers.
	// Zero keeps whatever the factory configured.
	MaxIterations int
}

// ErrNoLoop reports a runner with no way to build a loop.
var ErrNoLoop = errors.New("agent: no loop factory is configured")

// RunTask implements TaskRunner.
func (r *LoopRunner) RunTask(ctx context.Context, a *Agent, brief Brief) (TaskOutcome, error) {
	if r == nil || r.Loops == nil {
		return TaskOutcome{}, ErrNoLoop
	}
	sessionID := a.SessionID()
	if sessionID == "" {
		sessionID = a.ID
	}
	base := r.Loops(sessionID)
	if base == nil {
		return TaskOutcome{}, ErrNoLoop
	}

	// Copy so per-worker settings never mutate a loop the caller reuses.
	loop := *base
	loop.SessionID = sessionID
	loop.AgentID = a.ID

	source := r.Tools
	if source == nil {
		source = base.Tools
	}
	// The allowlist is enforced here, structurally: the worker's registry
	// contains only the granted tools, so the restriction holds for what the
	// model is offered *and* for what can be dispatched if it invents a name.
	loop.Tools = RestrictTools(source, brief.AllowedTools)

	if r.MaxIterations > 0 {
		loop.MaxIterations = r.MaxIterations
	}
	if a.Provider != "" {
		loop.Selection.Provider = a.Provider
	}
	if a.Model != "" {
		loop.Selection.Model = a.Model
	}

	_ = a.SetStatus(StatusThinking)
	turn, err := loop.Run(ctx, brief.Messages(r.SystemPrompt))
	outcome := TaskOutcome{AgentID: a.ID}
	if turn != nil {
		outcome.Output = turn.Text
		outcome.Usage = turn.Usage
		outcome.ToolCalls = turn.ToolCalls
		outcome.Truncated = turn.StoppedAtLimit
	}
	if err != nil {
		return outcome, err
	}
	return outcome, nil
}

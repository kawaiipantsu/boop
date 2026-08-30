package tui

import (
	"context"
	"sync"

	"github.com/kawaiipantsu/boop/internal/permissions"
)

// Approver serves the runtime's confirmation requests from the terminal UI.
//
// The tool loop calls Approve on its own goroutine and must block until the
// operator answers, while the UI can only answer on the update goroutine. The
// hand-off is the permission Broker: Approve parks in the broker's queue, the
// UI renders whatever is pending and calls Resolve, and the parked call wakes
// with the answer. No UI state is touched from the loop goroutine and no core
// logic runs on the render goroutine.
//
// The second job is cancellation. Approve has no context parameter, so an
// interrupted turn would otherwise leave the loop parked on a prompt the user
// has already walked away from. Approver therefore tracks the context of the
// turn in flight and parks on that, so cancelling the turn releases a pending
// approval as a denial (§51).
type Approver struct {
	broker *permissions.Broker

	mu        sync.RWMutex
	evaluator *permissions.Evaluator
	turnCtx   context.Context
}

// NewApprover returns an Approver backed by broker.
func NewApprover(broker *permissions.Broker) *Approver {
	return &Approver{broker: broker, turnCtx: context.Background()}
}

// Broker exposes the queue the UI resolves against.
func (a *Approver) Broker() *permissions.Broker { return a.broker }

// SetEvaluator attaches the policy evaluator.
//
// It is set after construction because the runtime builds the evaluator, and
// the runtime cannot be built without an Approver. The evaluator is only read,
// and only to explain to the user *why* they are being asked.
func (a *Approver) SetEvaluator(e *permissions.Evaluator) {
	a.mu.Lock()
	a.evaluator = e
	a.mu.Unlock()
}

// SetTurnContext binds later approvals to the lifetime of the current turn.
func (a *Approver) SetTurnContext(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	a.mu.Lock()
	a.turnCtx = ctx
	a.mu.Unlock()
}

// ResetTurnContext restores the background context once a turn is over, so a
// later approval is not tied to the finished turn's cancellation.
func (a *Approver) ResetTurnContext() { a.SetTurnContext(context.Background()) }

// Approve implements permissions.Approver. It blocks until the operator
// answers, the turn is cancelled, or the broker closes.
func (a *Approver) Approve(action permissions.Action) (bool, error) {
	a.mu.RLock()
	ctx, eval := a.turnCtx, a.evaluator
	a.mu.RUnlock()

	decision := permissions.Decision{
		Outcome: permissions.OutcomeConfirm,
		Reason:  "this action needs your approval",
	}
	if eval != nil {
		decision = eval.Evaluate(action)
	}
	if a.broker == nil {
		return false, permissions.ErrBrokerClosed
	}
	return a.broker.RequestDecision(ctx, action, decision)
}

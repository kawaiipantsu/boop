package tui

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kawaiipantsu/boop/internal/permissions"
)

// answer runs Approve on its own goroutine, the way the tool loop does.
func answer(a *Approver, action permissions.Action) <-chan struct {
	ok  bool
	err error
} {
	out := make(chan struct {
		ok  bool
		err error
	}, 1)
	go func() {
		ok, err := a.Approve(action)
		out <- struct {
			ok  bool
			err error
		}{ok, err}
	}()
	return out
}

func waitPending(t *testing.T, broker *permissions.Broker) permissions.PendingApproval {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pending := broker.Pending(); len(pending) > 0 {
			return pending[0]
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for a pending approval")
	return permissions.PendingApproval{}
}

func TestApproverBridgesTheLoopToTheUI(t *testing.T) {
	broker := permissions.NewBroker()
	approver := NewApprover(broker)
	action := permissions.Action{Tool: "run", Category: permissions.CatShellExecute, Risk: permissions.RiskLow}

	result := answer(approver, action)
	pending := waitPending(t, broker)
	if pending.Action.Tool != "run" {
		t.Fatalf("pending action = %+v", pending.Action)
	}
	if err := broker.ResolveWithScope(pending.ID, true, permissions.ScopeOnce); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	select {
	case got := <-result:
		if !got.ok || got.err != nil {
			t.Fatalf("Approve = (%v, %v)", got.ok, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Approve never returned")
	}
}

func TestApproverAttachesTheEvaluatorsReason(t *testing.T) {
	broker := permissions.NewBroker()
	approver := NewApprover(broker)
	approver.SetEvaluator(permissions.NewEvaluator(permissions.DefaultPolicy()))

	action := permissions.Action{Tool: "run", Category: permissions.CatShellExecute, Risk: permissions.RiskMedium,
		Summary: "run a command", Detail: "go test ./..."}
	result := answer(approver, action)
	pending := waitPending(t, broker)
	if pending.Decision.Reason == "" {
		t.Fatal("the approval carries no reason for the UI to show")
	}
	_ = broker.Resolve(pending.ID, false)
	<-result
}

func TestApproverReleasesOnTurnCancellation(t *testing.T) {
	// §51: interrupting a turn must not leave the loop parked on a prompt the
	// user has walked away from.
	broker := permissions.NewBroker()
	approver := NewApprover(broker)
	ctx, cancel := context.WithCancel(context.Background())
	approver.SetTurnContext(ctx)

	result := answer(approver, permissions.Action{Tool: "run", Risk: permissions.RiskLow})
	waitPending(t, broker)
	cancel()

	select {
	case got := <-result:
		if got.ok {
			t.Fatal("a cancelled approval must not read as consent")
		}
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Approve never returned after cancellation")
	}
	if len(broker.Pending()) != 0 {
		t.Fatal("the cancelled request is still queued")
	}
}

func TestApproverFailsClosedWhenTheBrokerCloses(t *testing.T) {
	broker := permissions.NewBroker()
	approver := NewApprover(broker)

	result := answer(approver, permissions.Action{Tool: "run", Risk: permissions.RiskLow})
	waitPending(t, broker)
	broker.Close()

	select {
	case got := <-result:
		if got.ok {
			t.Fatal("shutting down must never read as consent")
		}
		if !errors.Is(got.err, permissions.ErrBrokerClosed) {
			t.Fatalf("err = %v, want ErrBrokerClosed", got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Approve never returned after the broker closed")
	}
}

func TestApproverResetTurnContextRestoresBackground(t *testing.T) {
	broker := permissions.NewBroker()
	approver := NewApprover(broker)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	approver.SetTurnContext(ctx)
	approver.ResetTurnContext()

	result := answer(approver, permissions.Action{Tool: "run", Risk: permissions.RiskLow})
	pending := waitPending(t, broker)
	_ = broker.Resolve(pending.ID, true)
	if got := <-result; !got.ok {
		t.Fatalf("Approve = (%v, %v)", got.ok, got.err)
	}
}

func TestApproverWithoutABrokerRefuses(t *testing.T) {
	approver := NewApprover(nil)
	ok, err := approver.Approve(permissions.Action{Tool: "run"})
	if ok || err == nil {
		t.Fatalf("Approve = (%v, %v), want a refusal", ok, err)
	}
}

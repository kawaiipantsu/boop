package permissions

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
)

// The Broker is the frontend-facing half of the Approver contract.
var _ Approver = (*Broker)(nil)

func testAction(detail string) Action {
	return Action{
		Category: CatShellExecute,
		Risk:     RiskMedium,
		Tool:     "run",
		Summary:  "Run: " + detail,
		Detail:   detail,
	}
}

// waitForPending blocks until the broker has n pending requests.
func waitForPending(t *testing.T, b *Broker, n int) []PendingApproval {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		pending := b.Pending()
		if len(pending) == n {
			return pending
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d pending approvals, have %d", n, len(pending))
		}
		time.Sleep(time.Millisecond)
	}
}

func TestBrokerApproveAndDeny(t *testing.T) {
	tests := []struct {
		name     string
		approved bool
	}{
		{"approved", true},
		{"denied", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := NewBroker()
			type result struct {
				ok  bool
				err error
			}
			results := make(chan result, 1)
			go func() {
				ok, err := b.Request(context.Background(), testAction("go test ./..."))
				results <- result{ok, err}
			}()

			pending := waitForPending(t, b, 1)
			if pending[0].Action.Detail != "go test ./..." {
				t.Errorf("pending action detail = %q", pending[0].Action.Detail)
			}
			if err := b.Resolve(pending[0].ID, tt.approved); err != nil {
				t.Fatalf("Resolve: %v", err)
			}

			select {
			case got := <-results:
				if got.err != nil {
					t.Fatalf("Request returned error: %v", got.err)
				}
				if got.ok != tt.approved {
					t.Errorf("Request = %v, want %v", got.ok, tt.approved)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Request did not return after Resolve")
			}
			if len(b.Pending()) != 0 {
				t.Error("resolved request is still pending")
			}
		})
	}
}

func TestBrokerRequestDecisionCarriesReason(t *testing.T) {
	b := NewBroker()
	decision := Decision{Outcome: OutcomeConfirm, Rule: RuleConfirm, Reason: "because"}
	go func() {
		_, _ = b.RequestDecision(context.Background(), testAction("ls"), decision)
	}()
	pending := waitForPending(t, b, 1)
	if pending[0].Decision.Reason != "because" {
		t.Errorf("decision reason = %q, want %q", pending[0].Decision.Reason, "because")
	}
	if pending[0].RequestedAt.IsZero() {
		t.Error("RequestedAt was not stamped")
	}
	if got, ok := b.Get(pending[0].ID); !ok || got.ID != pending[0].ID {
		t.Error("Get did not return the pending approval")
	}
	if _, ok := b.Get("nope"); ok {
		t.Error("Get returned an unknown approval")
	}
	_ = b.Resolve(pending[0].ID, false)
}

func TestBrokerContextCancellation(t *testing.T) {
	b := NewBroker()
	ctx, cancel := context.WithCancel(context.Background())

	errs := make(chan error, 1)
	go func() {
		_, err := b.Request(ctx, testAction("sleep 1000"))
		errs <- err
	}()
	waitForPending(t, b, 1)
	cancel()

	select {
	case err := <-errs:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Request did not return after cancellation")
	}
	if len(b.Pending()) != 0 {
		t.Error("cancelled request is still pending")
	}
}

func TestBrokerAlreadyCancelledContext(t *testing.T) {
	b := NewBroker()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ok, err := b.Request(ctx, testAction("ls"))
	if ok || !errors.Is(err, context.Canceled) {
		t.Fatalf("got (%v, %v), want (false, context.Canceled)", ok, err)
	}
	if len(b.Pending()) != 0 {
		t.Error("a cancelled request should never be queued")
	}
}

func TestBrokerTimeout(t *testing.T) {
	b := NewBroker(WithTimeout(20 * time.Millisecond))
	ok, err := b.Approve(testAction("ls"))
	if ok {
		t.Error("Approve returned true on timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
}

func TestBrokerApproveUsesBoundContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	b := NewBroker(WithContext(ctx))
	errs := make(chan error, 1)
	go func() {
		_, err := b.Approve(testAction("ls"))
		errs <- err
	}()
	waitForPending(t, b, 1)
	cancel()
	select {
	case err := <-errs:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Approve did not observe the bound context")
	}
}

func TestBrokerClose(t *testing.T) {
	b := NewBroker()
	errs := make(chan error, 1)
	go func() {
		_, err := b.Request(context.Background(), testAction("ls"))
		errs <- err
	}()
	waitForPending(t, b, 1)
	b.Close()

	select {
	case err := <-errs:
		if !errors.Is(err, ErrBrokerClosed) {
			t.Fatalf("err = %v, want ErrBrokerClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Request did not return after Close")
	}

	if _, err := b.Request(context.Background(), testAction("ls")); !errors.Is(err, ErrBrokerClosed) {
		t.Fatalf("request after close: %v, want ErrBrokerClosed", err)
	}
	b.Close() // must be idempotent
}

func TestBrokerResolveUnknown(t *testing.T) {
	b := NewBroker()
	if err := b.Resolve("missing", true); !errors.Is(err, ErrNoSuchApproval) {
		t.Fatalf("Resolve(missing) = %v, want ErrNoSuchApproval", err)
	}

	go func() { _, _ = b.Request(context.Background(), testAction("ls")) }()
	pending := waitForPending(t, b, 1)
	if err := b.Resolve(pending[0].ID, true); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	if err := b.Resolve(pending[0].ID, true); !errors.Is(err, ErrNoSuchApproval) {
		t.Fatalf("second Resolve = %v, want ErrNoSuchApproval", err)
	}
}

func TestBrokerSessionGrants(t *testing.T) {
	t.Run("category grant skips later prompts", func(t *testing.T) {
		b := NewBroker()
		go func() { _, _ = b.Request(context.Background(), testAction("go test ./...")) }()
		pending := waitForPending(t, b, 1)
		if err := b.ResolveWithScope(pending[0].ID, true, ScopeSessionCategory); err != nil {
			t.Fatalf("ResolveWithScope: %v", err)
		}

		ok, err := b.Request(context.Background(), testAction("go vet ./..."))
		if !ok || err != nil {
			t.Fatalf("granted category should not prompt: (%v, %v)", ok, err)
		}
		grants := b.SessionGrants()
		if len(grants) != 1 || grants[0].Scope != ScopeSessionCategory {
			t.Fatalf("grants = %+v", grants)
		}

		b.ClearSessionGrants()
		if len(b.SessionGrants()) != 0 {
			t.Error("ClearSessionGrants left grants behind")
		}
	})

	t.Run("command grant matches only identical commands", func(t *testing.T) {
		b := NewBroker()
		action := testAction("go test ./...")
		if !b.AllowActionForSession(action) {
			t.Fatal("AllowActionForSession refused a medium-risk action")
		}
		ok, err := b.Request(context.Background(), action)
		if !ok || err != nil {
			t.Fatalf("identical command should be pre-approved: (%v, %v)", ok, err)
		}

		other := testAction("go test ./... -run TestX")
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if ok, err := b.Request(ctx, other); ok || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a different command must still prompt: (%v, %v)", ok, err)
		}
	})

	t.Run("category grants never cover production", func(t *testing.T) {
		b := NewBroker()
		if b.AllowCategoryForSession(CatProductionChange) {
			t.Error("production changes must not be grantable for a session")
		}
		if b.AllowCategoryForSession("") {
			t.Error("the empty category must not be grantable")
		}
	})

	t.Run("dangerous actions are not remembered", func(t *testing.T) {
		cases := []struct {
			name   string
			action Action
		}{
			{"critical", Action{Category: CatShellExecute, Risk: RiskCritical, Tool: "run", Detail: "rm -rf /"}},
			{"production flag", Action{Category: CatShellExecute, Risk: RiskLow, Production: true, Tool: "run", Detail: "kubectl get pods"}},
			{"production category", Action{Category: CatProductionChange, Risk: RiskLow, Tool: "run", Detail: "helm list"}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				b := NewBroker()
				if b.AllowActionForSession(tc.action) {
					t.Fatal("AllowActionForSession accepted an action that must always be re-approved")
				}
				go func() { _, _ = b.Request(context.Background(), tc.action) }()
				pending := waitForPending(t, b, 1)
				if err := b.ResolveWithScope(pending[0].ID, true, ScopeSessionCommand); err != nil {
					t.Fatalf("ResolveWithScope: %v", err)
				}
				if len(b.SessionGrants()) != 0 {
					t.Fatalf("a session grant was recorded for %s", tc.name)
				}
				// The next identical request must prompt again.
				ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
				defer cancel()
				if ok, _ := b.Request(ctx, tc.action); ok {
					t.Fatal("second request was auto-approved")
				}
			})
		}
	})

	t.Run("denial is never remembered", func(t *testing.T) {
		b := NewBroker()
		go func() { _, _ = b.Request(context.Background(), testAction("ls")) }()
		pending := waitForPending(t, b, 1)
		if err := b.ResolveWithScope(pending[0].ID, false, ScopeSessionCategory); err != nil {
			t.Fatalf("ResolveWithScope: %v", err)
		}
		if len(b.SessionGrants()) != 0 {
			t.Error("a rejection created a session grant")
		}
	})

	t.Run("revoke", func(t *testing.T) {
		b := NewBroker()
		if !b.AllowCategoryForSession(CatFilesystemWrite) {
			t.Fatal("category grant refused")
		}
		grants := b.SessionGrants()
		if len(grants) != 1 {
			t.Fatalf("grants = %+v", grants)
		}
		if !b.RevokeSessionGrant(grants[0]) {
			t.Error("RevokeSessionGrant returned false for a known grant")
		}
		if b.RevokeSessionGrant(grants[0]) {
			t.Error("RevokeSessionGrant returned true twice")
		}
		if b.RevokeSessionGrant(Grant{Scope: ScopeSessionCommand}) {
			t.Error("RevokeSessionGrant accepted an unidentifiable grant")
		}
	})
}

func TestBrokerSubscribe(t *testing.T) {
	b := NewBroker()
	events, unsubscribe := b.Subscribe()
	defer unsubscribe()

	go func() { _, _ = b.Request(context.Background(), testAction("ls")) }()
	pending := waitForPending(t, b, 1)

	added := <-events
	if added.Kind != ApprovalAdded || added.Approval.ID != pending[0].ID {
		t.Fatalf("first event = %+v", added)
	}

	if err := b.Resolve(pending[0].ID, true); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	resolved := <-events
	if resolved.Kind != ApprovalResolved || !resolved.Approved || resolved.Scope != ScopeOnce {
		t.Fatalf("second event = %+v", resolved)
	}

	unsubscribe()
	unsubscribe() // idempotent
	if _, ok := <-events; ok {
		t.Error("channel was not closed on unsubscribe")
	}

	// A subscriber attached after Close gets a closed channel, not a hang.
	b.Close()
	closedCh, cancel := b.Subscribe()
	defer cancel()
	if _, ok := <-closedCh; ok {
		t.Error("subscribing to a closed broker should yield a closed channel")
	}
}

func TestBrokerSlowSubscriberDoesNotBlock(t *testing.T) {
	b := NewBroker()
	_, unsubscribe := b.Subscribe() // never drained
	defer unsubscribe()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
			_, _ = b.Request(ctx, testAction(fmt.Sprintf("cmd-%d", i)))
			cancel()
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a subscriber that stopped reading blocked the core")
	}
}

func TestBrokerConcurrentRequests(t *testing.T) {
	const n = 50
	b := NewBroker()

	var wg sync.WaitGroup
	approvals := make(chan bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := b.Request(context.Background(), testAction(fmt.Sprintf("cmd-%d", i)))
			if err != nil {
				t.Errorf("Request %d: %v", i, err)
				return
			}
			approvals <- ok
		}(i)
	}

	// A frontend resolving the queue concurrently: even IDs approved.
	resolved := 0
	deadline := time.Now().Add(10 * time.Second)
	for resolved < n {
		if time.Now().After(deadline) {
			t.Fatalf("only resolved %d of %d requests", resolved, n)
		}
		for _, p := range b.Pending() {
			if err := b.Resolve(p.ID, true); err == nil {
				resolved++
			}
		}
	}
	wg.Wait()
	close(approvals)

	got := 0
	for ok := range approvals {
		if !ok {
			t.Error("a request was denied although every resolution approved")
		}
		got++
	}
	if got != n {
		t.Errorf("%d requests returned, want %d", got, n)
	}
	if len(b.Pending()) != 0 {
		t.Errorf("%d requests left pending", len(b.Pending()))
	}
}

func TestBrokerPendingOrderIsStable(t *testing.T) {
	b := NewBroker()
	for i := 0; i < 3; i++ {
		detail := fmt.Sprintf("cmd-%d", i)
		go func() { _, _ = b.Request(context.Background(), testAction(detail)) }()
		waitForPending(t, b, i+1)
	}
	pending := b.Pending()
	for i, p := range pending {
		if want := fmt.Sprintf("cmd-%d", i); p.Action.Detail != want {
			t.Errorf("pending[%d] = %q, want %q", i, p.Action.Detail, want)
		}
	}
	b.Close()
}

func TestSimpleApprovers(t *testing.T) {
	if ok, err := DenyAll().Approve(testAction("ls")); ok || err != nil {
		t.Errorf("DenyAll = (%v, %v), want (false, nil)", ok, err)
	}
	if ok, err := AllowAll().Approve(testAction("ls")); !ok || err != nil {
		t.Errorf("AllowAll = (%v, %v), want (true, nil)", ok, err)
	}
	sentinel := errors.New("boom")
	fn := ApproverFunc(func(a Action) (bool, error) {
		if a.Detail != "ls" {
			t.Errorf("action not passed through: %+v", a)
		}
		return false, sentinel
	})
	if _, err := fn.Approve(testAction("ls")); !errors.Is(err, sentinel) {
		t.Errorf("ApproverFunc did not propagate the error: %v", err)
	}
}

func TestCanGrantSession(t *testing.T) {
	tests := []struct {
		name   string
		action Action
		want   bool
	}{
		{"ordinary shell command", Action{Category: CatShellExecute, Risk: RiskMedium}, true},
		{"high risk is still grantable", Action{Category: CatShellExecute, Risk: RiskHigh}, true},
		{"critical is not", Action{Category: CatShellExecute, Risk: RiskCritical}, false},
		{"production flag is not", Action{Category: CatShellExecute, Risk: RiskLow, Production: true}, false},
		{"production category is not", Action{Category: CatProductionChange, Risk: RiskLow}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CanGrantSession(tt.action); got != tt.want {
				t.Errorf("CanGrantSession = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBrokerDoesNotLeakGoroutinesOnCancellation(t *testing.T) {
	b := NewBroker()
	settle := func() int {
		for i := 0; i < 50; i++ {
			runtime.GC()
			time.Sleep(2 * time.Millisecond)
		}
		return runtime.NumGoroutine()
	}
	before := settle()
	for i := 0; i < 200; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		go cancel()
		_, _ = b.Request(ctx, testAction(fmt.Sprintf("cmd-%d", i)))
	}
	if after := settle(); after > before+5 {
		t.Errorf("goroutine count grew from %d to %d across 200 cancelled requests", before, after)
	}
	if len(b.Pending()) != 0 {
		t.Errorf("%d cancelled requests left pending", len(b.Pending()))
	}
}

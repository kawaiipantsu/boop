package agent

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kawaiipantsu/boop/internal/app"
)

// collector records bus events for assertions.
type collector struct {
	mu     sync.Mutex
	events []app.Event
}

func (c *collector) handle(ev app.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
}

func (c *collector) of(t app.EventType) []app.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []app.Event
	for _, ev := range c.events {
		if ev.Type == t {
			out = append(out, ev)
		}
	}
	return out
}

func newBusWithCollector() (*app.Bus, *collector) {
	bus := app.NewBus()
	c := &collector{}
	bus.Subscribe(c.handle)
	return bus, c
}

func TestAgentStatusClassification(t *testing.T) {
	tests := []struct {
		status   AgentStatus
		valid    bool
		terminal bool
		active   bool
	}{
		{StatusIdle, true, false, false},
		{StatusPlanning, true, false, true},
		{StatusThinking, true, false, true},
		{StatusWorking, true, false, true},
		{StatusWaiting, true, false, true},
		{StatusRunning, true, false, true},
		{StatusTesting, true, false, true},
		{StatusBlocked, true, false, true},
		{StatusError, true, true, false},
		{StatusComplete, true, true, false},
		{StatusCancelled, true, true, false},
		{AgentStatus("busy"), false, false, false},
		{AgentStatus(""), false, false, false},
	}
	for _, tc := range tests {
		t.Run(string(tc.status), func(t *testing.T) {
			if got := tc.status.Valid(); got != tc.valid {
				t.Errorf("Valid() = %v, want %v", got, tc.valid)
			}
			if got := tc.status.Terminal(); got != tc.terminal {
				t.Errorf("Terminal() = %v, want %v", got, tc.terminal)
			}
			if got := tc.status.Active(); got != tc.active {
				t.Errorf("Active() = %v, want %v", got, tc.active)
			}
		})
	}
	if got := len(AllStatuses()); got != 11 {
		t.Errorf("AllStatuses() has %d entries, want the 11 defined by §10", got)
	}
}

func TestAgentStatusTransitions(t *testing.T) {
	tests := []struct {
		name     string
		from, to AgentStatus
		want     bool
	}{
		{"idle starts working", StatusIdle, StatusWorking, true},
		{"working thinks", StatusWorking, StatusThinking, true},
		{"thinking runs", StatusThinking, StatusRunning, true},
		{"running tests", StatusRunning, StatusTesting, true},
		{"testing blocks", StatusTesting, StatusBlocked, true},
		{"blocked recovers", StatusBlocked, StatusWorking, true},
		{"working completes", StatusWorking, StatusComplete, true},
		{"working fails", StatusWorking, StatusError, true},
		{"working cancels", StatusWorking, StatusCancelled, true},
		{"idle cancels", StatusIdle, StatusCancelled, true},
		{"repeat is a no-op", StatusWorking, StatusWorking, true},
		{"complete cannot work again", StatusComplete, StatusWorking, false},
		{"complete cannot fail", StatusComplete, StatusError, false},
		{"error cannot recover", StatusError, StatusWorking, false},
		{"cancelled cannot resume", StatusCancelled, StatusRunning, false},
		{"cannot return to idle", StatusWorking, StatusIdle, false},
		{"unknown target rejected", StatusWorking, AgentStatus("busy"), false},
		{"unknown source rejected", AgentStatus("busy"), StatusWorking, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.from.CanTransitionTo(tc.to); got != tc.want {
				t.Errorf("%s.CanTransitionTo(%s) = %v, want %v", tc.from, tc.to, got, tc.want)
			}
		})
	}
}

func TestNewAgentPublishesCreated(t *testing.T) {
	bus, c := newBusWithCollector()
	a := NewAgent(AgentSpec{Name: "worker", Task: "do the thing", Bus: bus, SessionID: "s1"})

	created := c.of(app.EventAgentCreated)
	if len(created) != 1 {
		t.Fatalf("got %d created events, want 1", len(created))
	}
	if created[0].AgentID != a.ID {
		t.Errorf("AgentID = %q, want %q", created[0].AgentID, a.ID)
	}
	if created[0].SessionID != "s1" {
		t.Errorf("SessionID = %q, want s1", created[0].SessionID)
	}
	info, ok := created[0].Payload.(AgentInfo)
	if !ok {
		t.Fatalf("payload is %T, want AgentInfo", created[0].Payload)
	}
	if info.Status != StatusIdle {
		t.Errorf("Status = %q, want idle", info.Status)
	}
	if a.Depth() != 1 {
		t.Errorf("Depth() = %d, want 1 for a top-level agent", a.Depth())
	}
	if a.RootID() != a.ID {
		t.Errorf("RootID() = %q, want its own id", a.RootID())
	}
}

func TestNewAgentSilentDefersAnnouncement(t *testing.T) {
	bus, c := newBusWithCollector()
	a := NewAgent(AgentSpec{Task: "quiet", Bus: bus, Silent: true})
	if got := len(c.of(app.EventAgentCreated)); got != 0 {
		t.Fatalf("silent agent published %d created events, want 0", got)
	}
	a.Announce()
	if got := len(c.of(app.EventAgentCreated)); got != 1 {
		t.Fatalf("after Announce got %d created events, want 1", got)
	}
}

func TestAgentSetStatusPublishesChange(t *testing.T) {
	bus, c := newBusWithCollector()
	a := NewAgent(AgentSpec{Task: "t", Bus: bus, SessionID: "s1"})

	if err := a.SetStatus(StatusWorking); err != nil {
		t.Fatalf("SetStatus(working) = %v", err)
	}
	// A repeat must not spam the bus.
	if err := a.SetStatus(StatusWorking); err != nil {
		t.Fatalf("repeat SetStatus = %v", err)
	}
	if err := a.Complete("done"); err != nil {
		t.Fatalf("Complete() = %v", err)
	}

	changes := c.of(app.EventAgentStatusChanged)
	if len(changes) != 2 {
		t.Fatalf("got %d status events, want 2", len(changes))
	}
	first, ok := changes[0].Payload.(StatusChange)
	if !ok {
		t.Fatalf("payload is %T, want StatusChange", changes[0].Payload)
	}
	if first.From != StatusIdle || first.To != StatusWorking {
		t.Errorf("first change = %s -> %s, want idle -> working", first.From, first.To)
	}
	if a.Output() != "done" {
		t.Errorf("Output() = %q, want done", a.Output())
	}
	if a.Snapshot().FinishedAt.IsZero() {
		t.Error("FinishedAt is zero after Complete")
	}
}

func TestAgentRejectsTransitionAfterTerminal(t *testing.T) {
	a := NewAgent(AgentSpec{Task: "t"})
	if err := a.Complete("done"); err != nil {
		t.Fatalf("Complete() = %v", err)
	}
	err := a.SetStatus(StatusWorking)
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("SetStatus after complete = %v, want ErrInvalidTransition", err)
	}
	if a.State() != StatusComplete {
		t.Errorf("State() = %q, want complete — a rejected transition must not change anything", a.State())
	}
}

func TestAgentFailRecordsCause(t *testing.T) {
	sentinel := errors.New("boom")
	a := NewAgent(AgentSpec{Task: "t"})
	if err := a.Fail(sentinel); err != nil {
		t.Fatalf("Fail() = %v", err)
	}
	if !errors.Is(a.Err(), sentinel) {
		t.Errorf("Err() = %v, want %v", a.Err(), sentinel)
	}
	if got := a.Snapshot().Error; got != "boom" {
		t.Errorf("Snapshot().Error = %q, want boom", got)
	}
}

func TestAgentCancelIsIdempotentOnTerminal(t *testing.T) {
	a := NewAgent(AgentSpec{Task: "t"})
	if err := a.Complete("done"); err != nil {
		t.Fatalf("Complete() = %v", err)
	}
	// Stopping something that just finished is a race the caller cannot avoid.
	if err := a.Cancel(); err != nil {
		t.Errorf("Cancel() on a complete agent = %v, want nil", err)
	}
	if a.State() != StatusComplete {
		t.Errorf("State() = %q, want complete", a.State())
	}
}

func TestAgentDurationUsesInjectedClock(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	now := base
	a := NewAgent(AgentSpec{Task: "t", Now: func() time.Time { return now }})
	now = base.Add(3 * time.Second)
	if got := a.Duration(); got != 3*time.Second {
		t.Errorf("Duration() = %v, want 3s", got)
	}
	if err := a.Complete("x"); err != nil {
		t.Fatalf("Complete() = %v", err)
	}
	now = base.Add(10 * time.Second)
	if got := a.Duration(); got != 3*time.Second {
		t.Errorf("Duration() after finishing = %v, want it frozen at 3s", got)
	}
}

// TestAgentConcurrentAccess is the -race guard: statuses change while
// frontends read snapshots, which is exactly what the TUI will do.
func TestAgentConcurrentAccess(t *testing.T) {
	bus, _ := newBusWithCollector()
	a := NewAgent(AgentSpec{Task: "t", Bus: bus})

	var wg sync.WaitGroup
	states := []AgentStatus{StatusWorking, StatusThinking, StatusRunning, StatusTesting}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = a.SetStatus(states[(i+j)%len(states)])
			}
		}(i)
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = a.Snapshot()
				_ = a.State()
				_ = a.Duration()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = a.Cancel()
	}()
	wg.Wait()

	if !a.State().Valid() {
		t.Errorf("State() = %q, want a valid status", a.State())
	}
}

package tui

import (
	"errors"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kawaiipantsu/boop/internal/app"
	"github.com/kawaiipantsu/boop/internal/permissions"
)

func TestTranslate(t *testing.T) {
	tests := []struct {
		name    string
		event   app.Event
		wantOK  bool
		want    uiEvent
		wantStr string
	}{
		{
			name:   "token",
			event:  app.Event{Type: app.EventModelToken, Payload: "hi"},
			wantOK: true, want: uiEvent{kind: evToken, text: "hi"},
		},
		{
			name:   "reasoning",
			event:  app.Event{Type: app.EventModelReasoning, Payload: "thinking"},
			wantOK: true, want: uiEvent{kind: evReasoning, text: "thinking"},
		},
		{
			name: "tool requested prefers the detail",
			event: app.Event{Type: app.EventToolRequested, Payload: permissions.Action{
				Tool: "run", Summary: "run a command", Detail: "go test ./...",
			}},
			wantOK: true, want: uiEvent{kind: evToolRequested, tool: "run", text: "go test ./..."},
		},
		{
			name: "tool completed parses its duration string",
			event: app.Event{Type: app.EventToolCompleted, Payload: map[string]any{
				"tool": "run", "error": true, "duration": "1.5s",
			}},
			wantOK: true, want: uiEvent{kind: evToolCompleted, tool: "run", isError: true, duration: 1500 * time.Millisecond},
		},
		{
			name: "denied approval closes the tool line",
			event: app.Event{Type: app.EventApprovalReceived, Payload: map[string]any{
				"tool": "run", "approved": false,
			}},
			wantOK: true, want: uiEvent{kind: evApprovalDecided, tool: "run"},
		},
		{
			name: "granted approval is not shown twice",
			event: app.Event{Type: app.EventApprovalReceived, Payload: map[string]any{
				"tool": "run", "approved": true,
			}},
			wantOK: false,
		},
		{
			name:   "error carries its text",
			event:  app.Event{Type: app.EventError, Payload: errors.New("boom")},
			wantOK: true, want: uiEvent{kind: evRuntimeError, text: "boom"},
		},
		{
			name:   "malformed payload degrades rather than panics",
			event:  app.Event{Type: app.EventToolCompleted, Payload: 42},
			wantOK: true, want: uiEvent{kind: evToolCompleted},
		},
		{
			name:   "unhandled type",
			event:  app.Event{Type: app.EventSessionStarted},
			wantOK: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := translate(tc.event)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Fatalf("event = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestPumpCoalescesTokenBursts(t *testing.T) {
	var mu sync.Mutex
	var wakes int
	p := newPump(func(tea.Msg) {
		mu.Lock()
		wakes++
		mu.Unlock()
	})

	for _, tok := range []string{"a", "b", "c"} {
		p.push(uiEvent{kind: evToken, text: tok})
	}
	events := p.drain()
	if len(events) != 1 {
		t.Fatalf("expected the tokens to merge, got %d events: %+v", len(events), events)
	}
	if events[0].text != "abc" {
		t.Fatalf("text = %q, want %q", events[0].text, "abc")
	}
}

func TestPumpKeepsDistinctEventsSeparate(t *testing.T) {
	p := newPump(nil)
	p.push(uiEvent{kind: evToken, text: "a"})
	p.push(uiEvent{kind: evToolRequested, tool: "run"})
	p.push(uiEvent{kind: evToken, text: "b"})
	events := p.drain()
	if len(events) != 3 {
		t.Fatalf("expected three events, got %+v", events)
	}
}

func TestPumpDoesNotMergeEventsCarryingMetadata(t *testing.T) {
	// Two completed tools must stay two lines even though the kinds match.
	p := newPump(nil)
	p.push(uiEvent{kind: evToolCompleted, tool: "read"})
	p.push(uiEvent{kind: evToolCompleted, tool: "run"})
	if got := len(p.drain()); got != 2 {
		t.Fatalf("events = %d, want 2", got)
	}
}

func TestPumpDrainReArmsTheWakeUp(t *testing.T) {
	woken := make(chan struct{}, 8)
	p := newPump(func(tea.Msg) { woken <- struct{}{} })

	p.push(uiEvent{kind: evToken, text: "a"})
	waitFor(t, woken)
	p.push(uiEvent{kind: evToken, text: "b"})
	select {
	case <-woken:
		t.Fatal("a second wake-up was sent before the first was drained")
	case <-time.After(20 * time.Millisecond):
	}

	if got := p.drain(); len(got) != 1 || got[0].text != "ab" {
		t.Fatalf("drain = %+v", got)
	}
	p.push(uiEvent{kind: evToken, text: "c"})
	waitFor(t, woken)
}

func TestPumpAttachForwardsBusEvents(t *testing.T) {
	bus := app.NewBus()
	p := newPump(nil)
	cancel := p.attach(bus)

	bus.Emit(app.EventModelToken, "s1", "hello")
	bus.Emit(app.EventSessionStarted, "s1", nil)
	events := p.drain()
	if len(events) != 1 || events[0].text != "hello" {
		t.Fatalf("events = %+v", events)
	}

	cancel()
	bus.Emit(app.EventModelToken, "s1", "ignored")
	if got := p.drain(); len(got) != 0 {
		t.Fatalf("expected nothing after unsubscribing, got %+v", got)
	}
}

func TestPumpAttachToleratesANilBus(t *testing.T) {
	p := newPump(nil)
	cancel := p.attach(nil)
	cancel()
}

func TestWatchApprovalsForwardsQueueChanges(t *testing.T) {
	broker := permissions.NewBroker()
	msgs := make(chan tea.Msg, 8)
	cancel := watchApprovals(broker, func(m tea.Msg) { msgs <- m })
	defer cancel()

	go func() {
		_, _ = broker.Approve(permissions.Action{Tool: "run", Risk: permissions.RiskLow})
	}()

	msg := receive(t, msgs)
	ev, ok := msg.(approvalMsg)
	if !ok || ev.event.Kind != permissions.ApprovalAdded {
		t.Fatalf("first message = %#v", msg)
	}
	if err := broker.Resolve(ev.event.Approval.ID, false); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	resolved := receive(t, msgs).(approvalMsg)
	if resolved.event.Kind != permissions.ApprovalResolved || resolved.event.Approved {
		t.Fatalf("resolution = %+v", resolved.event)
	}
}

func TestAsDuration(t *testing.T) {
	tests := []struct {
		in   any
		want time.Duration
	}{
		{"1.5s", 1500 * time.Millisecond},
		{time.Second, time.Second},
		{"nonsense", 0},
		{nil, 0},
		{42, 0},
	}
	for _, tc := range tests {
		if got := asDuration(tc.in); got != tc.want {
			t.Errorf("asDuration(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestAsString(t *testing.T) {
	tests := []struct {
		in   any
		want string
	}{
		{nil, ""},
		{"x", "x"},
		{[]byte("y"), "y"},
		{errors.New("z"), "z"},
		{123, ""},
	}
	for _, tc := range tests {
		if got := asString(tc.in); got != tc.want {
			t.Errorf("asString(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func waitFor(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a wake-up")
	}
}

func receive(t *testing.T, ch <-chan tea.Msg) tea.Msg {
	t.Helper()
	select {
	case msg := <-ch:
		return msg
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a message")
		return nil
	}
}

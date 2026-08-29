package tui

import (
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kawaiipantsu/boop/internal/app"
	"github.com/kawaiipantsu/boop/internal/permissions"
)

// uiEventKind names a runtime event after it has been translated out of the
// bus's `any` payloads into something the model can switch on safely.
type uiEventKind int

const (
	evToken uiEventKind = iota
	evReasoning
	evModelStarted
	evModelCompleted
	evToolRequested
	evToolCompleted
	evCommandOutput
	evCommandError
	evRuntimeError
	evAgentChanged
	evApprovalDecided
)

// uiEvent is a normalised bus event.
type uiEvent struct {
	kind     uiEventKind
	text     string
	tool     string
	duration time.Duration
	isError  bool
}

// flushMsg tells the model that the pump has events waiting. The events
// themselves are pulled from the pump rather than carried on the message, so
// a burst of tokens collapses into one redraw.
type flushMsg struct{}

// approvalMsg carries a change to the approval queue.
type approvalMsg struct{ event permissions.ApprovalEvent }

// pump buffers runtime events and wakes the Bubble Tea program.
//
// Two problems make this necessary. The bus publishes synchronously on the
// loop's goroutine, so a handler that blocks stalls model streaming; and
// tea.Program.Send blocks until the update loop reads, so calling it once per
// token would couple generation speed to render speed. The pump therefore
// queues events under a mutex, coalesces consecutive tokens into one string,
// and has at most one wake-up in flight at a time. Producers never wait for
// the UI, and the UI never processes a token one keystroke at a time.
type pump struct {
	mu      sync.Mutex
	queue   []uiEvent
	pending bool
	send    func(tea.Msg)
}

// newPump returns a pump that wakes the program through send.
func newPump(send func(tea.Msg)) *pump {
	if send == nil {
		send = func(tea.Msg) {}
	}
	return &pump{send: send}
}

// setSend installs the wake-up function once the program exists.
func (p *pump) setSend(send func(tea.Msg)) {
	if send == nil {
		return
	}
	p.mu.Lock()
	p.send = send
	p.mu.Unlock()
}

// push queues an event, merging it into the previous one when both are text
// from the same stream.
func (p *pump) push(ev uiEvent) {
	p.mu.Lock()
	if n := len(p.queue); n > 0 && mergeable(p.queue[n-1].kind, ev.kind) {
		p.queue[n-1].text += ev.text
	} else {
		p.queue = append(p.queue, ev)
	}
	wake := !p.pending
	p.pending = true
	send := p.send
	p.mu.Unlock()

	if wake {
		// Sending from a fresh goroutine keeps the publishing goroutine —
		// which is the model stream — free of any UI backpressure.
		go send(flushMsg{})
	}
}

// drain returns and clears the queued events, re-arming the wake-up.
func (p *pump) drain() []uiEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.queue
	p.queue = nil
	p.pending = false
	return out
}

// mergeable reports whether two consecutive events of these kinds can be
// concatenated. Only continuous text streams may merge; anything with its own
// metadata must stay a separate event.
func mergeable(prev, next uiEventKind) bool {
	if prev != next {
		return false
	}
	switch next {
	case evToken, evReasoning, evCommandOutput, evCommandError:
		return true
	default:
		return false
	}
}

// attach subscribes the pump to the runtime event bus and returns the
// unsubscribe function.
func (p *pump) attach(bus *app.Bus) func() {
	if bus == nil {
		return func() {}
	}
	return bus.Subscribe(func(ev app.Event) {
		if translated, ok := translate(ev); ok {
			p.push(translated)
		}
	})
}

// translate converts a bus event into a uiEvent, reporting false for events
// the transcript does not show.
//
// Payloads arrive as `any` because the bus is transport-neutral, so every
// assertion here is guarded: a payload of an unexpected shape degrades to a
// less detailed line rather than panicking the UI.
func translate(ev app.Event) (uiEvent, bool) {
	switch ev.Type {
	case app.EventModelToken:
		return uiEvent{kind: evToken, text: asString(ev.Payload)}, true
	case app.EventModelReasoning:
		return uiEvent{kind: evReasoning, text: asString(ev.Payload)}, true
	case app.EventModelRequestStarted:
		return uiEvent{kind: evModelStarted}, true
	case app.EventModelCompleted:
		return uiEvent{kind: evModelCompleted}, true
	case app.EventToolRequested:
		action, _ := ev.Payload.(permissions.Action)
		return uiEvent{kind: evToolRequested, tool: action.Tool, text: actionHeadline(action)}, true
	case app.EventToolCompleted:
		m, _ := ev.Payload.(map[string]any)
		u := uiEvent{kind: evToolCompleted}
		u.tool = asString(m["tool"])
		u.isError, _ = m["error"].(bool)
		u.duration = asDuration(m["duration"])
		return u, true
	case app.EventCommandStdout:
		return uiEvent{kind: evCommandOutput, text: asString(ev.Payload)}, true
	case app.EventCommandStderr:
		return uiEvent{kind: evCommandError, text: asString(ev.Payload)}, true
	case app.EventApprovalReceived:
		// A refusal from the policy table never reaches the broker, so this
		// is the only signal that the pending tool line will never complete.
		m, _ := ev.Payload.(map[string]any)
		approved, _ := m["approved"].(bool)
		if approved {
			return uiEvent{}, false
		}
		return uiEvent{kind: evApprovalDecided, tool: asString(m["tool"])}, true
	case app.EventError:
		return uiEvent{kind: evRuntimeError, text: asString(ev.Payload)}, true
	case app.EventAgentStatusChanged, app.EventAgentCreated:
		return uiEvent{kind: evAgentChanged, text: asString(ev.Payload)}, true
	default:
		return uiEvent{}, false
	}
}

// actionHeadline is the short description of a tool action for the transcript.
func actionHeadline(a permissions.Action) string {
	switch {
	case a.Detail != "" && a.Detail != a.Summary:
		return a.Detail
	case a.Summary != "":
		return a.Summary
	default:
		return string(a.Category)
	}
}

func asString(v any) string {
	switch s := v.(type) {
	case nil:
		return ""
	case string:
		return s
	case error:
		return s.Error()
	case []byte:
		return string(s)
	case interface{ String() string }:
		return s.String()
	default:
		return ""
	}
}

func asDuration(v any) time.Duration {
	switch d := v.(type) {
	case time.Duration:
		return d
	case string:
		parsed, err := time.ParseDuration(strings.TrimSpace(d))
		if err != nil {
			return 0
		}
		return parsed
	default:
		return 0
	}
}

// watchApprovals forwards approval queue changes to the program until the
// broker closes its channel.
//
// The queue, not this stream, is the source of truth: the broker drops events
// for a slow consumer, and the model recovers by asking for Pending().
func watchApprovals(broker *permissions.Broker, send func(tea.Msg)) func() {
	if broker == nil {
		return func() {}
	}
	ch, cancel := broker.Subscribe()
	go func() {
		for ev := range ch {
			send(approvalMsg{event: ev})
		}
	}()
	return cancel
}

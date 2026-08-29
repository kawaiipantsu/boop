// Package app owns process lifecycle and the transport-neutral event bus.
//
// The TUI, WebUI and plain CLI all subscribe to the same stream; this is what
// keeps frontends from accumulating business logic.
package app

import (
	"sync"
	"time"
)

// EventType names a core runtime event.
type EventType string

const (
	EventSessionStarted   EventType = "session.started"
	EventSessionCompleted EventType = "session.completed"
	EventPromptReceived   EventType = "prompt.received"

	EventModelRequestStarted EventType = "model.request.started"
	EventModelToken          EventType = "model.token"
	EventModelReasoning      EventType = "model.reasoning"
	EventModelCompleted      EventType = "model.response.completed"

	EventAgentCreated       EventType = "agent.created"
	EventAgentStatusChanged EventType = "agent.status.changed"

	EventToolRequested EventType = "tool.requested"
	EventToolCompleted EventType = "tool.completed"

	EventApprovalRequested EventType = "approval.requested"
	EventApprovalReceived  EventType = "approval.received"

	EventCommandStarted   EventType = "command.started"
	EventCommandStdout    EventType = "command.stdout"
	EventCommandStderr    EventType = "command.stderr"
	EventCommandCompleted EventType = "command.completed"

	EventTestStarted   EventType = "test.started"
	EventTestCompleted EventType = "test.completed"

	EventError EventType = "error"
)

// Event is one item on the bus.
//
// Payload is event-specific and must be JSON-serialisable so the WebUI can
// receive it unchanged over WebSocket.
type Event struct {
	Type      EventType `json:"type"`
	SessionID string    `json:"session_id,omitempty"`
	AgentID   string    `json:"agent_id,omitempty"`
	Payload   any       `json:"payload,omitempty"`
	At        time.Time `json:"at"`
}

// Handler receives events. It must not block; slow consumers should buffer.
type Handler func(Event)

// Bus is a synchronous in-process publish/subscribe hub. It is safe for
// concurrent use.
type Bus struct {
	mu   sync.RWMutex
	next int
	subs map[int]subscription
}

type subscription struct {
	handler Handler
	types   map[EventType]struct{}
}

// NewBus returns an empty bus.
func NewBus() *Bus {
	return &Bus{subs: make(map[int]subscription)}
}

// Subscribe registers h for the given event types; passing none subscribes to
// every event. The returned function unsubscribes and is safe to call twice.
func (b *Bus) Subscribe(h Handler, types ...EventType) (cancel func()) {
	var filter map[EventType]struct{}
	if len(types) > 0 {
		filter = make(map[EventType]struct{}, len(types))
		for _, t := range types {
			filter[t] = struct{}{}
		}
	}
	b.mu.Lock()
	id := b.next
	b.next++
	b.subs[id] = subscription{handler: h, types: filter}
	b.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, id)
			b.mu.Unlock()
		})
	}
}

// Publish delivers ev to every matching subscriber. At is stamped when unset.
func (b *Bus) Publish(ev Event) {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	b.mu.RLock()
	handlers := make([]Handler, 0, len(b.subs))
	for _, sub := range b.subs {
		if sub.types != nil {
			if _, ok := sub.types[ev.Type]; !ok {
				continue
			}
		}
		handlers = append(handlers, sub.handler)
	}
	b.mu.RUnlock()

	for _, h := range handlers {
		h(ev)
	}
}

// Emit is a convenience wrapper around Publish.
func (b *Bus) Emit(t EventType, sessionID string, payload any) {
	b.Publish(Event{Type: t, SessionID: sessionID, Payload: payload})
}

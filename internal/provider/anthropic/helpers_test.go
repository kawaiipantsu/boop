package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/boop-dev/boop/internal/provider"
)

// testAPIKey is an obvious fake. No test in this package may require a real
// credential or reach the network (§41).
const testAPIKey = "test-key"

// capture records what the fake server received, so assertions can be made on
// the request Boop actually built.
type capture struct {
	mu      sync.Mutex
	path    string
	method  string
	headers http.Header
	body    []byte
	hits    int
}

func (c *capture) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.path = r.URL.RequestURI()
	c.method = r.Method
	c.headers = r.Header.Clone()
	c.body = body
	c.hits++
}

func (c *capture) request(t *testing.T) map[string]any {
	t.Helper()
	c.mu.Lock()
	raw := append([]byte(nil), c.body...)
	c.mu.Unlock()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("request body is not JSON: %v\nbody: %s", err, raw)
	}
	return out
}

func (c *capture) header(name string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.headers.Get(name)
}

func (c *capture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits
}

// newTestClient starts a fake Anthropic endpoint served by handler and returns
// a Client pointed at it.
func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *capture) {
	t.Helper()
	return newTestClientWith(t, Options{}, handler)
}

// newTestClientWith is newTestClient with extra options; BaseURL, APIKey and
// HTTPClient are supplied here.
func newTestClientWith(t *testing.T, opts Options, handler http.HandlerFunc) (*Client, *capture) {
	t.Helper()
	cap := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	opts.BaseURL = srv.URL
	if opts.APIKey == "" {
		opts.APIKey = testAPIKey
	}
	opts.HTTPClient = srv.Client()
	return New(opts), cap
}

// jsonHandler replies with status and the given body.
func jsonHandler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// sseHandler replies with an SSE stream built from the given frames. Each frame
// is written verbatim followed by the blank line that terminates it.
func sseHandler(frames ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, frame := range frames {
			_, _ = io.WriteString(w, frame+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

// event builds one SSE frame in the shape Anthropic emits: an event: line and
// a data: line carrying the typed payload.
func event(name, data string) string {
	return "event: " + name + "\ndata: " + data
}

// collect drains a chat stream and asserts the terminal-event contract: the
// channel closes, and exactly one EventDone or EventError is emitted, last.
func collect(t *testing.T, ch <-chan provider.ChatEvent) []provider.ChatEvent {
	t.Helper()
	var events []provider.ChatEvent
	for ev := range ch {
		events = append(events, ev)
	}
	if len(events) == 0 {
		t.Fatal("stream produced no events; every stream must terminate in EventDone or EventError")
	}
	terminals := 0
	for i, ev := range events {
		if ev.Type != provider.EventDone && ev.Type != provider.EventError {
			continue
		}
		terminals++
		if i != len(events)-1 {
			t.Fatalf("terminal event at index %d of %d; it must be last", i, len(events))
		}
	}
	if terminals != 1 {
		t.Fatalf("got %d terminal events, want exactly 1: %v", terminals, types(events))
	}
	if events[len(events)-1].At.IsZero() {
		t.Error("events must carry a timestamp")
	}
	return events
}

func types(events []provider.ChatEvent) []provider.EventType {
	out := make([]provider.EventType, len(events))
	for i, ev := range events {
		out[i] = ev.Type
	}
	return out
}

// textOf concatenates every EventDelta payload.
func textOf(events []provider.ChatEvent) string {
	var b strings.Builder
	for _, ev := range events {
		if ev.Type == provider.EventDelta {
			b.WriteString(ev.Text)
		}
	}
	return b.String()
}

// reasoningOf concatenates every EventReasoning payload.
func reasoningOf(events []provider.ChatEvent) string {
	var b strings.Builder
	for _, ev := range events {
		if ev.Type == provider.EventReasoning {
			b.WriteString(ev.Text)
		}
	}
	return b.String()
}

// toolCallsOf returns every assembled tool call.
func toolCallsOf(events []provider.ChatEvent) []provider.ToolCall {
	var out []provider.ToolCall
	for _, ev := range events {
		if ev.Type == provider.EventToolCall && ev.ToolCall != nil {
			out = append(out, *ev.ToolCall)
		}
	}
	return out
}

// usageOf returns the single usage event, if any.
func usageOf(events []provider.ChatEvent) *provider.Usage {
	for _, ev := range events {
		if ev.Type == provider.EventUsage {
			return ev.Usage
		}
	}
	return nil
}

// terminalError returns the error carried by a terminal EventError.
func terminalError(t *testing.T, events []provider.ChatEvent) error {
	t.Helper()
	last := events[len(events)-1]
	if last.Type != provider.EventError {
		t.Fatalf("last event is %q, want an error event", last.Type)
	}
	if last.Err == nil {
		t.Fatal("error event carries no error")
	}
	return last.Err
}

// chatOnce runs one chat request against the client and returns the events.
func chatOnce(t *testing.T, c *Client, req provider.ChatRequest) []provider.ChatEvent {
	t.Helper()
	ch, err := c.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	return collect(t, ch)
}

// userReq is the minimal valid request.
func userReq(text string) provider.ChatRequest {
	return provider.ChatRequest{
		Model:    "claude-test-model",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: text}},
	}
}

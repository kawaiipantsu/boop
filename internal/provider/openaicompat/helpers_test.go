package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/boop-dev/boop/internal/provider"
)

// newTestServer starts an httptest server and returns a Client pointed at it.
func newTestServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *Client) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client := New(Options{
		Name:    "testprov",
		BaseURL: srv.URL + "/v1",
		Timeout: 5 * time.Second,
	})
	return srv, client
}

// sseWriter writes flushed Server-Sent Events frames.
type sseWriter struct {
	t  *testing.T
	w  http.ResponseWriter
	fl http.Flusher
}

func newSSEWriter(t *testing.T, w http.ResponseWriter) *sseWriter {
	t.Helper()
	fl, ok := w.(http.Flusher)
	if !ok {
		t.Fatal("test response writer does not support flushing")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl.Flush()
	return &sseWriter{t: t, w: w, fl: fl}
}

// send writes a single data frame.
func (s *sseWriter) send(payload string) {
	s.t.Helper()
	if _, err := io.WriteString(s.w, "data: "+payload+"\n\n"); err != nil {
		return
	}
	s.fl.Flush()
}

// done writes the terminating [DONE] frame.
func (s *sseWriter) done() { s.send("[DONE]") }

// collect drains an event channel, failing if it does not close in time.
func collect(t *testing.T, ch <-chan provider.ChatEvent) []provider.ChatEvent {
	t.Helper()
	var got []provider.ChatEvent
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("timed out draining events; got %d so far", len(got))
		}
	}
}

// types returns the event types in order, for compact assertions.
func types(events []provider.ChatEvent) []provider.EventType {
	out := make([]provider.EventType, len(events))
	for i, ev := range events {
		out[i] = ev.Type
	}
	return out
}

// textOf concatenates the text of every event of the given type.
func textOf(events []provider.ChatEvent, kind provider.EventType) string {
	var out string
	for _, ev := range events {
		if ev.Type == kind {
			out += ev.Text
		}
	}
	return out
}

// eventsOf filters events by type.
func eventsOf(events []provider.ChatEvent, kind provider.EventType) []provider.ChatEvent {
	var out []provider.ChatEvent
	for _, ev := range events {
		if ev.Type == kind {
			out = append(out, ev)
		}
	}
	return out
}

// decodeBody reads and decodes a JSON request body in a handler.
func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode request body %q: %v", raw, err)
	}
	return out
}

// simpleRequest is a minimal valid chat request for tests that only care about
// the response side.
func simpleRequest(stream bool) provider.ChatRequest {
	return provider.ChatRequest{
		Model:    "test-model",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hello"}},
		Stream:   stream,
	}
}

// waitForGoroutines polls until the goroutine count drops back to at most
// baseline+slack, which catches a stream goroutine that never returned.
func waitForGoroutines(t *testing.T, baseline, slack int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		runtime.GC()
		got := runtime.NumGoroutine()
		if got <= baseline+slack {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine leak: baseline %d, still running %d", baseline, got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// asProviderError extracts the normalized provider error from err.
func asProviderError(err error, target **provider.Error) bool {
	return errors.As(err, target)
}

// mustChat starts a chat and fails on a construction error.
func mustChat(t *testing.T, ctx context.Context, c *Client, req provider.ChatRequest) <-chan provider.ChatEvent {
	t.Helper()
	ch, err := c.Chat(ctx, req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	return ch
}

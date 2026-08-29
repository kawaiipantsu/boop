package openaicompat

import (
	"context"
	"encoding/json"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kawaiipantsu/boop/internal/provider"
)

func TestStreamContentDeltas(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("Accept = %q, want text/event-stream", got)
		}
		sse := newSSEWriter(t, w)
		sse.send(`{"choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}`)
		sse.send(`{"choices":[{"index":0,"delta":{"content":"lo, "}}]}`)
		sse.send(`{"choices":[{"index":0,"delta":{"content":"world"}}]}`)
		sse.send(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		sse.done()
	})

	events := collect(t, mustChat(t, context.Background(), client, simpleRequest(true)))

	if got, want := textOf(events, provider.EventDelta), "Hello, world"; got != want {
		t.Errorf("assembled text = %q, want %q", got, want)
	}
	if got := len(eventsOf(events, provider.EventDelta)); got != 3 {
		t.Errorf("delta events = %d, want 3", got)
	}
	assertSingleDone(t, events, provider.FinishStop)
}

func TestStreamReasoningFieldVariants(t *testing.T) {
	tests := []struct {
		name  string
		frame string
		want  string
	}{
		{"reasoning_content", `{"choices":[{"delta":{"reasoning_content":"step one"}}]}`, "step one"},
		{"reasoning", `{"choices":[{"delta":{"reasoning":"step two"}}]}`, "step two"},
		{"thinking", `{"choices":[{"delta":{"thinking":"step three"}}]}`, "step three"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				sse := newSSEWriter(t, w)
				sse.send(tc.frame)
				sse.send(`{"choices":[{"delta":{"content":"answer"},"finish_reason":"stop"}]}`)
				sse.done()
			})
			events := collect(t, mustChat(t, context.Background(), client, simpleRequest(true)))
			if got := textOf(events, provider.EventReasoning); got != tc.want {
				t.Errorf("reasoning = %q, want %q", got, tc.want)
			}
			if got := textOf(events, provider.EventDelta); got != "answer" {
				t.Errorf("answer = %q, want %q", got, "answer")
			}
		})
	}
}

// TestStreamFragmentedToolCall covers the OpenAI behaviour where the id, the
// function name and the arguments each arrive across several chunks.
func TestStreamFragmentedToolCall(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		sse := newSSEWriter(t, w)
		sse.send(`{"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`)
		sse.send(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_","type":"function","function":{"name":"get_","arguments":""}}]}}]}`)
		sse.send(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"abc123","function":{"name":"weather"}}]}}]}`)
		sse.send(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}`)
		sse.send(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Copenhagen\"}"}}]}}]}`)
		sse.send(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
		sse.done()
	})

	events := collect(t, mustChat(t, context.Background(), client, simpleRequest(true)))

	calls := eventsOf(events, provider.EventToolCall)
	if len(calls) != 1 {
		t.Fatalf("tool call events = %d, want 1 (%v)", len(calls), types(events))
	}
	got := calls[0].ToolCall
	if got.ID != "call_abc123" {
		t.Errorf("id = %q, want %q", got.ID, "call_abc123")
	}
	if got.Name != "get_weather" {
		t.Errorf("name = %q, want %q", got.Name, "get_weather")
	}
	if got.Arguments != `{"city":"Copenhagen"}` {
		t.Errorf("arguments = %q, want %q", got.Arguments, `{"city":"Copenhagen"}`)
	}
	assertSingleDone(t, events, provider.FinishToolCalls)
}

// TestStreamOllamaCompleteToolCall uses verbatim frames from a live Ollama
// 0.31.2 server: the whole tool call arrives in one delta and finish_reason
// lands on a separate later frame.
func TestStreamOllamaCompleteToolCall(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		sse := newSSEWriter(t, w)
		sse.send(`{"id":"chatcmpl-452","object":"chat.completion.chunk","created":1788016822,"model":"llama3.1:8b","system_fingerprint":"fp_ollama","choices":[{"index":0,"delta":{"role":"assistant","content":"","tool_calls":[{"id":"call_rjzv4ky2","index":0,"type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Copenhagen\"}"}}]},"finish_reason":null}]}`)
		sse.send(`{"id":"chatcmpl-452","object":"chat.completion.chunk","created":1788016822,"model":"llama3.1:8b","system_fingerprint":"fp_ollama","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":"tool_calls"}]}`)
		sse.send(`{"id":"chatcmpl-452","object":"chat.completion.chunk","created":1788016822,"model":"llama3.1:8b","system_fingerprint":"fp_ollama","choices":[],"usage":{"prompt_tokens":161,"completion_tokens":24,"total_tokens":185}}`)
		sse.done()
	})

	events := collect(t, mustChat(t, context.Background(), client, simpleRequest(true)))

	calls := eventsOf(events, provider.EventToolCall)
	if len(calls) != 1 {
		t.Fatalf("tool call events = %d, want exactly 1 (%v)", len(calls), types(events))
	}
	if got := calls[0].ToolCall; got.ID != "call_rjzv4ky2" || got.Name != "get_weather" || got.Arguments != `{"city":"Copenhagen"}` {
		t.Errorf("tool call = %+v, want complete single-chunk call", got)
	}
	usage := eventsOf(events, provider.EventUsage)
	if len(usage) != 1 {
		t.Fatalf("usage events = %d, want 1", len(usage))
	}
	if *usage[0].Usage != (provider.Usage{PromptTokens: 161, CompletionTokens: 24, TotalTokens: 185}) {
		t.Errorf("usage = %+v", *usage[0].Usage)
	}
	assertSingleDone(t, events, provider.FinishToolCalls)
}

// TestStreamUsageOnEmptyChoicesFrame guards the frame shape that makes a naive
// choices[0] parser panic.
func TestStreamUsageOnEmptyChoicesFrame(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		sse := newSSEWriter(t, w)
		sse.send(`{"choices":[{"index":0,"delta":{"content":"hi"}}]}`)
		sse.send(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		sse.send(`{"choices":[],"usage":{"prompt_tokens":13,"completion_tokens":2,"total_tokens":15}}`)
		sse.done()
	})

	events := collect(t, mustChat(t, context.Background(), client, simpleRequest(true)))

	usage := eventsOf(events, provider.EventUsage)
	if len(usage) != 1 {
		t.Fatalf("usage events = %d, want 1 (%v)", len(usage), types(events))
	}
	if usage[0].Usage.TotalTokens != 15 {
		t.Errorf("total tokens = %d, want 15", usage[0].Usage.TotalTokens)
	}
	assertSingleDone(t, events, provider.FinishStop)
}

func TestStreamMultipleToolCallsByIndex(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		sse := newSSEWriter(t, w)
		sse.send(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"one","arguments":"{}"}}]}}]}`)
		sse.send(`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"b","function":{"name":"tw"}}]}}]}`)
		sse.send(`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"name":"o","arguments":"{\"x\":1}"}}]}}]}`)
		sse.send(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
		sse.done()
	})

	events := collect(t, mustChat(t, context.Background(), client, simpleRequest(true)))
	calls := eventsOf(events, provider.EventToolCall)
	if len(calls) != 2 {
		t.Fatalf("tool call events = %d, want 2", len(calls))
	}
	if calls[0].ToolCall.Name != "one" || calls[1].ToolCall.Name != "two" {
		t.Errorf("names = %q, %q; want one, two", calls[0].ToolCall.Name, calls[1].ToolCall.Name)
	}
	if calls[1].ToolCall.Arguments != `{"x":1}` {
		t.Errorf("second arguments = %q", calls[1].ToolCall.Arguments)
	}
}

// TestStreamToolCallsWithoutIndex covers servers that emit whole tool calls
// back to back with no index field at all.
func TestStreamToolCallsWithoutIndex(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		sse := newSSEWriter(t, w)
		sse.send(`{"choices":[{"delta":{"tool_calls":[{"id":"a","function":{"name":"one","arguments":"{}"}}]}}]}`)
		sse.send(`{"choices":[{"delta":{"tool_calls":[{"id":"b","function":{"name":"two","arguments":"{}"}}]}}]}`)
		sse.send(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
		sse.done()
	})

	calls := eventsOf(collect(t, mustChat(t, context.Background(), client, simpleRequest(true))), provider.EventToolCall)
	if len(calls) != 2 {
		t.Fatalf("tool call events = %d, want 2", len(calls))
	}
	if calls[0].ToolCall.ID != "a" || calls[1].ToolCall.ID != "b" {
		t.Errorf("ids = %q, %q; want a, b", calls[0].ToolCall.ID, calls[1].ToolCall.ID)
	}
}

func TestStreamEventOrderingIsDeltasToolsUsageDone(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		sse := newSSEWriter(t, w)
		sse.send(`{"choices":[{"delta":{"content":"a"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		sse.send(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"t","function":{"name":"n","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
		sse.done()
	})

	got := types(collect(t, mustChat(t, context.Background(), client, simpleRequest(true))))
	want := []provider.EventType{provider.EventDelta, provider.EventToolCall, provider.EventUsage, provider.EventDone}
	if len(got) != len(want) {
		t.Fatalf("event types = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event types = %v, want %v", got, want)
		}
	}
}

func TestStreamHandlesSSENoise(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		// Comments, an event: field and CRLF line endings must all be tolerated.
		_, _ = w.Write([]byte(": ping\r\n\r\n"))
		_, _ = w.Write([]byte("event: message\r\ndata: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\r\n\r\n"))
		fl.Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		fl.Flush()
	})

	events := collect(t, mustChat(t, context.Background(), client, simpleRequest(true)))
	if got := textOf(events, provider.EventDelta); got != "ok" {
		t.Errorf("text = %q, want %q", got, "ok")
	}
	assertSingleDone(t, events, provider.FinishStop)
}

// TestStreamWithoutDoneToken covers servers that just close the connection.
func TestStreamWithoutDoneToken(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		sse := newSSEWriter(t, w)
		sse.send(`{"choices":[{"delta":{"content":"partial"}}]}`)
	})

	events := collect(t, mustChat(t, context.Background(), client, simpleRequest(true)))
	if got := textOf(events, provider.EventDelta); got != "partial" {
		t.Errorf("text = %q", got)
	}
	assertSingleDone(t, events, provider.FinishStop)
}

func TestStreamMalformedFrameEmitsError(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		sse := newSSEWriter(t, w)
		sse.send(`{"choices":[{"delta":{"content":"good"}}]}`)
		sse.send(`{"choices": [ this is not json`)
		sse.done()
	})

	events := collect(t, mustChat(t, context.Background(), client, simpleRequest(true)))
	last := events[len(events)-1]
	if last.Type != provider.EventError {
		t.Fatalf("last event = %v, want error (%v)", last.Type, types(events))
	}
	if cat, ok := provider.CategoryOf(last.Err); !ok || cat != provider.ErrMalformedResponse {
		t.Fatalf("category = %v (ok=%v), want malformed_response", cat, ok)
	}
	if n := len(eventsOf(events, provider.EventDone)); n != 0 {
		t.Errorf("done events = %d, want 0 after an error", n)
	}
}

func TestStreamEmptyBodyIsMalformed(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	})

	events := collect(t, mustChat(t, context.Background(), client, simpleRequest(true)))
	if len(events) != 1 || events[0].Type != provider.EventError {
		t.Fatalf("events = %v, want a single error", types(events))
	}
	if cat, _ := provider.CategoryOf(events[0].Err); cat != provider.ErrMalformedResponse {
		t.Errorf("category = %v, want malformed_response", cat)
	}
}

func TestStreamErrorFrame(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		sse := newSSEWriter(t, w)
		sse.send(`{"error":{"message":"model crashed","type":"server_error"}}`)
	})

	events := collect(t, mustChat(t, context.Background(), client, simpleRequest(true)))
	last := events[len(events)-1]
	if last.Type != provider.EventError {
		t.Fatalf("last event = %v, want error", last.Type)
	}
	if !strings.Contains(last.Err.Error(), "model crashed") {
		t.Errorf("error = %v, want the server message", last.Err)
	}
}

// TestStreamCancellationMidStream asserts the two invariants a cancelled stream
// must keep: the channel closes and the producing goroutine exits.
func TestStreamCancellationMidStream(t *testing.T) {
	released := make(chan struct{})
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		sse := newSSEWriter(t, w)
		sse.send(`{"choices":[{"delta":{"content":"first"}}]}`)
		// Hold the response open until the client goes away.
		<-r.Context().Done()
		close(released)
	})

	baseline := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	ch := mustChat(t, ctx, client, simpleRequest(true))

	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("channel closed before the first delta")
		}
		if ev.Type != provider.EventDelta {
			t.Fatalf("first event = %v, want delta", ev.Type)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the first delta")
	}

	cancel()

	// Draining must terminate: the channel is always closed by the adapter.
	deadline := time.After(3 * time.Second)
	for open := true; open; {
		select {
		case _, ok := <-ch:
			open = ok
		case <-deadline:
			t.Fatal("event channel was not closed after cancellation")
		}
	}

	select {
	case <-released:
	case <-time.After(3 * time.Second):
		t.Fatal("server handler was never released")
	}
	waitForGoroutines(t, baseline, 4)
}

func TestStreamServerErrorStatus(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"engine unavailable"}}`))
	})

	events := collect(t, mustChat(t, context.Background(), client, simpleRequest(true)))
	if len(events) != 1 || events[0].Type != provider.EventError {
		t.Fatalf("events = %v, want a single error", types(events))
	}
	var perr *provider.Error
	if !asProviderError(events[0].Err, &perr) {
		t.Fatalf("error is not a provider error: %v", events[0].Err)
	}
	if perr.Category != provider.ErrServer || perr.Status != 500 {
		t.Errorf("category=%v status=%d, want server_error/500", perr.Category, perr.Status)
	}
	if perr.Message != "engine unavailable" {
		t.Errorf("message = %q, want the server message", perr.Message)
	}
}

// TestStreamJSONDowngrade covers a proxy that answers a stream request with a
// plain JSON completion.
func TestStreamJSONDowngrade(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "downgraded"},
				"finish_reason": "stop",
			}},
		})
	})

	events := collect(t, mustChat(t, context.Background(), client, simpleRequest(true)))
	if got := textOf(events, provider.EventDelta); got != "downgraded" {
		t.Errorf("text = %q", got)
	}
	assertSingleDone(t, events, provider.FinishStop)
}

// assertSingleDone checks the terminal-event invariant.
func assertSingleDone(t *testing.T, events []provider.ChatEvent, want provider.FinishReason) {
	t.Helper()
	done := eventsOf(events, provider.EventDone)
	if len(done) != 1 {
		t.Fatalf("done events = %d, want exactly 1 (%v)", len(done), types(events))
	}
	if done[0].Finish != want {
		t.Errorf("finish reason = %q, want %q", done[0].Finish, want)
	}
	if events[len(events)-1].Type != provider.EventDone {
		t.Errorf("last event = %v, want done", events[len(events)-1].Type)
	}
	if n := len(eventsOf(events, provider.EventError)); n != 0 {
		t.Errorf("error events = %d, want 0", n)
	}
	for _, ev := range events {
		if ev.At.IsZero() {
			t.Errorf("event %v has a zero timestamp", ev.Type)
		}
	}
}

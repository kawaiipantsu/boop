package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/boop-dev/boop/internal/provider"
)

func streamReq(text string) provider.ChatRequest {
	req := userReq(text)
	req.Stream = true
	return req
}

func TestStreamFullTypedEventSequence(t *testing.T) {
	t.Parallel()

	// A complete stream in Anthropic's typed form: split usage, a thinking
	// block, text deltas, a tool call whose arguments arrive as partial JSON
	// across several frames, and the periodic ping.
	frames := []string{
		event("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-test-model","content":[],"stop_reason":null,"usage":{"input_tokens":25,"output_tokens":1,"cache_read_input_tokens":5}}}`),
		event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
		event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"weighing options"}}`),
		event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"abc"}}`),
		event("content_block_stop", `{"type":"content_block_stop","index":0}`),
		"event: ping\ndata: {\"type\":\"ping\"}",
		event("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`),
		event("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Checking "}}`),
		event("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"the weather."}}`),
		event("content_block_stop", `{"type":"content_block_stop","index":1}`),
		event("content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_42","name":"get_weather","input":{}}}`),
		event("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"loc"}}`),
		event("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"ation\":\"Par"}}`),
		event("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"is\"}"}}`),
		event("content_block_stop", `{"type":"content_block_stop","index":2}`),
		event("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":57}}`),
		event("message_stop", `{"type":"message_stop"}`),
	}

	c, _ := newTestClient(t, sseHandler(frames...))
	events := chatOnce(t, c, streamReq("weather?"))

	if got := reasoningOf(events); got != "weighing options" {
		t.Errorf("reasoning = %q, want %q", got, "weighing options")
	}
	if got := textOf(events); got != "Checking the weather." {
		t.Errorf("text = %q, want %q", got, "Checking the weather.")
	}

	calls := toolCallsOf(events)
	if len(calls) != 1 {
		t.Fatalf("tool calls = %v, want exactly one fully assembled call", calls)
	}
	if calls[0].ID != "toolu_42" || calls[0].Name != "get_weather" {
		t.Errorf("tool call = %+v", calls[0])
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatalf("accumulated input_json_delta is not valid JSON: %v (%q)", err, calls[0].Arguments)
	}
	if args["location"] != "Paris" {
		t.Errorf("tool arguments = %v, want location Paris", args)
	}

	// Usage arrives split: input on message_start, output on message_delta.
	usage := usageOf(events)
	if usage == nil {
		t.Fatal("no usage event")
	}
	if usage.PromptTokens != 30 || usage.CachedTokens != 5 {
		t.Errorf("prompt usage = %d (cached %d), want 30 (cached 5) from message_start",
			usage.PromptTokens, usage.CachedTokens)
	}
	if usage.CompletionTokens != 57 {
		t.Errorf("completion usage = %d, want 57 from message_delta", usage.CompletionTokens)
	}
	if usage.TotalTokens != 87 {
		t.Errorf("total usage = %d, want 87", usage.TotalTokens)
	}

	last := events[len(events)-1]
	if last.Type != provider.EventDone || last.Finish != provider.FinishToolCalls {
		t.Errorf("terminal event = %q/%q, want done/tool_calls", last.Type, last.Finish)
	}

	// Ordering: the tool call must be emitted once its block stops, after the
	// text that preceded it.
	order := types(events)
	toolIdx, doneIdx := -1, -1
	for i, ty := range order {
		switch ty {
		case provider.EventToolCall:
			toolIdx = i
		case provider.EventDone:
			doneIdx = i
		}
	}
	if toolIdx < 0 || doneIdx < 0 || toolIdx > doneIdx {
		t.Errorf("event order = %v", order)
	}
}

func TestStreamSetsStreamFlagAndAcceptHeader(t *testing.T) {
	t.Parallel()

	c, cap := newTestClient(t, sseHandler(
		event("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`),
		event("message_stop", `{"type":"message_stop"}`),
	))
	chatOnce(t, c, streamReq("hi"))

	if body := cap.request(t); body["stream"] != true {
		t.Errorf("stream = %v, want true", body["stream"])
	}
	if got := cap.header("Accept"); got != "text/event-stream" {
		t.Errorf("Accept = %q, want text/event-stream", got)
	}
}

func TestStreamErrorEventTerminatesTheStream(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		errType      string
		wantCategory provider.ErrorCategory
		wantRetry    bool
	}{
		{name: "overloaded", errType: "overloaded_error", wantCategory: provider.ErrServer, wantRetry: true},
		{name: "api error", errType: "api_error", wantCategory: provider.ErrServer, wantRetry: true},
		{name: "rate limited", errType: "rate_limit_error", wantCategory: provider.ErrRateLimited, wantRetry: true},
		{name: "invalid request", errType: "invalid_request_error", wantCategory: provider.ErrInvalidRequest},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			frames := []string{
				event("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":9}}}`),
				event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
				event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`),
				event("error", `{"type":"error","error":{"type":"`+tc.errType+`","message":"the server gave up"}}`),
				// Anything after the error must be ignored.
				event("message_stop", `{"type":"message_stop"}`),
			}
			c, _ := newTestClient(t, sseHandler(frames...))
			events := chatOnce(t, c, streamReq("hi"))

			if got := textOf(events); got != "partial" {
				t.Errorf("text = %q, want the deltas received before the error", got)
			}
			err := terminalError(t, events)
			if cat, ok := provider.CategoryOf(err); !ok || cat != tc.wantCategory {
				t.Errorf("category = %q, want %q", cat, tc.wantCategory)
			}
			if provider.IsRetryable(err) != tc.wantRetry {
				t.Errorf("IsRetryable = %v, want %v", provider.IsRetryable(err), tc.wantRetry)
			}
			if !strings.Contains(err.Error(), "the server gave up") {
				t.Errorf("error = %q, want the server message preserved", err)
			}
			if events[len(events)-1].Finish != provider.FinishError {
				t.Errorf("finish = %q, want %q", events[len(events)-1].Finish, provider.FinishError)
			}
		})
	}
}

func TestStreamMissingMessageStopStillCompletes(t *testing.T) {
	t.Parallel()

	// A server that closes after the last content frame without message_stop:
	// the turn produced real content and must not be discarded.
	c, _ := newTestClient(t, sseHandler(
		event("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":4}}}`),
		event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_7","name":"lookup","input":{}}}`),
		event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"go\"}"}}`),
		event("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":11}}`),
	))
	events := chatOnce(t, c, streamReq("hi"))

	calls := toolCallsOf(events)
	if len(calls) != 1 || calls[0].Arguments != `{"q":"go"}` {
		t.Fatalf("tool calls = %v, want the unstopped block flushed at end of stream", calls)
	}
	if last := events[len(events)-1]; last.Type != provider.EventDone || last.Finish != provider.FinishToolCalls {
		t.Errorf("terminal event = %q/%q", last.Type, last.Finish)
	}
}

func TestStreamEmitsEachToolCallOnce(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t, sseHandler(
		event("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":4}}}`),
		event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_a","name":"one","input":{}}}`),
		event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`),
		event("content_block_stop", `{"type":"content_block_stop","index":0}`),
		event("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_b","name":"two","input":{}}}`),
		event("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"x\":1}"}}`),
		event("content_block_stop", `{"type":"content_block_stop","index":1}`),
		event("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":3}}`),
		event("message_stop", `{"type":"message_stop"}`),
	))
	events := chatOnce(t, c, streamReq("hi"))

	calls := toolCallsOf(events)
	if len(calls) != 2 {
		t.Fatalf("tool calls = %v, want exactly two", calls)
	}
	if calls[0].ID != "toolu_a" || calls[0].Name != "one" || calls[0].Arguments != "{}" {
		t.Errorf("calls[0] = %+v", calls[0])
	}
	if calls[1].ID != "toolu_b" || calls[1].Name != "two" || calls[1].Arguments != `{"x":1}` {
		t.Errorf("calls[1] = %+v", calls[1])
	}
}

func TestStreamToolCallWithNoArgumentFragments(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t, sseHandler(
		event("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":4}}}`),
		event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_z","name":"no_args","input":{}}}`),
		event("content_block_stop", `{"type":"content_block_stop","index":0}`),
		event("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":2}}`),
		event("message_stop", `{"type":"message_stop"}`),
	))
	events := chatOnce(t, c, streamReq("hi"))

	calls := toolCallsOf(events)
	if len(calls) != 1 || calls[0].Arguments != "{}" {
		t.Fatalf("tool calls = %v, want one call with empty-object arguments", calls)
	}
}

func TestStreamHTTPFailure(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t, jsonHandler(http.StatusTooManyRequests,
		`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))

	events := chatOnce(t, c, streamReq("hi"))
	err := terminalError(t, events)
	if cat, ok := provider.CategoryOf(err); !ok || cat != provider.ErrRateLimited {
		t.Errorf("category = %q, want %q", cat, provider.ErrRateLimited)
	}
	if !provider.IsRetryable(err) {
		t.Error("a rate limit must be retryable")
	}
}

func TestStreamEmptyBodyIsMalformed(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	})

	events := chatOnce(t, c, streamReq("hi"))
	err := terminalError(t, events)
	if cat, ok := provider.CategoryOf(err); !ok || cat != provider.ErrMalformedResponse {
		t.Errorf("category = %q, want %q", cat, provider.ErrMalformedResponse)
	}
}

func TestStreamMalformedFrame(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t, sseHandler(
		event("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1}}}`),
		event("content_block_delta", `{"type":"content_block_delta",`),
	))
	events := chatOnce(t, c, streamReq("hi"))
	err := terminalError(t, events)
	if cat, ok := provider.CategoryOf(err); !ok || cat != provider.ErrMalformedResponse {
		t.Errorf("category = %q, want %q", cat, provider.ErrMalformedResponse)
	}
}

func TestStreamCancellationIsNotAFault(t *testing.T) {
	t.Parallel()

	released := make(chan struct{})
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-released
		<-r.Context().Done()
	})
	t.Cleanup(func() { close(released) })

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := c.Chat(ctx, streamReq("hi"))
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	cancel()

	// The adapter must close the channel rather than block on an abandoned
	// caller; a cancelled stream is not reported as a provider fault.
	for ev := range ch {
		if ev.Type == provider.EventError {
			if cat, _ := provider.CategoryOf(ev.Err); cat != provider.ErrCancelled {
				t.Errorf("category = %q, want %q", cat, provider.ErrCancelled)
			}
			if ev.Finish != provider.FinishCancelled {
				t.Errorf("finish = %q, want %q", ev.Finish, provider.FinishCancelled)
			}
		}
	}
}

func TestSSEScanner(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{
			name:  "event and data lines",
			input: "event: ping\ndata: {\"type\":\"ping\"}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			want:  []string{`{"type":"ping"}`, `{"type":"message_stop"}`},
		},
		{
			name:  "comment lines are keep-alives",
			input: ": keep-alive\n\ndata: {\"a\":1}\n\n",
			want:  []string{`{"a":1}`},
		},
		{
			name:  "carriage returns are stripped",
			input: "data: {\"a\":1}\r\n\r\n",
			want:  []string{`{"a":1}`},
		},
		{
			name:  "multi-line data is joined with newlines",
			input: "data: line1\ndata: line2\n\n",
			want:  []string{"line1\nline2"},
		},
		{
			name:  "a trailing frame with no blank line is still delivered",
			input: "data: {\"a\":1}",
			want:  []string{`{"a":1}`},
		},
		{
			name:  "fields other than data are ignored",
			input: "id: 7\nretry: 100\ndata: {\"a\":1}\n\n",
			want:  []string{`{"a":1}`},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newSSEScanner(strings.NewReader(tc.input))
			var got []string
			for {
				payload, err := s.next()
				if err != nil {
					break
				}
				if payload != "" {
					got = append(got, payload)
				}
			}
			if len(got) != len(tc.want) {
				t.Fatalf("frames = %q, want %q", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("frame %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

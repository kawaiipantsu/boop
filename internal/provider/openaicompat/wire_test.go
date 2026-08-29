package openaicompat

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/boop-dev/boop/internal/provider"
)

func TestFlexContentUnmarshal(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"string", `"hello"`, "hello"},
		{"null", `null`, ""},
		{"empty string", `""`, ""},
		{"part array", `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`, "ab"},
		{"empty array", `[]`, ""},
		{"unknown shape is ignored", `{"unexpected":true}`, ""},
		{"number is ignored", `42`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got flexContent
			if err := json.Unmarshal([]byte(tc.raw), &got); err != nil {
				t.Fatalf("Unmarshal(%s): %v", tc.raw, err)
			}
			if got.Text != tc.want {
				t.Errorf("text = %q, want %q", got.Text, tc.want)
			}
			round, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if want, _ := json.Marshal(tc.want); string(round) != string(want) {
				t.Errorf("round trip = %s, want %s", round, want)
			}
		})
	}
}

// TestArrayContentDelta covers servers that stream content as typed parts
// rather than a bare string.
func TestArrayContentDelta(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		sse := newSSEWriter(t, w)
		sse.send(`{"choices":[{"delta":{"content":[{"type":"text","text":"arr"}]}}]}`)
		sse.send(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
		sse.done()
	})
	events := collect(t, mustChat(t, context.Background(), client, simpleRequest(true)))
	if got := textOf(events, provider.EventDelta); got != "arr" {
		t.Errorf("text = %q, want %q", got, "arr")
	}
}

func TestToolCallWithoutIDGetsSyntheticID(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		sse := newSSEWriter(t, w)
		sse.send(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"noid","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
		sse.done()
	})

	calls := eventsOf(collect(t, mustChat(t, context.Background(), client, simpleRequest(true))), provider.EventToolCall)
	if len(calls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(calls))
	}
	// The tool runtime keys results by id, so an id is synthesised rather than
	// passed through empty.
	if !strings.HasPrefix(calls[0].ToolCall.ID, "call_") {
		t.Errorf("id = %q, want a synthesised call_ id", calls[0].ToolCall.ID)
	}
}

func TestToolCallWithoutNameIsDropped(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		sse := newSSEWriter(t, w)
		sse.send(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"x","function":{"arguments":"{}"}}]},"finish_reason":"stop"}]}`)
		sse.done()
	})

	events := collect(t, mustChat(t, context.Background(), client, simpleRequest(true)))
	if n := len(eventsOf(events, provider.EventToolCall)); n != 0 {
		t.Errorf("tool call events = %d, want 0 for a nameless fragment set", n)
	}
	assertSingleDone(t, events, provider.FinishStop)
}

func TestUsageAliasFields(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want provider.Usage
	}{
		{
			name: "standard",
			raw:  `{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}`,
			want: provider.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		},
		{
			name: "responses spelling with derived total",
			raw:  `{"input_tokens":10,"output_tokens":5}`,
			want: provider.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		},
		{
			name: "ollama native counters",
			raw:  `{"prompt_eval_count":7,"eval_count":3}`,
			want: provider.Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10},
		},
		{
			name: "cached prompt tokens",
			raw:  `{"prompt_tokens":10,"completion_tokens":0,"total_tokens":10,"prompt_tokens_details":{"cached_tokens":9}}`,
			want: provider.Usage{PromptTokens: 10, TotalTokens: 10, CachedTokens: 9},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var u wireUsage
			if err := json.Unmarshal([]byte(tc.raw), &u); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			got := convertUsage(&u)
			if got == nil {
				t.Fatal("convertUsage returned nil for reported usage")
			}
			if *got != tc.want {
				t.Errorf("usage = %+v, want %+v", *got, tc.want)
			}
		})
	}

	if convertUsage(nil) != nil {
		t.Error("convertUsage(nil) must be nil so no empty usage event is emitted")
	}
	var zero wireUsage
	if convertUsage(&zero) != nil {
		t.Error("convertUsage of an all-zero usage must be nil")
	}
}

func TestSSEScannerFraming(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"single frame", "data: a\n\n", []string{"a"}},
		{"two frames", "data: a\n\ndata: b\n\n", []string{"a", "b"}},
		{"multiline data joined", "data: a\ndata: b\n\n", []string{"a\nb"}},
		{"comments ignored", ": keep-alive\n\ndata: a\n\n", []string{"a"}},
		{"other fields ignored", "event: msg\nid: 1\ndata: a\n\n", []string{"a"}},
		{"crlf", "data: a\r\n\r\n", []string{"a"}},
		{"no trailing blank line", "data: a", []string{"a"}},
		{"no space after colon", "data:a\n\n", []string{"a"}},
		{"bare field name", "data\n\ndata: a\n\n", []string{"a"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newSSEScanner(strings.NewReader(tc.in))
			var got []string
			for {
				payload, err := s.next()
				if err != nil {
					break
				}
				got = append(got, payload)
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

package ollama

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/kawaiipantsu/boop/internal/provider"
)

// collect drains an event channel, failing if it does not close promptly.
func collect(t *testing.T, ch <-chan provider.ChatEvent) []provider.ChatEvent {
	t.Helper()
	var out []provider.ChatEvent
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

// sse writes flushed Server-Sent Events frames, as Ollama's /v1 surface does.
func sse(t *testing.T, w http.ResponseWriter, frames ...string) {
	t.Helper()
	flusher, ok := w.(http.Flusher)
	if !ok {
		t.Fatal("test writer cannot flush")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	for _, f := range frames {
		if _, err := io.WriteString(w, "data: "+f+"\n\n"); err != nil {
			return
		}
		flusher.Flush()
	}
}

// TestChatStreamQuirks pins the two shapes a real Ollama server produces that
// most OpenAI-compatible servers do not: a terminal usage frame whose choices
// array is empty, and a tool call delivered complete in one delta instead of
// fragmented across frames.
func TestChatStreamQuirks(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	f.handle("/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		sse(t, w,
			`{"id":"c1","object":"chat.completion.chunk","model":"llama3.1:8b","choices":[{"index":0,"delta":{"role":"assistant","content":"Oslo"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","model":"llama3.1:8b","choices":[{"index":0,"delta":{"content":" is cold"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","model":"llama3.1:8b","choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_1","index":0,"type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Oslo\"}"}}]},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","model":"llama3.1:8b","choices":[{"index":0,"delta":{"content":""},"finish_reason":"tool_calls"}]}`,
			`{"id":"c1","object":"chat.completion.chunk","model":"llama3.1:8b","choices":[],"usage":{"prompt_tokens":31,"completion_tokens":17,"total_tokens":48}}`,
			"[DONE]",
		)
	})
	_, c := f.start()

	ch, err := c.Chat(context.Background(), provider.ChatRequest{
		Model:    "llama3.1:8b",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "weather?"}},
		Stream:   true,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	events := collect(t, ch)

	var text string
	var calls []provider.ToolCall
	var usage *provider.Usage
	var finish provider.FinishReason
	var done bool
	for _, ev := range events {
		switch ev.Type {
		case provider.EventDelta:
			text += ev.Text
		case provider.EventToolCall:
			calls = append(calls, *ev.ToolCall)
		case provider.EventUsage:
			usage = ev.Usage
		case provider.EventDone:
			done = true
			finish = ev.Finish
		case provider.EventError:
			t.Fatalf("unexpected error event: %v", ev.Err)
		}
	}

	if !done {
		t.Fatal("stream did not terminate with EventDone")
	}
	if text != "Oslo is cold" {
		t.Fatalf("text = %q", text)
	}
	if len(calls) != 1 {
		t.Fatalf("got %d tool calls, want exactly 1 assembled from the single delta", len(calls))
	}
	if calls[0].Name != "get_weather" || calls[0].Arguments != `{"city":"Oslo"}` || calls[0].ID != "call_1" {
		t.Fatalf("tool call = %+v", calls[0])
	}
	if usage == nil || usage.TotalTokens != 48 {
		t.Fatalf("usage from the empty-choices frame = %+v", usage)
	}
	if finish != provider.FinishToolCalls {
		t.Fatalf("finish = %q, want %q", finish, provider.FinishToolCalls)
	}
}

// TestChatToolsOnUnsupportedModel reproduces the exact 400 Ollama returns when
// tools are requested from a completion-only model. §8 requires this to reach
// the user as a missing capability so the UI can offer a model that has it,
// not as a generic bad request.
func TestChatToolsOnUnsupportedModel(t *testing.T) {
	t.Parallel()

	const body = `{"error":{"message":"registry.ollama.ai/library/qwen:7b does not support tools","type":"invalid_request_error","param":null,"code":null}}`

	for _, stream := range []bool{false, true} {
		name := "non-streaming"
		if stream {
			name = "streaming"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			f.status("/v1/chat/completions", http.StatusBadRequest, body)
			_, c := f.start()

			ch, err := c.Chat(context.Background(), provider.ChatRequest{
				Model:    "qwen:7b",
				Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
				Stream:   stream,
				Tools: []provider.ToolDefinition{{
					Name:        "run",
					Description: "run a command",
					Schema:      map[string]any{"type": "object", "properties": map[string]any{}},
				}},
			})
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}

			events := collect(t, ch)
			if len(events) != 1 || events[0].Type != provider.EventError {
				t.Fatalf("want a single terminal error event, got %+v", events)
			}
			pe := providerError(t, events[0].Err)
			if pe.Category != provider.ErrUnsupportedCapability {
				t.Fatalf("category = %q, want %q", pe.Category, provider.ErrUnsupportedCapability)
			}
			if pe.Message != "registry.ollama.ai/library/qwen:7b does not support tools" {
				t.Fatalf("message = %q", pe.Message)
			}
		})
	}
}

// TestEmbedUsesOpenAIPath records the deliberate choice to embed through the
// OpenAI-compatible /v1/embeddings endpoint rather than Ollama's native
// /api/embed: both return identical vectors, and the shared path needs no
// vendor code at all.
func TestEmbedUsesOpenAIPath(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	f.static("/v1/embeddings", `{"object":"list","data":[
	  {"object":"embedding","index":1,"embedding":[0.3,0.4]},
	  {"object":"embedding","index":0,"embedding":[0.1,0.2]}],
	  "model":"nomic-embed-text","usage":{"prompt_tokens":4,"total_tokens":4}}`)
	_, c := f.start()

	resp, err := c.Embed(context.Background(), provider.EmbeddingRequest{
		Model: "nomic-embed-text",
		Input: []string{"a", "b"},
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(resp.Vectors) != 2 {
		t.Fatalf("got %d vectors, want 2", len(resp.Vectors))
	}
	// Vectors must align with the inputs, not with the server's ordering.
	if resp.Vectors[0][0] != 0.1 || resp.Vectors[1][0] != 0.3 {
		t.Fatalf("vectors out of input order: %v", resp.Vectors)
	}
	if got := f.count("/api/embed"); got != 0 {
		t.Fatalf("native /api/embed called %d times; the OpenAI path is the chosen one", got)
	}
}

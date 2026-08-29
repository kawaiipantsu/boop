package lemonade

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/kawaiipantsu/boop/internal/provider"
)

// TestChatUsesAPIPrefixedPath pins the one thing about Lemonade's OpenAI
// surface that this package is responsible for: composing the base URL so that
// chat, models and embeddings land under <root>/api/v1 rather than <root>/v1.
// Everything after that is the shared implementation and is tested there.
func TestChatUsesAPIPrefixedPath(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	f.handle(api("/chat/completions"), func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("test writer cannot flush")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		for _, frame := range []string{
			`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"boop"},"finish_reason":null}]}`,
			`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`,
			"[DONE]",
		} {
			_, _ = io.WriteString(w, "data: "+frame+"\n\n")
			flusher.Flush()
		}
	})
	_, c := f.start()

	ch, err := c.Chat(context.Background(), provider.ChatRequest{
		Model:    "Llama-3.2-1B-Instruct-Hybrid",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
		Stream:   true,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	var text string
	var sawDone bool
	for ev := range ch {
		switch ev.Type {
		case provider.EventDelta:
			text += ev.Text
		case provider.EventDone:
			sawDone = true
		case provider.EventError:
			t.Fatalf("stream error: %v", ev.Err)
		}
	}
	if !sawDone {
		t.Fatal("stream did not end with EventDone")
	}
	if text != "boop" {
		t.Fatalf("text = %q", text)
	}
	if got := f.count(api("/chat/completions")); got != 1 {
		t.Fatalf("chat endpoint hit %d times, want 1", got)
	}
}

func TestEmbedUsesAPIPrefixedPath(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	f.static(api("/embeddings"), `{"object":"list","data":[
	  {"object":"embedding","index":0,"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":2,"total_tokens":2}}`)
	_, c := f.start()

	resp, err := c.Embed(context.Background(), provider.EmbeddingRequest{
		Model: "nomic-embed-text-v1-GGUF",
		Input: []string{"boop"},
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(resp.Vectors) != 1 || len(resp.Vectors[0]) != 2 {
		t.Fatalf("vectors = %v", resp.Vectors)
	}
}

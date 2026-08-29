package ollama

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/boop-dev/boop/internal/provider"
)

// liveEnv names the environment variable that opts into tests against a real
// Ollama server.
//
// PROJECT.md §41 forbids the default suite from depending on external
// services, so everything below skips unless the variable is set. `make test`
// must never reach the network because of this file.
const liveEnv = "BOOP_TEST_OLLAMA_URL"

// liveClient returns a client for the configured server, skipping when the
// opt-in variable is unset.
func liveClient(t *testing.T) *Client {
	t.Helper()
	url := strings.TrimSpace(os.Getenv(liveEnv))
	if url == "" {
		t.Skipf("set %s to run tests against a real Ollama server", liveEnv)
	}
	return New(url, nil, WithTimeout(60*time.Second))
}

// liveCtx bounds a live call so a wedged server fails the test instead of the
// whole suite.
func liveCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestLiveHealthAndVersion(t *testing.T) {
	c := liveClient(t)

	if err := c.Health(liveCtx(t)); err != nil {
		t.Fatalf("Health: %v", err)
	}
	version, err := c.Version(liveCtx(t))
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if version == "" {
		t.Fatal("Version returned an empty string")
	}
	t.Logf("ollama version %s", version)
}

func TestLiveListModels(t *testing.T) {
	c := liveClient(t)

	models, err := c.ListModels(liveCtx(t))
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("no models installed on the live server")
	}
	for _, m := range models {
		t.Logf("%-28s ctx=%-7d caps=%v", m.ID, m.ContextWindow, m.Capabilities.Strings())
		if m.Provider != c.Name() {
			t.Fatalf("model %s attributed to %q", m.ID, m.Provider)
		}
		if len(m.Capabilities) == 0 {
			t.Fatalf("model %s reported no capabilities", m.ID)
		}
	}
}

func TestLiveCapabilitiesMatchDeclaration(t *testing.T) {
	c := liveClient(t)

	tests := []struct {
		model    string
		wantHas  []provider.Capability
		wantMiss []provider.Capability
	}{
		{model: "llama3.1:8b", wantHas: []provider.Capability{provider.CapabilityTools, provider.CapabilityStreaming}},
		{model: "qwen2.5:7b", wantHas: []provider.Capability{provider.CapabilityTools}},
		{model: "qwen:7b", wantHas: []provider.Capability{provider.CapabilityStreaming}, wantMiss: []provider.Capability{provider.CapabilityTools}},
		{model: "nomic-embed-text", wantHas: []provider.Capability{provider.CapabilityEmbeddings}, wantMiss: []provider.Capability{provider.CapabilityStreaming}},
	}
	for _, tc := range tests {
		t.Run(tc.model, func(t *testing.T) {
			caps, err := c.Capabilities(liveCtx(t), tc.model)
			if err != nil {
				t.Fatalf("Capabilities: %v", err)
			}
			if !caps.HasAll(tc.wantHas...) {
				t.Fatalf("%s = %v, missing %v", tc.model, caps, caps.Missing(tc.wantHas...))
			}
			for _, unwanted := range tc.wantMiss {
				if caps.Has(unwanted) {
					t.Fatalf("%s = %v, must not contain %q", tc.model, caps, unwanted)
				}
			}
		})
	}
}

func TestLiveChatStream(t *testing.T) {
	c := liveClient(t)

	ch, err := c.Chat(liveCtx(t), provider.ChatRequest{
		Model:     "llama3.2:latest",
		Messages:  []provider.Message{{Role: provider.RoleUser, Content: "Reply with the single word: boop"}},
		MaxTokens: 16,
		Stream:    true,
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
		case provider.EventUsage:
			t.Logf("usage: %+v", ev.Usage)
		case provider.EventDone:
			sawDone = true
		case provider.EventError:
			t.Fatalf("stream error: %v", ev.Err)
		}
	}
	if !sawDone {
		t.Fatal("stream did not end with EventDone")
	}
	if strings.TrimSpace(text) == "" {
		t.Fatal("stream produced no text")
	}
}

// TestLiveToolsOnUnsupportedModel is the reason qwen:7b is worth keeping
// installed: it proves the real 400 becomes ErrUnsupportedCapability.
func TestLiveToolsOnUnsupportedModel(t *testing.T) {
	c := liveClient(t)

	ch, err := c.Chat(liveCtx(t), provider.ChatRequest{
		Model:    "qwen:7b",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
		Stream:   true,
		Tools: []provider.ToolDefinition{{
			Name:        "get_weather",
			Description: "look up the weather",
			Schema:      map[string]any{"type": "object", "properties": map[string]any{}},
		}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	var last provider.ChatEvent
	for ev := range ch {
		last = ev
	}
	if last.Type != provider.EventError {
		t.Fatalf("last event = %q, want an error", last.Type)
	}
	cat, ok := provider.CategoryOf(last.Err)
	if !ok || cat != provider.ErrUnsupportedCapability {
		t.Fatalf("category = %q (provider error: %v), want %q", cat, ok, provider.ErrUnsupportedCapability)
	}
}

func TestLiveEmbed(t *testing.T) {
	c := liveClient(t)

	resp, err := c.Embed(liveCtx(t), provider.EmbeddingRequest{
		Model: "nomic-embed-text",
		Input: []string{"boop", "beep"},
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(resp.Vectors) != 2 {
		t.Fatalf("got %d vectors, want 2", len(resp.Vectors))
	}
	if len(resp.Vectors[0]) == 0 {
		t.Fatal("first vector is empty")
	}
}

func TestLiveModelLifecycle(t *testing.T) {
	c := liveClient(t)
	const model = "llama3.2:latest"

	if err := c.LoadModel(liveCtx(t), model); err != nil {
		t.Fatalf("LoadModel: %v", err)
	}
	if err := c.UnloadModel(liveCtx(t), model); err != nil {
		t.Fatalf("UnloadModel: %v", err)
	}
}

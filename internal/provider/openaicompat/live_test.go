package openaicompat_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/boop-dev/boop/internal/provider"
	"github.com/boop-dev/boop/internal/provider/openaicompat"
)

// Live tests run against a real OpenAI-compatible server and are opt-in.
//
// PROJECT.md §41 forbids the default suite depending on external services, so
// these skip unless BOOP_TEST_OPENAI_COMPAT_URL is set, for example:
//
//	BOOP_TEST_OPENAI_COMPAT_URL=http://127.0.0.1:11434/v1 \
//	BOOP_TEST_MODEL=llama3.1:8b go test ./internal/provider/openaicompat/ -run Live -v
func liveClient(t *testing.T) (*openaicompat.Client, string) {
	t.Helper()
	baseURL := os.Getenv("BOOP_TEST_OPENAI_COMPAT_URL")
	if baseURL == "" {
		t.Skip("set BOOP_TEST_OPENAI_COMPAT_URL to run live provider tests")
	}
	model := os.Getenv("BOOP_TEST_MODEL")
	if model == "" {
		model = "llama3.1:8b"
	}
	return openaicompat.New(openaicompat.Options{
		Name:    "live",
		BaseURL: baseURL,
		Timeout: 120 * time.Second,
	}), model
}

func TestLiveHealthAndListModels(t *testing.T) {
	c, _ := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := c.Health(ctx); err != nil {
		t.Fatalf("Health() = %v, want nil", err)
	}
	models, err := c.ListModels(ctx)
	if err != nil {
		t.Fatalf("ListModels() = %v, want nil", err)
	}
	if len(models) == 0 {
		t.Fatal("ListModels() returned no models")
	}
	t.Logf("server offers %d models, first = %s", len(models), models[0].ID)
}

func TestLiveStreamingCompletion(t *testing.T) {
	c, model := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	events, err := c.Chat(ctx, provider.ChatRequest{
		Model:     model,
		Stream:    true,
		MaxTokens: 40,
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: "Reply with exactly the word: ok"},
		},
	})
	if err != nil {
		t.Fatalf("Chat() = %v, want nil", err)
	}

	var text strings.Builder
	var usage *provider.Usage
	var dones int
	for ev := range events {
		switch ev.Type {
		case provider.EventDelta:
			text.WriteString(ev.Text)
		case provider.EventUsage:
			usage = ev.Usage
		case provider.EventDone:
			dones++
		case provider.EventError:
			t.Fatalf("stream error: %v", ev.Err)
		}
	}
	if dones != 1 {
		t.Errorf("got %d EventDone, want exactly 1", dones)
	}
	if strings.TrimSpace(text.String()) == "" {
		t.Error("stream produced no text")
	}
	// The live server reports usage on a frame carrying an empty choices
	// array; a regression there would silently drop accounting.
	if usage == nil {
		t.Error("no usage reported; the empty-choices usage frame may be being dropped")
	} else if usage.TotalTokens == 0 {
		t.Error("usage reported zero total tokens")
	}
	t.Logf("text=%q usage=%+v", strings.TrimSpace(text.String()), usage)
}

func TestLiveToolCall(t *testing.T) {
	c, model := liveClient(t)

	// A small model sometimes answers in prose instead of calling the tool.
	// That is model nondeterminism, not an adapter fault, so retry a few times
	// and only fail if the tool is never invoked. What is being tested here is
	// our reassembly of the call, not the model's obedience.
	const attempts = 3
	var calls []provider.ToolCall
	for attempt := 1; attempt <= attempts && len(calls) == 0; attempt++ {
		var err error
		calls, err = liveToolCallAttempt(t, c, model)
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		if len(calls) == 0 {
			t.Logf("attempt %d: model replied without calling the tool, retrying", attempt)
		}
	}
	if len(calls) == 0 {
		t.Skipf("%s never called the tool in %d attempts; nothing to verify about reassembly", model, attempts)
	}
	if len(calls) != 1 {
		t.Fatalf("got %d tool calls, want exactly 1 (reassembly is splitting or duplicating)", len(calls))
	}
	if calls[0].Name != "get_weather" {
		t.Errorf("tool name = %q, want get_weather", calls[0].Name)
	}
	if !strings.Contains(strings.ToLower(calls[0].Arguments), "copenhagen") {
		t.Errorf("arguments = %q, want them to mention Copenhagen", calls[0].Arguments)
	}
	if calls[0].ID == "" {
		t.Error("tool call has no id; the tool runtime keys results by id")
	}
	t.Logf("tool call: id=%s name=%s args=%s", calls[0].ID, calls[0].Name, calls[0].Arguments)
}

// liveToolCallAttempt runs one streamed tool-calling exchange and returns every
// tool call the adapter assembled from it.
func liveToolCallAttempt(t *testing.T, c *openaicompat.Client, model string) ([]provider.ToolCall, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	events, err := c.Chat(ctx, provider.ChatRequest{
		Model:  model,
		Stream: true,
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: "What is the weather in Copenhagen? Use the tool."},
		},
		Tools: []provider.ToolDefinition{{
			Name:        "get_weather",
			Description: "Get the current weather for a city",
			Schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"city": map[string]any{"type": "string"}},
				"required":   []string{"city"},
			},
		}},
	})
	if err != nil {
		return nil, err
	}

	var calls []provider.ToolCall
	var dones int
	for ev := range events {
		switch ev.Type {
		case provider.EventToolCall:
			calls = append(calls, *ev.ToolCall)
		case provider.EventDone:
			dones++
		case provider.EventError:
			return nil, ev.Err
		}
	}
	if dones != 1 {
		t.Errorf("got %d EventDone, want exactly 1", dones)
	}
	return calls, nil
}

// TestLiveUnsupportedCapability asserts that asking a completion-only model for
// tools surfaces as ErrUnsupportedCapability rather than a bare bad request, so
// the user is told to switch models (§8).
func TestLiveUnsupportedCapability(t *testing.T) {
	c, _ := liveClient(t)
	model := os.Getenv("BOOP_TEST_NOTOOLS_MODEL")
	if model == "" {
		t.Skip("set BOOP_TEST_NOTOOLS_MODEL to a completion-only model")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	events, err := c.Chat(ctx, provider.ChatRequest{
		Model:    model,
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
		Tools: []provider.ToolDefinition{{
			Name:   "noop",
			Schema: map[string]any{"type": "object"},
		}},
	})
	var got error
	if err != nil {
		got = err
	} else {
		for ev := range events {
			if ev.Type == provider.EventError {
				got = ev.Err
			}
		}
	}
	if got == nil {
		t.Fatal("expected an error asking a completion-only model for tools")
	}
	category, ok := provider.CategoryOf(got)
	if !ok || category != provider.ErrUnsupportedCapability {
		t.Errorf("category = %v (ok=%v), want %v", category, ok, provider.ErrUnsupportedCapability)
	}
	t.Logf("correctly classified: %v", got)
}

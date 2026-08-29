package lmstudio

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kawaiipantsu/boop/internal/provider"
)

// liveEnv names the environment variable that opts into tests against a real
// LM Studio server.
//
// PROJECT.md §41 forbids the default suite from depending on external
// services, so every test below skips unless the variable is set. `make test`
// must never reach the network because of this file.
//
// These tests are also the way to confirm the /api/v0 shapes this package
// infers: run them against a real server and any mismatch will surface here.
const liveEnv = "BOOP_TEST_LMSTUDIO_URL"

func liveClient(t *testing.T) *Client {
	t.Helper()
	url := strings.TrimSpace(os.Getenv(liveEnv))
	if url == "" {
		t.Skipf("set %s to run tests against a real LM Studio server", liveEnv)
	}
	return New(url, nil, WithTimeout(60*time.Second))
}

func liveCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestLiveHealth(t *testing.T) {
	c := liveClient(t)
	if err := c.Health(liveCtx(t)); err != nil {
		t.Fatalf("Health: %v", err)
	}
}

func TestLiveListModels(t *testing.T) {
	c := liveClient(t)

	models, err := c.ListModels(liveCtx(t))
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("no models available on the live server")
	}
	for _, m := range models {
		t.Logf("%-48s ctx=%-7d caps=%v", m.ID, m.ContextWindow, m.Capabilities.Strings())
		if m.Provider != c.Name() {
			t.Fatalf("model %s attributed to %q", m.ID, m.Provider)
		}
	}
}

// TestLiveNativeModels reports whether the running version serves /api/v0 and
// what it returns, which is the fastest way to correct the inferred shapes in
// this package.
func TestLiveNativeModels(t *testing.T) {
	c := liveClient(t)

	infos, err := c.NativeModels(liveCtx(t))
	if err != nil {
		var pe *provider.Error
		if errors.As(err, &pe) && pe.Category == provider.ErrUnsupportedCapability {
			t.Skipf("this LM Studio build has no %s: %v", restModelsPath, err)
		}
		t.Fatalf("NativeModels: %v", err)
	}
	for _, info := range infos {
		t.Logf("%-48s type=%-10s state=%-10s max_ctx=%d", info.ID, info.Type, info.State, info.MaxContextLength)
	}
}

func TestLiveChatStream(t *testing.T) {
	c := liveClient(t)

	models, err := c.ListModels(liveCtx(t))
	if err != nil || len(models) == 0 {
		t.Fatalf("ListModels: %v", err)
	}
	model := ""
	for _, m := range models {
		if m.Capabilities.Has(provider.CapabilityStreaming) {
			model = m.ID
			break
		}
	}
	if model == "" {
		t.Skip("no chat-capable model available on the live server")
	}

	ch, err := c.Chat(liveCtx(t), provider.ChatRequest{
		Model:     model,
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

func TestLiveModelLifecycle(t *testing.T) {
	c := liveClient(t)

	infos, err := c.NativeModels(liveCtx(t))
	if err != nil {
		t.Skipf("native model state unavailable: %v", err)
	}
	model := ""
	for _, info := range infos {
		if info.Type == typeLLM {
			model = info.ID
			break
		}
	}
	if model == "" {
		t.Skip("no llm-typed model available on the live server")
	}

	if err := c.LoadModel(liveCtx(t), model); err != nil {
		t.Fatalf("LoadModel(%s): %v", model, err)
	}
	loaded, err := c.IsLoaded(liveCtx(t), model)
	if err != nil {
		t.Fatalf("IsLoaded: %v", err)
	}
	if !loaded {
		t.Fatalf("%s is not loaded after LoadModel reported success", model)
	}

	// Unloading is documented as unsupported; assert it stays that way rather
	// than silently starting to lie if a future version changes.
	err = c.UnloadModel(liveCtx(t), model)
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Category != provider.ErrUnsupportedCapability {
		t.Fatalf("UnloadModel returned %v; the adapter declares this unsupported", err)
	}
}

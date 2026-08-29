package lemonade

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
// Lemonade Server.
//
// PROJECT.md §41 forbids the default suite from depending on external
// services, so every test below skips unless the variable is set. `make test`
// must never reach the network because of this file.
//
// These tests are also the intended way to replace this package's inferred
// endpoint knowledge with facts: they log what the server actually returns and
// treat a missing management endpoint as a skip, not a failure, so running
// them against a real server produces a correction list rather than noise.
const liveEnv = "BOOP_TEST_LEMONADE_URL"

func liveClient(t *testing.T) *Client {
	t.Helper()
	url := strings.TrimSpace(os.Getenv(liveEnv))
	if url == "" {
		t.Skipf("set %s to run tests against a real Lemonade server", liveEnv)
	}
	return New(url, nil, WithTimeout(60*time.Second))
}

func liveCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestLiveHealth(t *testing.T) {
	c := liveClient(t)
	if err := c.Health(liveCtx(t)); err != nil {
		t.Fatalf("Health: %v", err)
	}
}

// TestLiveServerStatus confirms or refutes the inferred health endpoint.
func TestLiveServerStatus(t *testing.T) {
	c := liveClient(t)

	loaded, err := c.ServerStatus(liveCtx(t))
	if err != nil {
		t.Logf("INFERRED endpoint %s did not work: %v", APIPath+healthPath, err)
		t.Skip("no usable native health endpoint on this build")
	}
	t.Logf("%s reports loaded model %q", APIPath+healthPath, loaded)
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
		t.Logf("%-44s ctx=%-7d caps=%v", m.ID, m.ContextWindow, m.Capabilities.Strings())
		if m.Provider != c.Name() {
			t.Fatalf("model %s attributed to %q", m.ID, m.Provider)
		}
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

// TestLiveModelLifecycle reports whether the inferred load/unload endpoints
// exist. An unsupported answer is a finding, not a failure: it is exactly the
// information needed to correct this package.
func TestLiveModelLifecycle(t *testing.T) {
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

	err = c.LoadModel(liveCtx(t), model)
	var pe *provider.Error
	if errors.As(err, &pe) && pe.Category == provider.ErrUnsupportedCapability {
		t.Skipf("INFERRED endpoint %s is not available: %v", APIPath+loadPath, err)
	}
	if err != nil {
		t.Fatalf("LoadModel(%s): %v", model, err)
	}

	err = c.UnloadModel(liveCtx(t), model)
	if errors.As(err, &pe) && pe.Category == provider.ErrUnsupportedCapability {
		t.Logf("INFERRED endpoint %s is not available: %v", APIPath+unloadPath, err)
		return
	}
	if err != nil {
		t.Fatalf("UnloadModel(%s): %v", model, err)
	}
}

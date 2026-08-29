package lmstudio

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/boop-dev/boop/internal/provider"
)

// loadedFixture is restFixture with qwen already resident, used to script a
// server whose state changes across a load.
const loadedFixture = `{"object":"list","data":[
 {"id":"qwen2.5-7b-instruct","object":"model","type":"llm","arch":"qwen2",
  "compatibility_type":"gguf","quantization":"Q4_K_M","state":"loaded",
  "max_context_length":32768,"loaded_context_length":32768}
]}`

func TestLoadModelJustInTime(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	f := newFake(t)
	f.handle(restModelsPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(w, restFixture) // not-loaded
			return
		}
		_, _ = io.WriteString(w, loadedFixture)
	})
	f.static("/v1/chat/completions", `{"id":"c","object":"chat.completion","choices":[
	  {"index":0,"message":{"role":"assistant","content":""},"finish_reason":"length"}]}`)
	_, c := f.start()

	if err := c.LoadModel(context.Background(), "qwen2.5-7b-instruct"); err != nil {
		t.Fatalf("LoadModel: %v", err)
	}

	body := f.lastBody("/v1/chat/completions")
	if got := body["model"]; got != "qwen2.5-7b-instruct" {
		t.Fatalf("model = %v", got)
	}
	if got := body["max_tokens"]; got != float64(1) {
		t.Fatalf("max_tokens = %#v, want 1: the probe must not generate", got)
	}
	if got := body["stream"]; got != false {
		t.Fatalf("stream = %#v, want false", got)
	}
}

func TestLoadModelSkipsAlreadyLoaded(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	f.static(restModelsPath, loadedFixture)
	_, c := f.start()

	if err := c.LoadModel(context.Background(), "qwen2.5-7b-instruct"); err != nil {
		t.Fatalf("LoadModel: %v", err)
	}
	if got := f.count("/v1/chat/completions"); got != 0 {
		t.Fatalf("a resident model was probed %d times; it should be left alone", got)
	}
}

func TestLoadModelUsesEmbeddingsEndpointForEmbeddingModels(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	f.static(restModelsPath, restFixture)
	f.static("/v1/embeddings", `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1]}]}`)
	_, c := f.start()

	// Verification will still report "not-loaded" because the fixture is
	// static; that is the honest-failure path, asserted below. What matters
	// here is that the probe went to the right endpoint.
	_ = c.LoadModel(context.Background(), "text-embedding-nomic-embed-text-v1.5")

	if got := f.count("/v1/embeddings"); got != 1 {
		t.Fatalf("/v1/embeddings called %d times, want 1", got)
	}
	if got := f.count("/v1/chat/completions"); got != 0 {
		t.Fatal("an embedding model must not be probed with a chat completion")
	}
}

func TestLoadModelReportsUnconfirmedLoad(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	// The state never changes: JIT loading is disabled, or the load failed.
	f.static(restModelsPath, restFixture)
	f.static("/v1/chat/completions", `{"id":"c","object":"chat.completion","choices":[
	  {"index":0,"message":{"role":"assistant","content":""},"finish_reason":"length"}]}`)
	_, c := f.start()

	pe := providerError(t, c.LoadModel(context.Background(), "qwen2.5-7b-instruct"))
	if pe.Category != provider.ErrUnavailable {
		t.Fatalf("category = %q, want %q", pe.Category, provider.ErrUnavailable)
	}
	if !strings.Contains(pe.Message, "did not load") {
		t.Fatalf("message = %q", pe.Message)
	}
}

func TestLoadModelWithoutRESTAPI(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	// No /api/v0: the probe is issued blind, and its success is all there is.
	f.static("/v1/chat/completions", `{"id":"c","object":"chat.completion","choices":[
	  {"index":0,"message":{"role":"assistant","content":""},"finish_reason":"length"}]}`)
	_, c := f.start()

	if err := c.LoadModel(context.Background(), "qwen2.5-7b-instruct"); err != nil {
		t.Fatalf("LoadModel: %v", err)
	}
	if got := f.count("/v1/chat/completions"); got != 1 {
		t.Fatalf("chat probe called %d times, want 1", got)
	}
}

func TestLoadModelPropagatesProbeFailure(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	f.status("/v1/chat/completions", http.StatusNotFound,
		`{"error":"Model 'ghost' not found. Please load a model first."}`)
	_, c := f.start()

	pe := providerError(t, c.LoadModel(context.Background(), "ghost"))
	if pe.Category != provider.ErrInvalidRequest {
		t.Fatalf("category = %q, want %q", pe.Category, provider.ErrInvalidRequest)
	}
}

func TestLoadModelRejectsEmptyModel(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	_, c := f.start()

	pe := providerError(t, c.LoadModel(context.Background(), " "))
	if pe.Category != provider.ErrInvalidRequest {
		t.Fatalf("category = %q, want %q", pe.Category, provider.ErrInvalidRequest)
	}
}

// TestUnloadModelIsHonest pins the deliberate refusal: LM Studio exposes no
// unload endpoint, and claiming success would leave the caller with a wrong
// picture of what is in memory.
func TestUnloadModelIsHonest(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	_, c := f.start()

	pe := providerError(t, c.UnloadModel(context.Background(), "qwen2.5-7b-instruct"))
	if pe.Category != provider.ErrUnsupportedCapability {
		t.Fatalf("category = %q, want %q", pe.Category, provider.ErrUnsupportedCapability)
	}
	if !strings.Contains(pe.Detail, "lms unload") {
		t.Fatalf("detail %q should point at the mechanism that does work", pe.Detail)
	}
	if got := f.count("/v1/chat/completions") + f.count(restModelsPath); got != 0 {
		t.Fatalf("UnloadModel made %d requests; it should make none", got)
	}
}

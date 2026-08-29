package lmstudio

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/boop-dev/boop/internal/provider"
)

// fake is a scriptable stand-in for an LM Studio server.
//
// Unrouted paths answer 404, which is precisely how an LM Studio build without
// the /api/v0 REST API behaves and therefore the fallback case worth covering.
type fake struct {
	t *testing.T

	mu     sync.Mutex
	routes map[string]http.HandlerFunc
	bodies map[string][]map[string]any
	hits   map[string]int
}

func newFake(t *testing.T) *fake {
	t.Helper()
	return &fake{
		t:      t,
		routes: make(map[string]http.HandlerFunc),
		bodies: make(map[string][]map[string]any),
		hits:   make(map[string]int),
	}
}

func (f *fake) handle(path string, fn http.HandlerFunc) *fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[path] = fn
	return f
}

func (f *fake) static(path, payload string) *fake {
	return f.handle(path, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, payload)
	})
}

func (f *fake) status(path string, code int, payload string) *fake {
	return f.handle(path, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, payload)
	})
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var decoded map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &decoded)
	}

	f.mu.Lock()
	f.hits[r.URL.Path]++
	if decoded != nil {
		f.bodies[r.URL.Path] = append(f.bodies[r.URL.Path], decoded)
	}
	handler, ok := f.routes[r.URL.Path]
	f.mu.Unlock()

	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"Unexpected endpoint or method."}`)
		return
	}
	handler(w, r)
}

func (f *fake) lastBody(path string) map[string]any {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	bodies := f.bodies[path]
	if len(bodies) == 0 {
		f.t.Fatalf("no request body recorded for %s", path)
	}
	return bodies[len(bodies)-1]
}

func (f *fake) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[path]
}

func (f *fake) start() (*httptest.Server, *Client) {
	f.t.Helper()
	srv := httptest.NewServer(f)
	f.t.Cleanup(srv.Close)
	return srv, New(srv.URL, srv.Client(), WithTimeout(5*time.Second))
}

// restFixture is a GET /api/v0/models response in the shape LM Studio's REST
// API documents. It is INFERRED, not captured from a live server, and the
// tests below exist to pin the adapter's behaviour against that shape rather
// than to prove the shape itself.
const restFixture = `{"object":"list","data":[
 {"id":"qwen2.5-7b-instruct","object":"model","type":"llm","publisher":"lmstudio-community",
  "arch":"qwen2","compatibility_type":"gguf","quantization":"Q4_K_M","state":"not-loaded",
  "max_context_length":32768},
 {"id":"llava-v1.5-7b","object":"model","type":"vlm","publisher":"lmstudio-community",
  "arch":"llama","compatibility_type":"gguf","quantization":"Q4_0","state":"loaded",
  "max_context_length":4096,"loaded_context_length":2048},
 {"id":"text-embedding-nomic-embed-text-v1.5","object":"model","type":"embeddings",
  "publisher":"nomic-ai","arch":"nomic-bert","compatibility_type":"gguf","quantization":"F16",
  "state":"not-loaded","max_context_length":2048}
]}`

// openAIFixture is the plain /v1/models listing every LM Studio version serves.
const openAIFixture = `{"object":"list","data":[
 {"id":"qwen2.5-7b-instruct","object":"model","owned_by":"organization_owner"},
 {"id":"llava-v1.5-7b","object":"model","owned_by":"organization_owner"}
]}`

// unreachableClient points at a server that has been shut down.
func unreachableClient(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	return New(url, nil, WithTimeout(2*time.Second))
}

func providerError(t *testing.T, err error) *provider.Error {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var pe *provider.Error
	if !errors.As(err, &pe) {
		t.Fatalf("error %v is not a *provider.Error", err)
	}
	return pe
}

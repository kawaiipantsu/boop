package ollama

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

// fake is a scriptable stand-in for an Ollama server.
//
// It records every request body so tests can assert on what the adapter sent —
// keep_alive in particular — and answers 404 for unrouted paths, which is how
// an older or non-Ollama server behaves and therefore exactly the fallback
// case worth covering.
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

// handle registers a handler for path.
func (f *fake) handle(path string, fn http.HandlerFunc) *fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[path] = fn
	return f
}

// static registers a fixed JSON response for path.
func (f *fake) static(path, payload string) *fake {
	return f.handle(path, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, payload)
	})
}

// status registers a failing response for path.
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
		_, _ = io.WriteString(w, `{"error":"404 page not found"}`)
		return
	}
	handler(w, r)
}

// lastBody returns the most recent decoded request body seen at path.
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

// count reports how many requests hit path.
func (f *fake) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[path]
}

// start runs the fake and returns a Client pointed at it.
func (f *fake) start() (*httptest.Server, *Client) {
	f.t.Helper()
	srv := httptest.NewServer(f)
	f.t.Cleanup(srv.Close)
	return srv, New(srv.URL, srv.Client(), WithTimeout(5*time.Second))
}

// tagsFixture is a trimmed copy of a real GET /api/tags response from Ollama
// 0.31.2, keeping one model per capability shape that matters.
const tagsFixture = `{"models":[
 {"name":"llama3.1:8b","model":"llama3.1:8b","size":4920753328,"digest":"46e0c10c",
  "details":{"format":"gguf","family":"llama","parameter_size":"8.0B","quantization_level":"Q4_K_M","context_length":131072,"embedding_length":4096},
  "capabilities":["completion","tools"]},
 {"name":"qwen:7b","model":"qwen:7b","size":4511914544,"digest":"2091ee8c",
  "details":{"format":"gguf","family":"qwen2","parameter_size":"8B","quantization_level":"Q4_0","context_length":32768,"embedding_length":4096},
  "capabilities":["completion"]},
 {"name":"nomic-embed-text:latest","model":"nomic-embed-text:latest","size":274302450,"digest":"0a109f42",
  "details":{"format":"gguf","family":"nomic-bert","parameter_size":"137M","quantization_level":"F16","context_length":2048,"embedding_length":768},
  "capabilities":["embedding"]},
 {"name":"llava:13b","model":"llava:13b","size":8000000000,"digest":"deadbeef",
  "details":{"format":"gguf","family":"llama","parameter_size":"13B","quantization_level":"Q4_0","context_length":4096},
  "capabilities":["completion","vision","thinking"]}
]}`

// unreachableClient points at a server that has been shut down, which is the
// most common real failure: the daemon is not running.
func unreachableClient(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	return New(url, nil, WithTimeout(2*time.Second))
}

// providerError extracts the normalized error, failing when err is not one.
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

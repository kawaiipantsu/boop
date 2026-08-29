package lemonade

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

// fake is a scriptable stand-in for a Lemonade Server.
//
// Unrouted paths answer 404, which is how a build without a given management
// endpoint behaves — the case this adapter is written to survive, since every
// native path it uses is inferred rather than verified.
type fake struct {
	t *testing.T

	mu     sync.Mutex
	routes map[string]http.HandlerFunc
	bodies map[string][]json.RawMessage
	hits   map[string]int
}

func newFake(t *testing.T) *fake {
	t.Helper()
	return &fake{
		t:      t,
		routes: make(map[string]http.HandlerFunc),
		bodies: make(map[string][]json.RawMessage),
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

	f.mu.Lock()
	f.hits[r.URL.Path]++
	if len(raw) > 0 {
		f.bodies[r.URL.Path] = append(f.bodies[r.URL.Path], json.RawMessage(raw))
	}
	handler, ok := f.routes[r.URL.Path]
	f.mu.Unlock()

	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"detail":"Not Found"}`)
		return
	}
	handler(w, r)
}

// rawBodies returns every request body seen at path, in order.
func (f *fake) rawBodies(path string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.bodies[path]))
	for _, b := range f.bodies[path] {
		out = append(out, string(b))
	}
	return out
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

// api prefixes a management path with Lemonade's API prefix, matching what the
// adapter puts on the wire.
func api(path string) string { return APIPath + path }

// modelsFixture is a plain OpenAI model listing, which is the only model shape
// this adapter relies on.
const modelsFixture = `{"object":"list","data":[
 {"id":"Llama-3.2-1B-Instruct-Hybrid","object":"model","owned_by":"lemonade"},
 {"id":"Qwen2.5-7B-Instruct-CPU","object":"model","owned_by":"lemonade"},
 {"id":"nomic-embed-text-v1-GGUF","object":"model","owned_by":"lemonade"}
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

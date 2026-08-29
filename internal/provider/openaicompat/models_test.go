package openaicompat

import (
	"context"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kawaiipantsu/boop/internal/provider"
)

func TestNewAppliesDefaults(t *testing.T) {
	c := New(Options{BaseURL: "http://127.0.0.1:1234/v1/"})
	if c.Name() != DefaultName {
		t.Errorf("Name() = %q, want %q", c.Name(), DefaultName)
	}
	if c.BaseURL() != "http://127.0.0.1:1234/v1" {
		t.Errorf("BaseURL() = %q, want the trailing slash trimmed", c.BaseURL())
	}
	if c.modelsPath != DefaultModelsPath || c.chatPath != DefaultChatPath || c.embeddingsPath != DefaultEmbeddingsPath {
		t.Errorf("paths = %q/%q/%q", c.modelsPath, c.chatPath, c.embeddingsPath)
	}
	if c.timeout != DefaultTimeout {
		t.Errorf("timeout = %v, want %v", c.timeout, DefaultTimeout)
	}
	if c.http == nil {
		t.Error("http client is nil")
	}
}

func TestEndpointJoining(t *testing.T) {
	c := New(Options{BaseURL: "http://host:1234/v1"})
	tests := []struct {
		path string
		want string
	}{
		{"/models", "http://host:1234/v1/models"},
		{"models", "http://host:1234/v1/models"},
		{"", "http://host:1234/v1"},
		{"http://host:1234/api/tags", "http://host:1234/api/tags"},
		{"https://other/api", "https://other/api"},
	}
	for _, tc := range tests {
		if got := c.endpoint(tc.path); got != tc.want {
			t.Errorf("endpoint(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestRequestHeaders(t *testing.T) {
	var got http.Header
	srv, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	client := New(Options{
		Name:    "cloud",
		BaseURL: srv.URL + "/v1",
		APIKey:  "secret-key-value",
		Headers: map[string]string{"X-Boop": "1", "User-Agent": "boop-test"},
		Timeout: 5 * time.Second,
	})

	if _, err := client.ListModels(context.Background()); err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if got.Get("Authorization") != "Bearer secret-key-value" {
		t.Errorf("Authorization = %q", got.Get("Authorization"))
	}
	if got.Get("X-Boop") != "1" {
		t.Errorf("X-Boop = %q", got.Get("X-Boop"))
	}
	if got.Get("User-Agent") != "boop-test" {
		t.Errorf("User-Agent = %q, want the Headers override to win", got.Get("User-Agent"))
	}
	if got.Get("Accept") != "application/json" {
		t.Errorf("Accept = %q", got.Get("Accept"))
	}
}

func TestNoAuthorizationHeaderWithoutKey(t *testing.T) {
	var got http.Header
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	if err := client.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if _, ok := got["Authorization"]; ok {
		t.Error("Authorization header sent without a configured key")
	}
}

func TestListModelsResponseShapes(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{"data envelope", `{"object":"list","data":[{"id":"a"},{"id":"b"}]}`, []string{"a", "b"}},
		{"bare array", `[{"id":"a"},{"id":"b"}]`, []string{"a", "b"}},
		{"models envelope", `{"models":[{"id":"a"},{"model":"b"}]}`, []string{"a", "b"}},
		{"skips empty ids", `{"data":[{"id":""},{"id":"b"}]}`, []string{"b"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/models" {
					t.Errorf("path = %q, want /v1/models", r.URL.Path)
				}
				_, _ = w.Write([]byte(tc.body))
			})
			models, err := client.ListModels(context.Background())
			if err != nil {
				t.Fatalf("ListModels: %v", err)
			}
			var ids []string
			for _, m := range models {
				ids = append(ids, m.ID)
				if m.Provider != "testprov" {
					t.Errorf("provider = %q, want testprov", m.Provider)
				}
			}
			if !reflect.DeepEqual(ids, tc.want) {
				t.Errorf("ids = %v, want %v", ids, tc.want)
			}
		})
	}
}

func TestListModelsMetadataAndRefinement(t *testing.T) {
	_, base := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[
			{"id":"local-chat","max_context_length":8192,"max_output_tokens":1024,"capabilities":["tool_use","vision"]},
			{"id":"nomic-embed-text"}
		]}`))
	})
	client := New(Options{
		Name:    "testprov",
		BaseURL: base.BaseURL(),
		Timeout: 5 * time.Second,
		RefineModels: func(models []provider.Model) []provider.Model {
			out := make([]provider.Model, 0, len(models))
			for _, m := range models {
				m.DisplayName = "· " + m.ID
				out = append(out, m)
			}
			return out
		},
		RefineCapabilities: func(model string, caps provider.Capabilities) provider.Capabilities {
			if model == "local-chat" {
				return caps.Add(provider.CapabilityResponses)
			}
			return caps
		},
	})

	models, err := client.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %d, want 2", len(models))
	}

	chat := models[0]
	if chat.ContextWindow != 8192 || chat.MaxOutput != 1024 {
		t.Errorf("context/output = %d/%d", chat.ContextWindow, chat.MaxOutput)
	}
	if chat.DisplayName != "· local-chat" {
		t.Errorf("DisplayName = %q, want the RefineModels hook applied", chat.DisplayName)
	}
	if !chat.Capabilities.HasAll(provider.CapabilityTools, provider.CapabilityVision, provider.CapabilityResponses) {
		t.Errorf("capabilities = %v, want declared plus refined", chat.Capabilities)
	}

	embed := models[1]
	if !embed.Capabilities.Has(provider.CapabilityEmbeddings) || embed.Capabilities.Has(provider.CapabilityTools) {
		t.Errorf("embedding model capabilities = %v", embed.Capabilities)
	}

	// ListModels warms the capability cache, so this must not hit the server.
	caps, err := client.Capabilities(context.Background(), "local-chat")
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if !reflect.DeepEqual(caps, chat.Capabilities) {
		t.Errorf("cached capabilities = %v, want %v", caps, chat.Capabilities)
	}
}

func TestGetAndPostJSON(t *testing.T) {
	var seen struct {
		method string
		path   string
		body   map[string]any
	}
	srv, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		seen.method = r.Method
		seen.path = r.URL.Path
		if r.Method == http.MethodPost {
			seen.body = decodeBody(t, r)
			if ct := r.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q", ct)
			}
		}
		_, _ = w.Write([]byte(`{"ok":true,"n":3}`))
	})

	var out struct {
		OK bool `json:"ok"`
		N  int  `json:"n"`
	}
	if err := client.GetJSON(context.Background(), "/native/status", &out); err != nil {
		t.Fatalf("GetJSON: %v", err)
	}
	if !out.OK || out.N != 3 {
		t.Errorf("decoded = %+v", out)
	}
	if seen.method != http.MethodGet || seen.path != "/v1/native/status" {
		t.Errorf("request = %s %s", seen.method, seen.path)
	}

	// An absolute URL escapes the versioned base, which is how adapters reach
	// native endpoints such as Ollama's /api/tags.
	if err := client.GetJSON(context.Background(), srv.URL+"/api/tags", nil); err != nil {
		t.Fatalf("GetJSON absolute: %v", err)
	}
	if seen.path != "/api/tags" {
		t.Errorf("absolute path = %q, want /api/tags", seen.path)
	}

	if err := client.PostJSON(context.Background(), "/api/pull", map[string]any{"name": "llama3"}, &out); err != nil {
		t.Fatalf("PostJSON: %v", err)
	}
	if seen.method != http.MethodPost || seen.body["name"] != "llama3" {
		t.Errorf("post = %s %v", seen.method, seen.body)
	}
}

func TestPostJSONPropagatesNormalizedErrors(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
	})
	err := client.PostJSON(context.Background(), "/api/anything", map[string]any{}, nil)
	if cat, ok := provider.CategoryOf(err); !ok || cat != provider.ErrRateLimited {
		t.Errorf("category = %v (ok=%v), want rate_limited", cat, ok)
	}
}

func TestHealthSucceedsAndCounts(t *testing.T) {
	var calls atomic.Int32
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
	})
	if err := client.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("requests = %d, want a single cheap probe", got)
	}
}

func TestEmbed(t *testing.T) {
	var body map[string]any
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("path = %q", r.URL.Path)
		}
		body = decodeBody(t, r)
		// Deliberately out of order to prove the client re-sorts by index.
		_, _ = w.Write([]byte(`{"data":[
			{"index":1,"embedding":[0.3,0.4]},
			{"index":0,"embedding":[0.1,0.2]}
		],"usage":{"prompt_tokens":6,"total_tokens":6}}`))
	})

	resp, err := client.Embed(context.Background(), provider.EmbeddingRequest{
		Model: "nomic-embed-text",
		Input: []string{"first", "second"},
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if body["model"] != "nomic-embed-text" {
		t.Errorf("model = %v", body["model"])
	}
	want := [][]float32{{0.1, 0.2}, {0.3, 0.4}}
	if !reflect.DeepEqual(resp.Vectors, want) {
		t.Errorf("vectors = %v, want %v (input order)", resp.Vectors, want)
	}
	if resp.Usage.PromptTokens != 6 || resp.Usage.TotalTokens != 6 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestEmbedValidation(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	tests := []struct {
		name string
		req  provider.EmbeddingRequest
		want provider.ErrorCategory
	}{
		{"no model", provider.EmbeddingRequest{Input: []string{"x"}}, provider.ErrInvalidRequest},
		{"no input", provider.EmbeddingRequest{Model: "m"}, provider.ErrInvalidRequest},
		{"empty data", provider.EmbeddingRequest{Model: "m", Input: []string{"x"}}, provider.ErrMalformedResponse},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.Embed(context.Background(), tc.req)
			if cat, ok := provider.CategoryOf(err); !ok || cat != tc.want {
				t.Errorf("category = %v (ok=%v), want %v", cat, ok, tc.want)
			}
		})
	}
}

func TestClientImplementsOptionalInterfaces(t *testing.T) {
	var c any = New(Options{BaseURL: "http://127.0.0.1:1/v1"})
	if _, ok := c.(provider.Provider); !ok {
		t.Error("Client does not implement provider.Provider")
	}
	if _, ok := c.(provider.EmbeddingProvider); !ok {
		t.Error("Client does not implement provider.EmbeddingProvider")
	}
}

func TestCustomPathsAreUsed(t *testing.T) {
	var paths []string
	srv, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_, _ = w.Write([]byte(`{"data":[{"id":"m"}],"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}]}`))
	})
	client := New(Options{
		BaseURL:        srv.URL + "/openai/v1",
		ModelsPath:     "/model-list",
		ChatPath:       "/completions",
		EmbeddingsPath: "/embed",
		Timeout:        5 * time.Second,
	})
	if err := client.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
	collect(t, mustChat(t, context.Background(), client, simpleRequest(false)))

	want := []string{"/openai/v1/model-list", "/openai/v1/completions"}
	if !reflect.DeepEqual(paths, want) {
		t.Errorf("paths = %v, want %v", paths, want)
	}
}

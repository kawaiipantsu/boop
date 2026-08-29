package ollama

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/kawaiipantsu/boop/internal/provider"
)

func TestMapCapabilities(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		declared []string
		want     provider.Capabilities
	}{
		{
			name:     "nothing declared stays empty so callers can tell",
			declared: nil,
			want:     nil,
		},
		{
			name:     "completion implies streaming and structured output",
			declared: []string{"completion"},
			want:     provider.Capabilities{provider.CapabilityStreaming, provider.CapabilityStructuredOutput},
		},
		{
			name:     "tools",
			declared: []string{"completion", "tools"},
			want: provider.Capabilities{
				provider.CapabilityStreaming, provider.CapabilityStructuredOutput, provider.CapabilityTools,
			},
		},
		{
			name:     "embedding only, never chat",
			declared: []string{"embedding"},
			want:     provider.Capabilities{provider.CapabilityEmbeddings},
		},
		{
			name:     "vision and thinking",
			declared: []string{"completion", "vision", "thinking"},
			want: provider.Capabilities{
				provider.CapabilityReasoning, provider.CapabilityStreaming,
				provider.CapabilityStructuredOutput, provider.CapabilityVision,
			},
		},
		{
			name:     "unknown tokens are dropped rather than guessed at",
			declared: []string{"completion", "insert", "wombat"},
			want:     provider.Capabilities{provider.CapabilityStreaming, provider.CapabilityStructuredOutput},
		},
		{
			name:     "case and padding tolerated",
			declared: []string{" Completion ", "TOOLS"},
			want: provider.Capabilities{
				provider.CapabilityStreaming, provider.CapabilityStructuredOutput, provider.CapabilityTools,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := mapCapabilities(tc.declared)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("mapCapabilities(%v) = %v, want %v", tc.declared, got, tc.want)
			}
		})
	}
}

func TestListModelsUsesTags(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	f.static(tagsPath, tagsFixture)
	_, c := f.start()

	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 4 {
		t.Fatalf("got %d models, want 4", len(models))
	}
	// Sorted by id for a stable picker order.
	wantOrder := []string{"llama3.1:8b", "llava:13b", "nomic-embed-text:latest", "qwen:7b"}
	for i, want := range wantOrder {
		if models[i].ID != want {
			t.Fatalf("models[%d].ID = %q, want %q", i, models[i].ID, want)
		}
		if models[i].Provider != ProviderName {
			t.Fatalf("models[%d].Provider = %q, want %q", i, models[i].Provider, ProviderName)
		}
	}

	byID := map[string]provider.Model{}
	for _, m := range models {
		byID[m.ID] = m
	}

	if got := byID["llama3.1:8b"].ContextWindow; got != 131072 {
		t.Fatalf("llama3.1:8b context window = %d, want 131072", got)
	}
	if got := byID["llama3.1:8b"].DisplayName; got != "llama3.1:8b (8.0B Q4_K_M)" {
		t.Fatalf("display name = %q", got)
	}
	if !byID["llama3.1:8b"].Capabilities.Has(provider.CapabilityTools) {
		t.Fatal("llama3.1:8b should declare tools")
	}
	// The whole point of reading /api/tags: the shared name heuristics would
	// have assumed tools here, and asking for them returns HTTP 400.
	if byID["qwen:7b"].Capabilities.Has(provider.CapabilityTools) {
		t.Fatal("qwen:7b declares only completion and must not advertise tools")
	}
	if !byID["nomic-embed-text:latest"].Capabilities.Has(provider.CapabilityEmbeddings) {
		t.Fatal("nomic-embed-text should declare embeddings")
	}
	if byID["nomic-embed-text:latest"].Capabilities.Has(provider.CapabilityStreaming) {
		t.Fatal("an embedding model must not be offered for chat")
	}
	if !byID["llava:13b"].Capabilities.HasAll(provider.CapabilityVision, provider.CapabilityReasoning) {
		t.Fatalf("llava:13b capabilities = %v", byID["llava:13b"].Capabilities)
	}
}

func TestListModelsFallsBackToOpenAIListing(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	// No /api/tags route: the fake answers 404, as a proxy exposing only the
	// OpenAI surface would.
	f.static("/v1/models", `{"object":"list","data":[{"id":"llama3.1:8b"}]}`)
	_, c := f.start()

	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 1 || models[0].ID != "llama3.1:8b" {
		t.Fatalf("got %+v, want the single OpenAI-listed model", models)
	}
	if len(models[0].Capabilities) == 0 {
		t.Fatal("fallback path should still derive capabilities")
	}
}

func TestListModelsFailsWhenServerIsDown(t *testing.T) {
	t.Parallel()

	c := unreachableClient(t)
	if _, err := c.ListModels(context.Background()); err == nil {
		t.Fatal("expected an error from an unreachable server")
	} else if pe := providerError(t, err); pe.Category != provider.ErrUnavailable {
		t.Fatalf("category = %q, want %q", pe.Category, provider.ErrUnavailable)
	}
}

func TestCapabilities(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		model    string
		wantHas  []provider.Capability
		wantMiss []provider.Capability
	}{
		{
			name:    "exact tag",
			model:   "llama3.1:8b",
			wantHas: []provider.Capability{provider.CapabilityStreaming, provider.CapabilityTools},
		},
		{
			name:     "completion-only model has no tools",
			model:    "qwen:7b",
			wantHas:  []provider.Capability{provider.CapabilityStreaming},
			wantMiss: []provider.Capability{provider.CapabilityTools},
		},
		{
			name:    "implicit latest tag",
			model:   "nomic-embed-text",
			wantHas: []provider.Capability{provider.CapabilityEmbeddings},
		},
		{
			name:    "bare repository name resolves to its only tag",
			model:   "llava",
			wantHas: []provider.Capability{provider.CapabilityVision},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			f.static(tagsPath, tagsFixture)
			_, c := f.start()

			caps, err := c.Capabilities(context.Background(), tc.model)
			if err != nil {
				t.Fatalf("Capabilities(%q): %v", tc.model, err)
			}
			if !caps.HasAll(tc.wantHas...) {
				t.Fatalf("Capabilities(%q) = %v, missing %v", tc.model, caps, caps.Missing(tc.wantHas...))
			}
			for _, unwanted := range tc.wantMiss {
				if caps.Has(unwanted) {
					t.Fatalf("Capabilities(%q) = %v, must not contain %q", tc.model, caps, unwanted)
				}
			}
		})
	}
}

func TestCapabilitiesRejectsEmptyModel(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	f.static(tagsPath, tagsFixture)
	_, c := f.start()

	pe := providerError(t, func() error {
		_, err := c.Capabilities(context.Background(), "  ")
		return err
	}())
	if pe.Category != provider.ErrInvalidRequest {
		t.Fatalf("category = %q, want %q", pe.Category, provider.ErrInvalidRequest)
	}
}

func TestCapabilitiesFallsBackToShow(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	f.static(tagsPath, `{"models":[]}`)
	f.static(showPath, `{"details":{"family":"qwen2"},
	  "model_info":{"qwen2.usage.context_length":32768,"general.parameter_count":7000000000},
	  "capabilities":["completion","tools"]}`)
	_, c := f.start()

	caps, err := c.Capabilities(context.Background(), "sha256:abcdef")
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if !caps.HasAll(provider.CapabilityStreaming, provider.CapabilityTools) {
		t.Fatalf("caps = %v", caps)
	}
	if f.count(showPath) != 1 {
		t.Fatalf("/api/show called %d times, want 1", f.count(showPath))
	}
	if got := f.lastBody(showPath)["model"]; got != "sha256:abcdef" {
		t.Fatalf("show body model = %v", got)
	}
}

func TestCapabilitiesFallsBackToHeuristics(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	// Neither native endpoint exists; only the OpenAI listing does.
	f.static("/v1/models", `{"object":"list","data":[{"id":"llama3.1:8b"}]}`)
	_, c := f.start()

	caps, err := c.Capabilities(context.Background(), "llama3.1:8b")
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if !caps.Has(provider.CapabilityStreaming) {
		t.Fatalf("heuristic fallback produced %v", caps)
	}
}

func TestShow(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	f.static(showPath, `{"details":{"family":"nomic-bert","parameter_size":"137M","quantization_level":"F16"},
	  "model_info":{"nomic-bert.context_length":2048},
	  "capabilities":["embedding"]}`)
	_, c := f.start()

	got, err := c.Show(context.Background(), "nomic-embed-text")
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	if got.ContextWindow != 2048 {
		t.Fatalf("ContextWindow = %d, want 2048", got.ContextWindow)
	}
	if got.DisplayName != "nomic-embed-text (137M F16)" {
		t.Fatalf("DisplayName = %q", got.DisplayName)
	}
	if !got.Capabilities.Has(provider.CapabilityEmbeddings) {
		t.Fatalf("Capabilities = %v", got.Capabilities)
	}
}

func TestShowMissingModel(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	f.status(showPath, http.StatusNotFound, `{"error":"model 'ghost' not found"}`)
	_, c := f.start()

	_, err := c.Show(context.Background(), "ghost")
	pe := providerError(t, err)
	if pe.Category != provider.ErrInvalidRequest {
		t.Fatalf("category = %q, want %q", pe.Category, provider.ErrInvalidRequest)
	}
	if pe.Message != "model 'ghost' not found" {
		t.Fatalf("message = %q, want the server's own wording", pe.Message)
	}
}

func TestShowResponseContextLength(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		resp showResponse
		want int
	}{
		{
			name: "details wins when present",
			resp: showResponse{
				Details:   tagDetails{Family: "qwen2", ContextLength: 4096},
				ModelInfo: map[string]any{"qwen2.context_length": float64(32768)},
			},
			want: 4096,
		},
		{
			name: "family-prefixed model_info key",
			resp: showResponse{
				Details:   tagDetails{Family: "qwen2"},
				ModelInfo: map[string]any{"qwen2.usage.context_length": float64(32768), "clip.context_length": float64(77)},
			},
			want: 32768,
		},
		{
			name: "largest context_length when no family matches",
			resp: showResponse{
				ModelInfo: map[string]any{"a.context_length": float64(2048), "b.context_length": float64(8192)},
			},
			want: 8192,
		},
		{
			name: "string values are tolerated",
			resp: showResponse{
				Details:   tagDetails{Family: "llama"},
				ModelInfo: map[string]any{"llama.context_length": "131072"},
			},
			want: 131072,
		},
		{
			name: "unrelated keys ignored",
			resp: showResponse{ModelInfo: map[string]any{"llama.embedding_length": float64(4096)}},
			want: 0,
		},
		{
			name: "nothing at all",
			resp: showResponse{},
			want: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.resp.contextLength(); got != tc.want {
				t.Fatalf("contextLength() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestLookupTag(t *testing.T) {
	t.Parallel()

	c := New("", nil)
	c.storeTags([]tagModel{
		{Name: "llama3.1:8b"},
		{Name: "llama3.2:latest"},
		{Name: "qwen:7b"},
		{Name: "qwen2.5:7b"},
		{Name: "qwen2.5:14b"},
	})

	tests := []struct {
		name  string
		query string
		want  string
		found bool
	}{
		{"exact", "llama3.1:8b", "llama3.1:8b", true},
		{"implicit latest", "llama3.2", "llama3.2:latest", true},
		{"explicit latest against a tagged entry", "llama3.1:latest", "llama3.1:8b", true},
		{"bare repository with one tag", "qwen", "qwen:7b", true},
		{"bare repository with two tags is ambiguous", "qwen2.5", "", false},
		{"unknown", "mistral", "", false},
		{"empty", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := c.lookupTag(tc.query)
			if ok != tc.found {
				t.Fatalf("lookupTag(%q) found = %v, want %v", tc.query, ok, tc.found)
			}
			if ok && got.Name != tc.want {
				t.Fatalf("lookupTag(%q) = %q, want %q", tc.query, got.Name, tc.want)
			}
		})
	}
}

func TestStoreTagsReplacesDeletedModels(t *testing.T) {
	t.Parallel()

	c := New("", nil)
	c.storeTags([]tagModel{{Name: "old:1"}, {Name: "keep:1"}})
	c.storeTags([]tagModel{{Name: "keep:1"}})

	if _, ok := c.lookupTag("old:1"); ok {
		t.Fatal("a model removed from the server must disappear from the cache")
	}
	if _, ok := c.lookupTag("keep:1"); !ok {
		t.Fatal("keep:1 should still be cached")
	}
}

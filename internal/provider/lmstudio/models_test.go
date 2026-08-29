package lmstudio

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/boop-dev/boop/internal/provider"
)

func TestRefine(t *testing.T) {
	t.Parallel()

	base := provider.Capabilities{}.Add(
		provider.CapabilityStreaming,
		provider.CapabilityStructuredOutput,
		provider.CapabilityTools,
		provider.CapabilityVision,
	)

	tests := []struct {
		name string
		info ModelInfo
		base provider.Capabilities
		want provider.Capabilities
	}{
		{
			name: "embeddings replaces everything, never chat",
			info: ModelInfo{Type: typeEmbedding},
			base: base,
			want: provider.Capabilities{provider.CapabilityEmbeddings},
		},
		{
			name: "vlm keeps vision",
			info: ModelInfo{Type: typeVLM},
			base: provider.Capabilities{}.Add(provider.CapabilityTools),
			want: provider.Capabilities{
				provider.CapabilityStreaming, provider.CapabilityTools, provider.CapabilityVision,
			},
		},
		{
			name: "llm withdraws a name-derived vision guess",
			info: ModelInfo{Type: typeLLM},
			base: base,
			want: provider.Capabilities{
				provider.CapabilityStreaming, provider.CapabilityStructuredOutput, provider.CapabilityTools,
			},
		},
		{
			name: "unknown type leaves the shared detection alone",
			info: ModelInfo{Type: "something-new"},
			base: base,
			want: base,
		},
		{
			name: "refinement is idempotent",
			info: ModelInfo{Type: typeLLM},
			base: refine(ModelInfo{Type: typeLLM}, base),
			want: provider.Capabilities{
				provider.CapabilityStreaming, provider.CapabilityStructuredOutput, provider.CapabilityTools,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := refine(tc.info, tc.base)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("refine() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestListModelsUsesRESTAPI(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	f.static(restModelsPath, restFixture)
	f.static("/v1/models", openAIFixture)
	_, c := f.start()

	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 3 {
		t.Fatalf("got %d models, want 3", len(models))
	}

	byID := map[string]provider.Model{}
	for _, m := range models {
		byID[m.ID] = m
		if m.Provider != ProviderName {
			t.Fatalf("%s attributed to %q", m.ID, m.Provider)
		}
	}

	if got := byID["qwen2.5-7b-instruct"].ContextWindow; got != 32768 {
		t.Fatalf("context window = %d, want 32768", got)
	}
	if byID["qwen2.5-7b-instruct"].Capabilities.Has(provider.CapabilityVision) {
		t.Fatal(`a model typed "llm" must not advertise vision`)
	}
	if !byID["llava-v1.5-7b"].Capabilities.Has(provider.CapabilityVision) {
		t.Fatal(`a model typed "vlm" must advertise vision`)
	}
	if got := byID["llava-v1.5-7b"].DisplayName; got != "llava-v1.5-7b (Q4_0, gguf, loaded)" {
		t.Fatalf("display name = %q", got)
	}
	embed := byID["text-embedding-nomic-embed-text-v1.5"].Capabilities
	if !embed.Has(provider.CapabilityEmbeddings) || embed.Has(provider.CapabilityStreaming) {
		t.Fatalf("embedding model capabilities = %v", embed)
	}
}

func TestListModelsFallsBackWhenRESTAPIIsAbsent(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	// No /api/v0 route: an older LM Studio build, or a proxy.
	f.static("/v1/models", openAIFixture)
	_, c := f.start()

	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2 from the OpenAI listing", len(models))
	}
	if len(models[0].Capabilities) == 0 {
		t.Fatal("fallback path should still derive capabilities")
	}

	// The absent REST API is remembered, so it is probed once and not again.
	if _, err := c.ListModels(context.Background()); err != nil {
		t.Fatalf("second ListModels: %v", err)
	}
	if got := f.count(restModelsPath); got != 1 {
		t.Fatalf("%s probed %d times, want 1", restModelsPath, got)
	}
}

func TestListModelsFailsWhenServerIsDown(t *testing.T) {
	t.Parallel()

	c := unreachableClient(t)
	_, err := c.ListModels(context.Background())
	if pe := providerError(t, err); pe.Category != provider.ErrUnavailable {
		t.Fatalf("category = %q, want %q", pe.Category, provider.ErrUnavailable)
	}
}

func TestNativeModelsAndLoadState(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	f.static(restModelsPath, restFixture)
	_, c := f.start()

	infos, err := c.NativeModels(context.Background())
	if err != nil {
		t.Fatalf("NativeModels: %v", err)
	}
	if len(infos) != 3 {
		t.Fatalf("got %d records, want 3", len(infos))
	}

	tests := []struct {
		model string
		want  bool
	}{
		{"llava-v1.5-7b", true},
		{"qwen2.5-7b-instruct", false},
	}
	for _, tc := range tests {
		got, err := c.IsLoaded(context.Background(), tc.model)
		if err != nil {
			t.Fatalf("IsLoaded(%q): %v", tc.model, err)
		}
		if got != tc.want {
			t.Fatalf("IsLoaded(%q) = %v, want %v", tc.model, got, tc.want)
		}
	}

	if _, err := c.IsLoaded(context.Background(), "not-installed"); err == nil {
		t.Fatal("expected an error for an unknown model")
	}
}

func TestNativeModelsWithoutRESTAPI(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	f.status(restModelsPath, http.StatusNotFound, `{"error":"Unexpected endpoint"}`)
	_, c := f.start()

	_, err := c.NativeModels(context.Background())
	pe := providerError(t, err)
	if pe.Category != provider.ErrUnsupportedCapability {
		t.Fatalf("category = %q, want %q", pe.Category, provider.ErrUnsupportedCapability)
	}

	// The verdict is remembered rather than re-probed on every call.
	if _, err := c.NativeModels(context.Background()); err == nil {
		t.Fatal("expected the same error on the second call")
	}
	if got := f.count(restModelsPath); got != 1 {
		t.Fatalf("%s probed %d times, want 1", restModelsPath, got)
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
			name:     "llm",
			model:    "qwen2.5-7b-instruct",
			wantHas:  []provider.Capability{provider.CapabilityStreaming, provider.CapabilityTools},
			wantMiss: []provider.Capability{provider.CapabilityVision},
		},
		{
			name:    "vlm",
			model:   "llava-v1.5-7b",
			wantHas: []provider.Capability{provider.CapabilityVision, provider.CapabilityStreaming},
		},
		{
			name:     "embeddings",
			model:    "text-embedding-nomic-embed-text-v1.5",
			wantHas:  []provider.Capability{provider.CapabilityEmbeddings},
			wantMiss: []provider.Capability{provider.CapabilityStreaming, provider.CapabilityTools},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			f.static(restModelsPath, restFixture)
			f.static("/v1/models", openAIFixture)
			_, c := f.start()

			caps, err := c.Capabilities(context.Background(), tc.model)
			if err != nil {
				t.Fatalf("Capabilities: %v", err)
			}
			if !caps.HasAll(tc.wantHas...) {
				t.Fatalf("caps = %v, missing %v", caps, caps.Missing(tc.wantHas...))
			}
			for _, unwanted := range tc.wantMiss {
				if caps.Has(unwanted) {
					t.Fatalf("caps = %v, must not contain %q", caps, unwanted)
				}
			}
		})
	}
}

func TestCapabilitiesRejectsEmptyModel(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	f.static(restModelsPath, restFixture)
	_, c := f.start()

	_, err := c.Capabilities(context.Background(), " ")
	if pe := providerError(t, err); pe.Category != provider.ErrInvalidRequest {
		t.Fatalf("category = %q, want %q", pe.Category, provider.ErrInvalidRequest)
	}
}

func TestStoreNativeReplacesRemovedModels(t *testing.T) {
	t.Parallel()

	c := New("", nil)
	c.storeNative([]ModelInfo{{ID: "old"}, {ID: "keep"}})
	c.storeNative([]ModelInfo{{ID: "keep"}})

	if _, ok := c.cachedNative("old"); ok {
		t.Fatal("a model removed from the server must disappear from the cache")
	}
	if _, ok := c.cachedNative("keep"); !ok {
		t.Fatal("keep should still be cached")
	}
}

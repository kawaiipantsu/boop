package openaicompat

import (
	"context"
	"net/http"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/boop-dev/boop/internal/provider"
)

func TestHeuristicCapabilities(t *testing.T) {
	tests := []struct {
		model      string
		want       []provider.Capability
		wantAbsent []provider.Capability
	}{
		{
			model: "llama3.1:8b",
			want:  []provider.Capability{provider.CapabilityStreaming, provider.CapabilityTools, provider.CapabilityStructuredOutput},
			wantAbsent: []provider.Capability{
				provider.CapabilityVision, provider.CapabilityReasoning, provider.CapabilityEmbeddings,
			},
		},
		{
			model:      "Qwen2.5-VL-7B-Instruct",
			want:       []provider.Capability{provider.CapabilityVision, provider.CapabilityTools},
			wantAbsent: []provider.Capability{provider.CapabilityReasoning},
		},
		{
			model: "llava:13b",
			want:  []provider.Capability{provider.CapabilityVision},
		},
		{
			model: "gpt-4o-mini",
			want:  []provider.Capability{provider.CapabilityVision, provider.CapabilityTools},
		},
		{
			model:      "deepseek-r1:14b",
			want:       []provider.Capability{provider.CapabilityReasoning, provider.CapabilityStreaming},
			wantAbsent: []provider.Capability{provider.CapabilityVision},
		},
		{
			model: "o3-mini",
			want:  []provider.Capability{provider.CapabilityReasoning},
		},
		{
			model: "qwq:32b",
			want:  []provider.Capability{provider.CapabilityReasoning},
		},
		{
			model:      "nomic-embed-text",
			want:       []provider.Capability{provider.CapabilityEmbeddings},
			wantAbsent: []provider.Capability{provider.CapabilityStreaming, provider.CapabilityTools},
		},
		{
			model:      "text-embedding-3-small",
			want:       []provider.Capability{provider.CapabilityEmbeddings},
			wantAbsent: []provider.Capability{provider.CapabilityTools},
		},
		{
			model:      "e5-mistral-7b-instruct",
			want:       []provider.Capability{provider.CapabilityEmbeddings},
			wantAbsent: []provider.Capability{provider.CapabilityTools},
		},
		{
			model:      "whisper-large-v3",
			want:       []provider.Capability{provider.CapabilityAudio},
			wantAbsent: []provider.Capability{provider.CapabilityTools},
		},
		{
			model:      "codellama:7b",
			want:       []provider.Capability{provider.CapabilityStreaming},
			wantAbsent: []provider.Capability{provider.CapabilityTools},
		},
		{
			// "vl" must match a whole segment, not the middle of a word.
			model:      "mistral-small-latest",
			wantAbsent: []provider.Capability{provider.CapabilityVision},
		},
		{
			// "o1"/"r1" must not be found inside longer segments.
			model:      "phi3:mini",
			wantAbsent: []provider.Capability{provider.CapabilityReasoning, provider.CapabilityVision},
		},
	}

	for _, tc := range tests {
		t.Run(tc.model, func(t *testing.T) {
			caps := heuristicCapabilities(tc.model)
			if missing := caps.Missing(tc.want...); len(missing) > 0 {
				t.Errorf("caps(%s) = %v, missing %v", tc.model, caps, missing)
			}
			for _, unwanted := range tc.wantAbsent {
				if caps.Has(unwanted) {
					t.Errorf("caps(%s) = %v, must not include %q", tc.model, caps, unwanted)
				}
			}
		})
	}
}

func TestApplyDeclaredCapabilities(t *testing.T) {
	tests := []struct {
		name string
		meta modelMeta
		want []provider.Capability
	}{
		{
			name: "declared names",
			meta: modelMeta{Declared: []string{"tool_use", "vision", "reasoning", "json_schema"}},
			want: []provider.Capability{
				provider.CapabilityTools, provider.CapabilityVision,
				provider.CapabilityReasoning, provider.CapabilityStructuredOutput,
			},
		},
		{
			name: "declared modalities",
			meta: modelMeta{Modalities: []string{"text", "image"}},
			want: []provider.Capability{provider.CapabilityVision},
		},
		{
			name: "audio modality",
			meta: modelMeta{Modalities: []string{"audio"}},
			want: []provider.Capability{provider.CapabilityAudio},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			caps := applyDeclared(provider.Capabilities{}, tc.meta)
			if missing := caps.Missing(tc.want...); len(missing) > 0 {
				t.Errorf("caps = %v, missing %v", caps, missing)
			}
		})
	}
}

func TestDeclaredCapabilitiesDecoding(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{"array", `{"id":"m","capabilities":["tools","vision"]}`, []string{"tools", "vision"}},
		{"object", `{"id":"m","capabilities":{"tools":true,"vision":false}}`, []string{"tools"}},
		{"absent", `{"id":"m"}`, nil},
		{"unknown shape", `{"id":"m","capabilities":42}`, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			models, err := decodeModels([]byte("[" + tc.raw + "]"))
			if err != nil {
				t.Fatalf("decodeModels: %v", err)
			}
			if got := models[0].declaredCapabilities(); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("declaredCapabilities() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCapabilitiesUsesServerMetadataAndCaches proves the cheap path: the model
// listing is fetched once and reused for later capability questions.
func TestCapabilitiesUsesServerMetadataAndCaches(t *testing.T) {
	var calls atomic.Int32
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"data":[{"id":"plain-model","capabilities":["vision"]}]}`))
	})

	caps, err := client.Capabilities(context.Background(), "plain-model")
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if !caps.Has(provider.CapabilityVision) {
		t.Errorf("caps = %v, want vision from the server declaration", caps)
	}
	if _, err := client.Capabilities(context.Background(), "plain-model"); err != nil {
		t.Fatalf("second Capabilities: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("model listings = %d, want 1 (result must be cached)", got)
	}
}

// TestCapabilitiesDegradesWhenListingFails covers local servers with a thin or
// missing /models endpoint: detection falls back to heuristics rather than
// failing the call.
func TestCapabilitiesDegradesWhenListingFails(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	caps, err := client.Capabilities(context.Background(), "qwen2.5-vl:7b")
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if !caps.Has(provider.CapabilityVision) {
		t.Errorf("caps = %v, want the heuristic fallback", caps)
	}
}

func TestCapabilitiesReportsCancellation(t *testing.T) {
	release := make(chan struct{})
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		close(release)
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := client.Capabilities(ctx, "some-model")
	<-release
	if cat, ok := provider.CategoryOf(err); !ok || cat != provider.ErrCancelled {
		t.Errorf("category = %v (ok=%v), want cancelled", cat, ok)
	}
}

func TestCapabilitiesRefineHookHasFinalWord(t *testing.T) {
	srv, _ := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"llama3.1:8b"}]}`))
	})
	client := New(Options{
		Name:    "vendor",
		BaseURL: srv.URL + "/v1",
		Timeout: 5 * time.Second,
		RefineCapabilities: func(model string, base provider.Capabilities) provider.Capabilities {
			// A vendor that knows tools are broken for this backend.
			out := provider.Capabilities{}
			for _, c := range base {
				if c != provider.CapabilityTools {
					out = out.Add(c)
				}
			}
			return out.Add(provider.CapabilityReasoning)
		},
	})

	caps, err := client.Capabilities(context.Background(), "llama3.1:8b")
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if caps.Has(provider.CapabilityTools) {
		t.Errorf("caps = %v, want the refine hook's removal respected", caps)
	}
	if !caps.Has(provider.CapabilityReasoning) {
		t.Errorf("caps = %v, want the refine hook's addition", caps)
	}
}

func TestCapabilitiesRejectsEmptyModel(t *testing.T) {
	client := New(Options{BaseURL: "http://127.0.0.1:1/v1"})
	if _, err := client.Capabilities(context.Background(), "  "); err == nil {
		t.Fatal("Capabilities succeeded for an empty model id")
	} else if cat, _ := provider.CategoryOf(err); cat != provider.ErrInvalidRequest {
		t.Errorf("category = %v, want invalid_request", cat)
	}
}

// TestCapabilitiesConcurrentAccess exercises the cache under -race.
func TestCapabilitiesConcurrentAccess(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"m1"},{"id":"m2"}]}`))
	})

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			model := "m1"
			if i%2 == 1 {
				model = "m2"
			}
			if _, err := client.Capabilities(context.Background(), model); err != nil {
				t.Errorf("Capabilities: %v", err)
			}
			if _, err := client.ListModels(context.Background()); err != nil {
				t.Errorf("ListModels: %v", err)
			}
		}(i)
	}
	wg.Wait()
}

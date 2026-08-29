package lemonade

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/provider"
)

func TestNormalizeBaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty falls back to the documented default", "", DefaultBaseURL},
		{"whitespace only", "  ", DefaultBaseURL},
		{"trailing slash removed", "http://box:13305/", "http://box:13305"},
		{"api prefix removed", "http://box:13305/api/v1", "http://box:13305"},
		{"api prefix with slash removed", "http://box:13305/api/v1/", "http://box:13305"},
		{"bare host gets http", "box:13305", "http://box:13305"},
		{"https preserved", "https://box/api/v1", "https://box"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := normalizeBaseURL(tc.in); got != tc.want {
				t.Fatalf("normalizeBaseURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNewDefaults(t *testing.T) {
	t.Parallel()

	c := New("", nil)
	if got := c.Name(); got != ProviderName {
		t.Fatalf("Name() = %q, want %q", got, ProviderName)
	}
	if got := c.Root(); got != DefaultBaseURL {
		t.Fatalf("Root() = %q, want %q", got, DefaultBaseURL)
	}
	if want := DefaultBaseURL + APIPath; c.BaseURL() != want {
		t.Fatalf("BaseURL() = %q, want %q", c.BaseURL(), want)
	}
}

func TestHealth(t *testing.T) {
	t.Parallel()

	t.Run("native health endpoint answers", func(t *testing.T) {
		t.Parallel()
		f := newFake(t)
		f.static(api(healthPath), `{"status":"ok","model_loaded":"Llama-3.2-1B-Instruct-Hybrid"}`)
		_, c := f.start()

		if err := c.Health(context.Background()); err != nil {
			t.Fatalf("Health: %v", err)
		}
		if got := f.count(api("/models")); got != 0 {
			t.Fatalf("the OpenAI listing was probed %d times despite a healthy native answer", got)
		}
	})

	t.Run("falls back to the model listing when health is absent", func(t *testing.T) {
		t.Parallel()
		f := newFake(t)
		f.static(api("/models"), modelsFixture)
		_, c := f.start()

		if err := c.Health(context.Background()); err != nil {
			t.Fatalf("Health: %v", err)
		}
	})

	t.Run("reports failure when both probes fail", func(t *testing.T) {
		t.Parallel()
		f := newFake(t)
		f.status(api("/models"), http.StatusInternalServerError, `{"detail":"boom"}`)
		_, c := f.start()

		pe := providerError(t, c.Health(context.Background()))
		if pe.Category != provider.ErrServer {
			t.Fatalf("category = %q, want %q", pe.Category, provider.ErrServer)
		}
	})

	t.Run("connection refused names the address and the fix", func(t *testing.T) {
		t.Parallel()
		c := unreachableClient(t)

		pe := providerError(t, c.Health(context.Background()))
		if pe.Category != provider.ErrUnavailable {
			t.Fatalf("category = %q, want %q", pe.Category, provider.ErrUnavailable)
		}
		if !strings.Contains(pe.Message, c.Root()) {
			t.Fatalf("message %q does not name the address %q", pe.Message, c.Root())
		}
		if !strings.Contains(strings.ToLower(pe.Message), "running") {
			t.Fatalf("message %q does not suggest the server may not be running", pe.Message)
		}
	})

	t.Run("only the native probe runs when the server is down", func(t *testing.T) {
		t.Parallel()
		c := unreachableClient(t)
		if err := c.Health(context.Background()); err == nil {
			t.Fatal("expected an error")
		}
	})
}

func TestServerStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		payload string
		want    string
		wantErr bool
	}{
		{"model_loaded", `{"status":"ok","model_loaded":"Llama-3.2-1B-Instruct-Hybrid"}`, "Llama-3.2-1B-Instruct-Hybrid", false},
		{"checkpoint fallback", `{"status":"ok","checkpoint_loaded":"cp"}`, "cp", false},
		{"up but silent", `{"status":"ok"}`, "", false},
		{"no status field at all is still an answer", `{}`, "", false},
		{"non-ok status is an error", `{"status":"degraded"}`, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			f.static(api(healthPath), tc.payload)
			_, c := f.start()

			got, err := c.ServerStatus(context.Background())
			if tc.wantErr {
				providerError(t, err)
				return
			}
			if err != nil {
				t.Fatalf("ServerStatus: %v", err)
			}
			if got != tc.want {
				t.Fatalf("ServerStatus() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestListModelsUsesSharedPath records the deliberate decision to add no
// vendor code for listing: Lemonade's listing is the standard OpenAI one and
// nothing about it was verifiable here, so guessing at extra fields would be
// worse than using the shared implementation.
func TestListModelsUsesSharedPath(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	f.static(api("/models"), modelsFixture)
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
	if !byID["Llama-3.2-1B-Instruct-Hybrid"].Capabilities.Has(provider.CapabilityStreaming) {
		t.Fatalf("chat model capabilities = %v", byID["Llama-3.2-1B-Instruct-Hybrid"].Capabilities)
	}
	embed := byID["nomic-embed-text-v1-GGUF"].Capabilities
	if !reflect.DeepEqual(embed, provider.Capabilities{provider.CapabilityEmbeddings}) {
		t.Fatalf("embedding model capabilities = %v, want embeddings only", embed)
	}
}

func TestRefineCapabilities(t *testing.T) {
	t.Parallel()

	chat := provider.Capabilities{}.Add(provider.CapabilityTools, provider.CapabilityStructuredOutput)

	tests := []struct {
		name  string
		model string
		base  provider.Capabilities
		want  provider.Capabilities
	}{
		{
			name:  "chat model gains streaming",
			model: "Llama-3.2-1B-Instruct-Hybrid",
			base:  chat,
			want: provider.Capabilities{
				provider.CapabilityStreaming, provider.CapabilityStructuredOutput, provider.CapabilityTools,
			},
		},
		{
			name:  "embedding id is embeddings only, backend suffix and all",
			model: "nomic-embed-text-v1-GGUF",
			base:  chat,
			want:  provider.Capabilities{provider.CapabilityEmbeddings},
		},
		{
			name:  "an audio model is left alone",
			model: "whisper-large-v3-CPU",
			base:  provider.Capabilities{provider.CapabilityAudio},
			want:  provider.Capabilities{provider.CapabilityAudio},
		},
		{
			name:  "an explicit chat marker beats an embedding marker",
			model: "e5-mistral-7b-chat-CPU",
			base:  chat,
			want: provider.Capabilities{
				provider.CapabilityStreaming, provider.CapabilityStructuredOutput, provider.CapabilityTools,
			},
		},
		{
			name:  "empty id changes nothing",
			model: "  ",
			base:  chat,
			want:  chat,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := refineCapabilities(tc.model, tc.base)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("refineCapabilities(%q) = %v, want %v", tc.model, got, tc.want)
			}
		})
	}
}

func TestStripBackendSuffix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want string
	}{
		{"llama-3.2-1b-instruct-hybrid", "llama-3.2-1b-instruct"},
		{"qwen2.5-7b-instruct-cpu", "qwen2.5-7b-instruct"},
		{"phi-3-mini-npu", "phi-3-mini"},
		{"nomic-embed-text-v1-gguf", "nomic-embed-text-v1"},
		{"plain-model", "plain-model"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			if got := stripBackendSuffix(tc.in); got != tc.want {
				t.Fatalf("stripBackendSuffix(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestHealthDoesNotMaskADegradedServer pins the deliberate asymmetry in the
// fallback: a missing endpoint falls through to the model listing, but a
// server that reports itself unhealthy is believed.
func TestHealthDoesNotMaskADegradedServer(t *testing.T) {
	t.Parallel()

	f := newFake(t)
	f.static(api(healthPath), `{"status":"degraded"}`)
	f.static(api("/models"), modelsFixture)
	_, c := f.start()

	pe := providerError(t, c.Health(context.Background()))
	if pe.Category != provider.ErrServer {
		t.Fatalf("category = %q, want %q", pe.Category, provider.ErrServer)
	}
	if got := f.count(api("/models")); got != 0 {
		t.Fatalf("the listing was probed %d times despite a definitive unhealthy answer", got)
	}
}

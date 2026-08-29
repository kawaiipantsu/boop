package anthropic

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/kawaiipantsu/boop/internal/provider"
)

func TestListModels(t *testing.T) {
	t.Parallel()

	body := `{
	  "data": [
	    {"type": "model", "id": "claude-test-opus", "display_name": "Claude Test Opus", "max_input_tokens": 200000, "max_tokens": 64000},
	    {"type": "model", "id": "claude-3-5-haiku-test", "display_name": "Claude Test Haiku"},
	    {"type": "model", "id": "   "}
	  ],
	  "has_more": false
	}`
	c, cap := newTestClient(t, jsonHandler(http.StatusOK, body))

	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %v, want the two named entries (the blank id is dropped)", models)
	}

	first := models[0]
	if first.ID != "claude-test-opus" || first.Provider != ProviderName {
		t.Errorf("models[0] = %+v", first)
	}
	if first.DisplayName != "Claude Test Opus" {
		t.Errorf("display name = %q", first.DisplayName)
	}
	if first.ContextWindow != 200000 || first.MaxOutput != 64000 {
		t.Errorf("context/output = %d/%d, want 200000/64000", first.ContextWindow, first.MaxOutput)
	}
	if !first.Capabilities.HasAll(provider.CapabilityStreaming, provider.CapabilityTools, provider.CapabilityVision) {
		t.Errorf("capabilities = %v", first.Capabilities)
	}
	// 3.5 Haiku is text only; the listing carries no capability data, so the
	// family table is what has to get this right.
	if models[1].Capabilities.Has(provider.CapabilityVision) {
		t.Errorf("models[1] capabilities = %v, want no vision for the 3.5 Haiku family", models[1].Capabilities)
	}

	if !strings.HasPrefix(cap.path, modelsPath) {
		t.Errorf("path = %q, want it to start with %q", cap.path, modelsPath)
	}
	if cap.header("X-Api-Key") != testAPIKey || cap.header("Anthropic-Version") != APIVersion {
		t.Error("the model listing must carry the same auth and version headers as a completion")
	}
}

func TestListModelsPaginates(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var paths []string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.RequestURI())
		page := len(paths)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if page == 1 {
			fmt.Fprint(w, `{"data":[{"id":"claude-page-one"}],"has_more":true,"last_id":"claude-page-one"}`)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"claude-page-two"}],"has_more":false}`)
	})

	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %v, want both pages", models)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 2 {
		t.Fatalf("requests = %v, want two pages fetched", paths)
	}
	if !strings.Contains(paths[1], "after_id=claude-page-one") {
		t.Errorf("second request = %q, want it to carry the cursor", paths[1])
	}
}

func TestListModelsError(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t, jsonHandler(http.StatusUnauthorized,
		`{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`))

	if _, err := c.ListModels(context.Background()); err == nil {
		t.Fatal("ListModels() succeeded on a 401")
	} else if cat, ok := provider.CategoryOf(err); !ok || cat != provider.ErrAuthentication {
		t.Errorf("category = %q, want %q", cat, provider.ErrAuthentication)
	}
}

func TestHealth(t *testing.T) {
	t.Parallel()

	c, cap := newTestClient(t, jsonHandler(http.StatusOK, `{"data":[],"has_more":false}`))
	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health() error = %v", err)
	}
	if !strings.Contains(cap.path, "limit=1") {
		t.Errorf("health probe path = %q, want the cheapest listing", cap.path)
	}
}

func TestCapabilities(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		model   string
		want    provider.Capabilities
		absent  []provider.Capability
		present []provider.Capability
	}{
		{
			name:    "current opus family",
			model:   "claude-opus-4-1",
			present: []provider.Capability{provider.CapabilityStreaming, provider.CapabilityTools, provider.CapabilityVision, provider.CapabilityReasoning, provider.CapabilityStructuredOutput},
			absent:  []provider.Capability{provider.CapabilityEmbeddings, provider.CapabilityAudio, provider.CapabilityResponses},
		},
		{
			name:    "3.7 generation gained thinking",
			model:   "claude-3-7-sonnet-latest",
			present: []provider.Capability{provider.CapabilityReasoning, provider.CapabilityVision},
		},
		{
			name:    "3.5 sonnet predates thinking",
			model:   "claude-3-5-sonnet-latest",
			present: []provider.Capability{provider.CapabilityVision, provider.CapabilityTools},
			absent:  []provider.Capability{provider.CapabilityReasoning},
		},
		{
			name:    "3.5 haiku is text only",
			model:   "claude-3-5-haiku-latest",
			present: []provider.Capability{provider.CapabilityTools},
			absent:  []provider.Capability{provider.CapabilityVision, provider.CapabilityReasoning},
		},
		{
			name:    "claude 3 opus has vision but no thinking",
			model:   "claude-3-opus-latest",
			present: []provider.Capability{provider.CapabilityVision, provider.CapabilityTools},
			absent:  []provider.Capability{provider.CapabilityReasoning},
		},
		{
			name:    "claude 2 predates tools and vision",
			model:   "claude-2.1",
			present: []provider.Capability{provider.CapabilityStreaming},
			absent:  []provider.Capability{provider.CapabilityTools, provider.CapabilityVision, provider.CapabilityReasoning},
		},
		{
			name:    "an unknown id gets the modern baseline",
			model:   "claude-something-new",
			present: []provider.Capability{provider.CapabilityStreaming, provider.CapabilityTools, provider.CapabilityVision, provider.CapabilityReasoning},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := New(Options{APIKey: testAPIKey})
			caps, err := c.Capabilities(context.Background(), tc.model)
			if err != nil {
				t.Fatalf("Capabilities(%q) error = %v", tc.model, err)
			}
			for _, want := range tc.present {
				if !caps.Has(want) {
					t.Errorf("Capabilities(%q) = %v, want it to include %q", tc.model, caps, want)
				}
			}
			for _, notWant := range tc.absent {
				if caps.Has(notWant) {
					t.Errorf("Capabilities(%q) = %v, want it to exclude %q", tc.model, caps, notWant)
				}
			}
		})
	}
}

func TestCapabilitiesRejectsAnEmptyModel(t *testing.T) {
	t.Parallel()

	c := New(Options{APIKey: testAPIKey})
	if _, err := c.Capabilities(context.Background(), "  "); err == nil {
		t.Fatal("Capabilities(\"\") succeeded")
	} else if cat, ok := provider.CategoryOf(err); !ok || cat != provider.ErrInvalidRequest {
		t.Errorf("category = %q, want %q", cat, provider.ErrInvalidRequest)
	}
}

func TestCapabilitiesRefinementHook(t *testing.T) {
	t.Parallel()

	c := New(Options{
		APIKey: testAPIKey,
		RefineCapabilities: func(model string, base provider.Capabilities) provider.Capabilities {
			if model == "claude-audio-test" {
				return base.Add(provider.CapabilityAudio)
			}
			return base
		},
	})
	caps, err := c.Capabilities(context.Background(), "claude-audio-test")
	if err != nil {
		t.Fatalf("Capabilities() error = %v", err)
	}
	if !caps.Has(provider.CapabilityAudio) {
		t.Errorf("caps = %v, want the refinement hook applied", caps)
	}
}

func TestCapabilitiesAreCached(t *testing.T) {
	t.Parallel()

	calls := 0
	c := New(Options{
		APIKey: testAPIKey,
		RefineCapabilities: func(_ string, base provider.Capabilities) provider.Capabilities {
			calls++
			return base
		},
	})
	for i := 0; i < 3; i++ {
		if _, err := c.Capabilities(context.Background(), "claude-cache-test"); err != nil {
			t.Fatalf("Capabilities() error = %v", err)
		}
	}
	if calls != 1 {
		t.Errorf("capability derivation ran %d times, want 1 (cached thereafter)", calls)
	}
}

func TestClientDefaults(t *testing.T) {
	t.Parallel()

	c := New(Options{})
	if c.Name() != ProviderName {
		t.Errorf("Name() = %q, want %q", c.Name(), ProviderName)
	}
	if c.BaseURL() != DefaultBaseURL {
		t.Errorf("BaseURL() = %q, want %q", c.BaseURL(), DefaultBaseURL)
	}

	named := New(Options{Name: "anthropic-proxy", BaseURL: "https://proxy.test/"})
	if named.Name() != "anthropic-proxy" {
		t.Errorf("Name() = %q", named.Name())
	}
	if named.BaseURL() != "https://proxy.test" {
		t.Errorf("BaseURL() = %q, want the trailing slash trimmed", named.BaseURL())
	}
	if strings.Contains(named.String(), "x-api-key(redacted)") {
		t.Error("String() must report no auth when no key is configured")
	}
}

func TestCustomHeadersOverrideDefaults(t *testing.T) {
	t.Parallel()

	c, cap := newTestClientWith(t, Options{
		Version: "2099-01-01",
		Headers: map[string]string{"X-Trace": "abc"},
	}, jsonHandler(http.StatusOK, okResponse))

	chatOnce(t, c, userReq("hi"))

	if got := cap.header("Anthropic-Version"); got != "2099-01-01" {
		t.Errorf("Anthropic-Version = %q, want the override", got)
	}
	if got := cap.header("X-Trace"); got != "abc" {
		t.Errorf("X-Trace = %q, want the custom header", got)
	}
}

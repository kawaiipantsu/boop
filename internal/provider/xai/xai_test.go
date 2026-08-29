package xai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kawaiipantsu/boop/internal/provider"
)

// testAPIKey is an obvious fake; no test here may need a real credential (§41).
const testAPIKey = "test-key"

type capture struct {
	mu      sync.Mutex
	headers http.Header
	path    string
}

func (c *capture) record(r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.headers = r.Header.Clone()
	c.path = r.URL.Path
}

func (c *capture) header(name string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.headers.Get(name)
}

func newTestClient(t *testing.T, opts Options, status int, body string) (*Client, *capture) {
	t.Helper()
	cap := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	opts.BaseURL = srv.URL
	if opts.APIKey == "" {
		opts.APIKey = testAPIKey
	}
	opts.HTTPClient = srv.Client()
	return New(opts), cap
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

	named := New(Options{Name: "grok-proxy", BaseURL: "https://proxy.test/v1/"})
	if named.Name() != "grok-proxy" || named.BaseURL() != "https://proxy.test/v1" {
		t.Errorf("Name()/BaseURL() = %q/%q", named.Name(), named.BaseURL())
	}
}

// TestChatUsesTheSharedOpenAIPath proves the adapter is wiring only, and that
// xAI's OpenAI-dialect responses flow through unchanged.
func TestChatUsesTheSharedOpenAIPath(t *testing.T) {
	t.Parallel()

	body := `{"id":"c","model":"grok-4","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`
	c, cap := newTestClient(t, Options{Headers: map[string]string{"X-Trace": "abc"}}, http.StatusOK, body)

	ch, err := c.Chat(context.Background(), provider.ChatRequest{
		Model:    "grok-4",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}

	var text strings.Builder
	var events []provider.ChatEvent
	for ev := range ch {
		events = append(events, ev)
		if ev.Type == provider.EventDelta {
			text.WriteString(ev.Text)
		}
		if ev.Type == provider.EventError {
			t.Fatalf("unexpected error event: %v", ev.Err)
		}
	}
	if text.String() != "hello" {
		t.Errorf("text = %q, want %q", text.String(), "hello")
	}
	if last := events[len(events)-1]; last.Type != provider.EventDone {
		t.Errorf("terminal event = %q, want done", last.Type)
	}
	if cap.path != "/chat/completions" {
		t.Errorf("path = %q", cap.path)
	}
	if got := cap.header("Authorization"); got != "Bearer "+testAPIKey {
		t.Errorf("Authorization = %q, want the bearer token xAI expects", got)
	}
	if got := cap.header("X-Trace"); got != "abc" {
		t.Errorf("X-Trace = %q, want custom headers forwarded", got)
	}
}

func TestErrorsAreNormalizedByTheSharedPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
		body   string
		want   provider.ErrorCategory
	}{
		{name: "401", status: http.StatusUnauthorized, body: `{"error":"Incorrect API key provided"}`, want: provider.ErrAuthentication},
		{name: "429", status: http.StatusTooManyRequests, body: `{"error":{"message":"rate limited"}}`, want: provider.ErrRateLimited},
		{name: "500", status: http.StatusInternalServerError, body: `{"error":"internal"}`, want: provider.ErrServer},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, _ := newTestClient(t, Options{}, tc.status, tc.body)
			_, err := c.ListModels(context.Background())
			if err == nil {
				t.Fatalf("ListModels() succeeded on a %d", tc.status)
			}
			if cat, ok := provider.CategoryOf(err); !ok || cat != tc.want {
				t.Errorf("category = %q, want %q", cat, tc.want)
			}
		})
	}
}

func TestCapabilityFamilies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		model   string
		want    []provider.Capability
		absent  []provider.Capability
		exactly int
	}{
		{
			name:  "grok-4 reasons and sees",
			model: "grok-4",
			want: []provider.Capability{provider.CapabilityStreaming, provider.CapabilityTools,
				provider.CapabilityVision, provider.CapabilityReasoning, provider.CapabilityStructuredOutput},
		},
		{
			name:  "grok-4 dated variants match the family",
			model: "grok-4-0709",
			want:  []provider.Capability{provider.CapabilityReasoning, provider.CapabilityVision},
		},
		{
			name:   "grok-3-mini reasons but does not see",
			model:  "grok-3-mini-fast",
			want:   []provider.Capability{provider.CapabilityReasoning, provider.CapabilityTools},
			absent: []provider.Capability{provider.CapabilityVision},
		},
		{
			name:   "grok-3 does not reason",
			model:  "grok-3",
			want:   []provider.Capability{provider.CapabilityTools, provider.CapabilityStructuredOutput},
			absent: []provider.Capability{provider.CapabilityReasoning, provider.CapabilityVision},
		},
		{
			name:   "grok-2-vision sees",
			model:  "grok-2-vision-1212",
			want:   []provider.Capability{provider.CapabilityVision, provider.CapabilityTools},
			absent: []provider.Capability{provider.CapabilityReasoning},
		},
		{
			name:    "grok-2-image is not a chat model",
			model:   "grok-2-image-1212",
			exactly: 0,
		},
		{
			name:   "grok-2 is text only",
			model:  "grok-2-1212",
			want:   []provider.Capability{provider.CapabilityTools},
			absent: []provider.Capability{provider.CapabilityVision},
		},
		{
			name:   "grok-code has tools",
			model:  "grok-code-fast-1",
			want:   []provider.Capability{provider.CapabilityTools, provider.CapabilityStructuredOutput},
			absent: []provider.Capability{provider.CapabilityVision},
		},
		{
			name:   "grok-vision-beta sees but has no tools",
			model:  "grok-vision-beta",
			want:   []provider.Capability{provider.CapabilityVision},
			absent: []provider.Capability{provider.CapabilityTools},
		},
		{
			name:   "grok-beta has tools",
			model:  "grok-beta",
			want:   []provider.Capability{provider.CapabilityTools},
			absent: []provider.Capability{provider.CapabilityVision},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// A deliberately wrong base set: a recognized family replaces it.
			base := provider.Capabilities{}.Add(provider.CapabilityEmbeddings, provider.CapabilityAudio)
			got := refineCapabilities(tc.model, base)

			for _, want := range tc.want {
				if !got.Has(want) {
					t.Errorf("refineCapabilities(%q) = %v, want it to include %q", tc.model, got, want)
				}
			}
			for _, notWant := range tc.absent {
				if got.Has(notWant) {
					t.Errorf("refineCapabilities(%q) = %v, want it to exclude %q", tc.model, got, notWant)
				}
			}
			if tc.exactly == 0 && len(tc.want) == 0 && len(got) != 0 {
				t.Errorf("refineCapabilities(%q) = %v, want an empty set", tc.model, got)
			}
			// xAI publishes no embedding or audio chat surface, so the wrong
			// base must never survive for a known family.
			if len(tc.want) > 0 && (got.Has(provider.CapabilityEmbeddings) || got.Has(provider.CapabilityAudio)) {
				t.Errorf("refineCapabilities(%q) = %v, want the base set replaced", tc.model, got)
			}
		})
	}
}

func TestUnknownFamilyKeepsTheGenericHeuristics(t *testing.T) {
	t.Parallel()

	base := provider.Capabilities{}.Add(provider.CapabilityStreaming, provider.CapabilityTools)
	for _, model := range []string{"grok-99-unreleased", "", "  "} {
		if got := refineCapabilities(model, base); len(got) != len(base) {
			t.Errorf("refineCapabilities(%q) = %v, want the generic heuristics %v", model, got, base)
		}
	}
}

func TestCapabilitiesFlowThroughTheProviderInterface(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t, Options{}, http.StatusOK, `{"data":[]}`)
	caps, err := c.Capabilities(context.Background(), "grok-2-image-1212")
	if err != nil {
		t.Fatalf("Capabilities() error = %v", err)
	}
	if len(caps) != 0 {
		t.Errorf("caps = %v, want the image model reported as unusable for chat", caps)
	}
}

func TestExtraRefinementHookRunsAfterTheFamilyTable(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t, Options{
		RefineCapabilities: func(model string, base provider.Capabilities) provider.Capabilities {
			if model == "grok-3" {
				return base.Add(provider.CapabilityReasoning)
			}
			return base
		},
	}, http.StatusOK, `{"data":[]}`)

	caps, err := c.Capabilities(context.Background(), "grok-3")
	if err != nil {
		t.Fatalf("Capabilities() error = %v", err)
	}
	if !caps.Has(provider.CapabilityReasoning) {
		t.Errorf("caps = %v, want the caller's hook to override the family table", caps)
	}
}

func TestListModelsCarriesCapabilities(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t, Options{}, http.StatusOK,
		`{"data":[{"id":"grok-4"},{"id":"grok-2-image-1212"}]}`)

	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %v, want 2", models)
	}
	if models[0].Provider != ProviderName {
		t.Errorf("models[0].Provider = %q, want %q", models[0].Provider, ProviderName)
	}
	if !models[0].Capabilities.Has(provider.CapabilityReasoning) {
		t.Errorf("models[0] capabilities = %v, want grok-4 to reason", models[0].Capabilities)
	}
	if len(models[1].Capabilities) != 0 {
		t.Errorf("models[1] capabilities = %v, want none for the image model", models[1].Capabilities)
	}
}

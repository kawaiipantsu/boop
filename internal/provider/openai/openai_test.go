package openai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/boop-dev/boop/internal/provider"
)

// testAPIKey is an obvious fake; no test here may need a real credential (§41).
const testAPIKey = "test-key"

// capture records what the fake server received.
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

func chatOnce(t *testing.T, c *Client) []provider.ChatEvent {
	t.Helper()
	ch, err := c.Chat(context.Background(), provider.ChatRequest{
		Model:    "gpt-test",
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	var events []provider.ChatEvent
	for ev := range ch {
		events = append(events, ev)
	}
	if len(events) == 0 {
		t.Fatal("stream produced no events")
	}
	return events
}

func terminalErr(t *testing.T, events []provider.ChatEvent) error {
	t.Helper()
	last := events[len(events)-1]
	if last.Type != provider.EventError || last.Err == nil {
		t.Fatalf("last event = %q, want a terminal error event", last.Type)
	}
	return last.Err
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
	if named := New(Options{Name: "openai-eu", BaseURL: "https://eu.example.test/v1/"}); named.Name() != "openai-eu" ||
		named.BaseURL() != "https://eu.example.test/v1" {
		t.Errorf("Name()/BaseURL() = %q/%q", named.Name(), named.BaseURL())
	}
}

func TestOrganizationAndProjectHeaders(t *testing.T) {
	t.Parallel()

	const okBody = `{"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`

	tests := []struct {
		name    string
		opts    Options
		wantOrg string
		wantPrj string
	}{
		{name: "both configured", opts: Options{Organization: "org-test", Project: "proj-test"}, wantOrg: "org-test", wantPrj: "proj-test"},
		{name: "organization only", opts: Options{Organization: "org-test"}, wantOrg: "org-test"},
		{name: "neither configured", opts: Options{}},
		{name: "blank values are dropped", opts: Options{Organization: "  ", Project: "\t"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, cap := newTestClient(t, tc.opts, http.StatusOK, okBody)
			chatOnce(t, c)

			if got := cap.header("OpenAI-Organization"); got != tc.wantOrg {
				t.Errorf("OpenAI-Organization = %q, want %q", got, tc.wantOrg)
			}
			if got := cap.header("OpenAI-Project"); got != tc.wantPrj {
				t.Errorf("OpenAI-Project = %q, want %q", got, tc.wantPrj)
			}
			if got := cap.header("Authorization"); got != "Bearer "+testAPIKey {
				t.Errorf("Authorization = %q, want the bearer token OpenAI expects", got)
			}
		})
	}
}

func TestCustomHeadersOverrideTheOrganizationHeader(t *testing.T) {
	t.Parallel()

	c, cap := newTestClient(t, Options{
		Organization: "org-default",
		Headers:      map[string]string{"OpenAI-Organization": "org-override"},
	}, http.StatusOK, `{"choices":[{"index":0,"message":{"content":"hi"},"finish_reason":"stop"}]}`)
	chatOnce(t, c)

	if got := cap.header("OpenAI-Organization"); got != "org-override" {
		t.Errorf("OpenAI-Organization = %q, want the explicit header to win", got)
	}
}

// TestQuotaExhaustionIsNotRetryable is the one place OpenAI's HTTP status is
// actively misleading: an exhausted billing quota arrives as 429, which the
// generic path treats as a retryable rate limit.
func TestQuotaExhaustionIsNotRetryable(t *testing.T) {
	t.Parallel()

	body := `{"error":{"message":"You exceeded your current quota, please check your plan and billing details.","type":"insufficient_quota","param":null,"code":"insufficient_quota"}}`
	c, _ := newTestClient(t, Options{}, http.StatusTooManyRequests, body)

	err := terminalErr(t, chatOnce(t, c))
	if cat, ok := provider.CategoryOf(err); !ok || cat != provider.ErrAuthentication {
		t.Errorf("category = %q, want %q so the router stops instead of retrying", cat, provider.ErrAuthentication)
	}
	if provider.IsRetryable(err) {
		t.Error("an exhausted quota does not recover by waiting; it must not be retryable")
	}
	if !strings.Contains(err.Error(), "quota is exhausted") {
		t.Errorf("error = %q, want an actionable message", err)
	}

	var pe *provider.Error
	if errors.As(err, &pe) && pe.Status != http.StatusTooManyRequests {
		t.Errorf("status = %d, want the original 429 preserved for debug output", pe.Status)
	}
}

func TestOrdinaryRateLimitStaysRetryable(t *testing.T) {
	t.Parallel()

	body := `{"error":{"message":"Rate limit reached for gpt-test","type":"requests","code":"rate_limit_exceeded"}}`
	c, _ := newTestClient(t, Options{}, http.StatusTooManyRequests, body)

	err := terminalErr(t, chatOnce(t, c))
	if cat, ok := provider.CategoryOf(err); !ok || cat != provider.ErrRateLimited {
		t.Errorf("category = %q, want %q", cat, provider.ErrRateLimited)
	}
	if !provider.IsRetryable(err) {
		t.Error("a genuine rate limit must stay retryable")
	}
}

func TestMaxCompletionTokensHint(t *testing.T) {
	t.Parallel()

	body := `{"error":{"message":"Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead.","type":"invalid_request_error","param":"max_tokens","code":"unsupported_parameter"}}`
	c, _ := newTestClient(t, Options{}, http.StatusBadRequest, body)

	err := terminalErr(t, chatOnce(t, c))
	if cat, ok := provider.CategoryOf(err); !ok || cat != provider.ErrInvalidRequest {
		t.Errorf("category = %q, want %q", cat, provider.ErrInvalidRequest)
	}
	if !strings.Contains(err.Error(), "leave max_tokens unset") {
		t.Errorf("error = %q, want the actionable hint appended", err)
	}
}

func TestReclassifyPassesEverythingElseThrough(t *testing.T) {
	t.Parallel()

	plain := errors.New("not a provider error")
	if got := reclassify(plain); !errors.Is(got, plain) {
		t.Errorf("reclassify(plain) = %v, want the original error", got)
	}
	if got := reclassify(nil); got != nil {
		t.Errorf("reclassify(nil) = %v, want nil", got)
	}

	untouched := &provider.Error{Category: provider.ErrServer, Message: "boom"}
	if got := reclassify(untouched); got != error(untouched) {
		t.Errorf("reclassify() rewrote an error it should have left alone: %v", got)
	}
}

func TestListModelsAndHealthReclassify(t *testing.T) {
	t.Parallel()

	body := `{"error":{"message":"You exceeded your current quota","type":"insufficient_quota","code":"insufficient_quota"}}`
	c, _ := newTestClient(t, Options{}, http.StatusTooManyRequests, body)

	if _, err := c.ListModels(context.Background()); err == nil {
		t.Fatal("ListModels() succeeded on a 429")
	} else if provider.IsRetryable(err) {
		t.Error("ListModels() error must be reclassified too")
	}
	if err := c.Health(context.Background()); err == nil {
		t.Fatal("Health() succeeded on a 429")
	} else if provider.IsRetryable(err) {
		t.Error("Health() error must be reclassified too")
	}
	if _, err := c.Embed(context.Background(), provider.EmbeddingRequest{
		Model: "text-embedding-3-small", Input: []string{"hi"},
	}); err == nil {
		t.Fatal("Embed() succeeded on a 429")
	} else if provider.IsRetryable(err) {
		t.Error("Embed() error must be reclassified too")
	}
}

func TestChatSucceedsThroughTheSharedPath(t *testing.T) {
	t.Parallel()

	body := `{"id":"c","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`
	c, cap := newTestClient(t, Options{}, http.StatusOK, body)

	events := chatOnce(t, c)
	var text strings.Builder
	var sawUsage bool
	for _, ev := range events {
		switch ev.Type {
		case provider.EventDelta:
			text.WriteString(ev.Text)
		case provider.EventUsage:
			sawUsage = true
		case provider.EventError:
			t.Fatalf("unexpected error event: %v", ev.Err)
		}
	}
	if text.String() != "hello" {
		t.Errorf("text = %q, want %q", text.String(), "hello")
	}
	if !sawUsage {
		t.Error("no usage event")
	}
	if last := events[len(events)-1]; last.Type != provider.EventDone || last.Finish != provider.FinishStop {
		t.Errorf("terminal event = %q/%q", last.Type, last.Finish)
	}
	if cap.path != "/chat/completions" {
		t.Errorf("path = %q, want the OpenAI completion endpoint", cap.path)
	}
}

func TestCapabilityFamilies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		model   string
		want    []provider.Capability
		absent  []provider.Capability
		exactly int // when > 0, the expected size of the capability set
	}{
		{
			name:  "gpt-5 reasons and sees",
			model: "gpt-5",
			want: []provider.Capability{provider.CapabilityStreaming, provider.CapabilityTools,
				provider.CapabilityVision, provider.CapabilityReasoning, provider.CapabilityStructuredOutput, provider.CapabilityResponses},
		},
		{
			name:   "gpt-4o sees but does not reason",
			model:  "gpt-4o-2024-11-20",
			want:   []provider.Capability{provider.CapabilityVision, provider.CapabilityTools, provider.CapabilityResponses},
			absent: []provider.Capability{provider.CapabilityReasoning, provider.CapabilityAudio},
		},
		{
			name:   "gpt-4o audio variants are audio models, not vision chat models",
			model:  "gpt-4o-audio-preview",
			want:   []provider.Capability{provider.CapabilityAudio},
			absent: []provider.Capability{provider.CapabilityVision},
		},
		{
			name:    "gpt-4o transcription is audio only",
			model:   "gpt-4o-transcribe",
			want:    []provider.Capability{provider.CapabilityAudio},
			exactly: 1,
		},
		{
			name:   "realtime variants are audio",
			model:  "gpt-4o-mini-realtime-preview",
			want:   []provider.Capability{provider.CapabilityAudio},
			absent: []provider.Capability{provider.CapabilityVision},
		},
		{
			name:   "o1-mini accepts neither tools nor images",
			model:  "o1-mini",
			want:   []provider.Capability{provider.CapabilityReasoning, provider.CapabilityStreaming},
			absent: []provider.Capability{provider.CapabilityTools, provider.CapabilityVision},
		},
		{
			name:   "o3-mini has tools but no vision",
			model:  "o3-mini-2025-01-31",
			want:   []provider.Capability{provider.CapabilityTools, provider.CapabilityReasoning},
			absent: []provider.Capability{provider.CapabilityVision},
		},
		{
			name:  "o4-mini sees",
			model: "o4-mini",
			want:  []provider.Capability{provider.CapabilityVision, provider.CapabilityReasoning, provider.CapabilityTools},
		},
		{
			name:    "embedding models only embed",
			model:   "text-embedding-3-large",
			want:    []provider.Capability{provider.CapabilityEmbeddings},
			exactly: 1,
		},
		{
			name:    "image generation is not a chat model",
			model:   "dall-e-3",
			exactly: 0,
		},
		{
			name:    "moderation is not a chat model",
			model:   "omni-moderation-latest",
			exactly: 0,
		},
		{
			name:   "gpt-4 turbo sees",
			model:  "gpt-4-turbo-2024-04-09",
			want:   []provider.Capability{provider.CapabilityVision, provider.CapabilityTools},
			absent: []provider.Capability{provider.CapabilityReasoning},
		},
		{
			name:   "legacy gpt-4 is text only",
			model:  "gpt-4-0613",
			want:   []provider.Capability{provider.CapabilityTools},
			absent: []provider.Capability{provider.CapabilityVision},
		},
		{
			name:   "gpt-3.5 has no strict structured output",
			model:  "gpt-3.5-turbo",
			want:   []provider.Capability{provider.CapabilityTools, provider.CapabilityStreaming},
			absent: []provider.Capability{provider.CapabilityVision, provider.CapabilityStructuredOutput},
		},
		{
			name:   "fine-tuned models inherit the base family",
			model:  "ft:gpt-4o-mini-2024-07-18:acme::abc123",
			want:   []provider.Capability{provider.CapabilityVision, provider.CapabilityTools},
			absent: []provider.Capability{provider.CapabilityReasoning},
		},
		{
			name:  "whisper is audio",
			model: "whisper-1",
			want:  []provider.Capability{provider.CapabilityAudio},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// A deliberately wrong base set: a recognized family must replace
			// it wholesale rather than merge with it.
			base := provider.Capabilities{}.Add(provider.CapabilityAudio, provider.CapabilityEmbeddings)
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
			if tc.exactly > 0 && len(got) != tc.exactly {
				t.Errorf("refineCapabilities(%q) = %v, want exactly %d capabilities", tc.model, got, tc.exactly)
			}
			if tc.exactly == 0 && len(tc.want) == 0 && len(got) != 0 {
				t.Errorf("refineCapabilities(%q) = %v, want an empty set", tc.model, got)
			}
		})
	}
}

func TestUnknownFamilyKeepsTheGenericHeuristics(t *testing.T) {
	t.Parallel()

	base := provider.Capabilities{}.Add(provider.CapabilityStreaming, provider.CapabilityTools)
	for _, model := range []string{"some-future-model", "", "   "} {
		got := refineCapabilities(model, base)
		if len(got) != len(base) {
			t.Errorf("refineCapabilities(%q) = %v, want the generic heuristics %v", model, got, base)
		}
	}
}

func TestNormalizeModelID(t *testing.T) {
	t.Parallel()

	tests := []struct{ in, want string }{
		{in: "GPT-4o", want: "gpt-4o"},
		{in: "  gpt-5  ", want: "gpt-5"},
		{in: "ft:gpt-4o-mini-2024-07-18:acme::abc", want: "gpt-4o-mini-2024-07-18"},
		{in: "ft:gpt-4o", want: "gpt-4o"},
		{in: "", want: ""},
	}
	for _, tc := range tests {
		if got := normalizeModelID(tc.in); got != tc.want {
			t.Errorf("normalizeModelID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCapabilitiesFlowThroughTheProviderInterface(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t, Options{}, http.StatusOK, `{"data":[]}`)
	caps, err := c.Capabilities(context.Background(), "o1-mini")
	if err != nil {
		t.Fatalf("Capabilities() error = %v", err)
	}
	if caps.Has(provider.CapabilityTools) {
		t.Errorf("caps = %v, want the adapter's family table to reach Capabilities()", caps)
	}
}

func TestExtraRefinementHookRunsAfterTheFamilyTable(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t, Options{
		RefineCapabilities: func(model string, base provider.Capabilities) provider.Capabilities {
			if model == "o1-mini" {
				return base.Add(provider.CapabilityTools)
			}
			return base
		},
	}, http.StatusOK, `{"data":[]}`)

	caps, err := c.Capabilities(context.Background(), "o1-mini")
	if err != nil {
		t.Fatalf("Capabilities() error = %v", err)
	}
	if !caps.Has(provider.CapabilityTools) {
		t.Errorf("caps = %v, want the caller's hook to override the family table", caps)
	}
}

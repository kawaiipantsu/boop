package openaicompat

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kawaiipantsu/boop/internal/provider"
)

func TestStatusCodeMapping(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		body         string
		wantCategory provider.ErrorCategory
		wantMessage  string
		wantRetry    bool
	}{
		{
			name: "401 authentication", status: http.StatusUnauthorized,
			body:         `{"error":{"message":"Incorrect API key provided","type":"invalid_request_error","code":"invalid_api_key"}}`,
			wantCategory: provider.ErrAuthentication, wantMessage: "Incorrect API key provided",
		},
		{
			name: "403 authentication", status: http.StatusForbidden,
			body:         `{"error":{"message":"forbidden"}}`,
			wantCategory: provider.ErrAuthentication, wantMessage: "forbidden",
		},
		{
			name: "429 rate limited", status: http.StatusTooManyRequests,
			body:         `{"error":{"message":"Rate limit reached for gpt-4o"}}`,
			wantCategory: provider.ErrRateLimited, wantMessage: "Rate limit reached for gpt-4o", wantRetry: true,
		},
		{
			name: "400 invalid request", status: http.StatusBadRequest,
			body:         `{"error":{"message":"unknown parameter foo"}}`,
			wantCategory: provider.ErrInvalidRequest, wantMessage: "unknown parameter foo",
		},
		{
			name: "422 invalid request", status: http.StatusUnprocessableEntity,
			body:         `{"detail":"input too long"}`,
			wantCategory: provider.ErrInvalidRequest, wantMessage: "input too long",
		},
		{
			name: "404 invalid request", status: http.StatusNotFound,
			body:         `{"error":{"message":"model not found"}}`,
			wantCategory: provider.ErrInvalidRequest, wantMessage: "model not found",
		},
		{
			name: "500 server error", status: http.StatusInternalServerError,
			body:         `{"error":{"message":"internal failure"}}`,
			wantCategory: provider.ErrServer, wantMessage: "internal failure", wantRetry: true,
		},
		{
			name: "503 server error", status: http.StatusServiceUnavailable,
			body:         "upstream down",
			wantCategory: provider.ErrServer, wantMessage: "upstream down", wantRetry: true,
		},
		{
			name: "504 timeout", status: http.StatusGatewayTimeout,
			body:         `{"error":{"message":"upstream timed out"}}`,
			wantCategory: provider.ErrTimeout, wantMessage: "upstream timed out", wantRetry: true,
		},
		{
			name: "html body is kept out of the message", status: http.StatusBadGateway,
			body:         "<html><body>502 Bad Gateway</body></html>",
			wantCategory: provider.ErrServer, wantMessage: "testprov returned a server error", wantRetry: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})

			err := client.Health(context.Background())
			var perr *provider.Error
			if !asProviderError(err, &perr) {
				t.Fatalf("error = %v, want a *provider.Error", err)
			}
			if perr.Category != tc.wantCategory {
				t.Errorf("category = %q, want %q", perr.Category, tc.wantCategory)
			}
			if perr.Status != tc.status {
				t.Errorf("status = %d, want %d", perr.Status, tc.status)
			}
			if perr.Message != tc.wantMessage {
				t.Errorf("message = %q, want %q", perr.Message, tc.wantMessage)
			}
			if perr.Provider != "testprov" {
				t.Errorf("provider = %q, want testprov", perr.Provider)
			}
			if perr.Detail == "" {
				t.Error("detail is empty; the raw body belongs in Detail")
			}
			if perr.Retryable() != tc.wantRetry {
				t.Errorf("Retryable() = %v, want %v", perr.Retryable(), tc.wantRetry)
			}
		})
	}
}

// TestUnsupportedCapabilityReclassification covers a live Ollama 400 that is
// semantically a missing capability, and the conservatism that keeps genuine
// bad requests out of that bucket.
func TestUnsupportedCapabilityReclassification(t *testing.T) {
	const ollamaBody = `{"error":{"message":"registry.ollama.ai/library/qwen:7b does not support tools","type":"invalid_request_error","param":null,"code":null}}`

	tests := []struct {
		name     string
		body     string
		req      provider.ChatRequest
		wantCat  provider.ErrorCategory
		wantMsg  string
		wantCode int
	}{
		{
			name: "tools rejected while tools were requested",
			body: ollamaBody,
			req: provider.ChatRequest{
				Model:    "qwen:7b",
				Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
				Tools:    []provider.ToolDefinition{{Name: "get_weather"}},
			},
			wantCat: provider.ErrUnsupportedCapability,
			wantMsg: "registry.ollama.ai/library/qwen:7b does not support tools",
		},
		{
			name:    "same message without tools stays invalid request",
			body:    ollamaBody,
			req:     simpleRequest(false),
			wantCat: provider.ErrInvalidRequest,
			wantMsg: "registry.ollama.ai/library/qwen:7b does not support tools",
		},
		{
			name: "vision rejection while an image was sent",
			body: `{"error":{"message":"this model does not support image input"}}`,
			req: provider.ChatRequest{
				Model: "llama3.1:8b",
				Messages: []provider.Message{{Role: provider.RoleUser, Parts: []provider.ContentPart{
					{Kind: provider.PartImage, MIMEType: "image/png", Data: []byte{1, 2, 3}},
				}}},
			},
			wantCat: provider.ErrUnsupportedCapability,
			wantMsg: "this model does not support image input",
		},
		{
			name: "unrelated bad request with tools present",
			body: `{"error":{"message":"temperature must be between 0 and 2"}}`,
			req: provider.ChatRequest{
				Model:    "qwen:7b",
				Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}},
				Tools:    []provider.ToolDefinition{{Name: "get_weather"}},
			},
			wantCat: provider.ErrInvalidRequest,
			wantMsg: "temperature must be between 0 and 2",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(tc.body))
			})

			events := collect(t, mustChat(t, context.Background(), client, tc.req))
			if len(events) != 1 || events[0].Type != provider.EventError {
				t.Fatalf("events = %v, want a single error", types(events))
			}
			var perr *provider.Error
			if !asProviderError(events[0].Err, &perr) {
				t.Fatalf("error = %v, want *provider.Error", events[0].Err)
			}
			if perr.Category != tc.wantCat {
				t.Errorf("category = %q, want %q", perr.Category, tc.wantCat)
			}
			if perr.Message != tc.wantMsg {
				t.Errorf("message = %q, want the server text verbatim", perr.Message)
			}
			if perr.Status != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", perr.Status)
			}
			if perr.Model != tc.req.Model {
				t.Errorf("model = %q, want %q", perr.Model, tc.req.Model)
			}
		})
	}
}

// TestUnsupportedCapabilityStreaming checks the same reclassification on the
// streaming path.
func TestUnsupportedCapabilityStreaming(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"model does not support tools"}}`))
	})
	req := simpleRequest(true)
	req.Tools = []provider.ToolDefinition{{Name: "run"}}

	events := collect(t, mustChat(t, context.Background(), client, req))
	var perr *provider.Error
	if !asProviderError(events[len(events)-1].Err, &perr) {
		t.Fatalf("no provider error: %v", events)
	}
	if perr.Category != provider.ErrUnsupportedCapability {
		t.Errorf("category = %q, want unsupported_capability", perr.Category)
	}
}

func TestConnectionRefusedIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing is listening now

	client := New(Options{Name: "lmstudio", BaseURL: url + "/v1", Timeout: 2 * time.Second})
	err := client.Health(context.Background())

	var perr *provider.Error
	if !asProviderError(err, &perr) {
		t.Fatalf("error = %v, want *provider.Error", err)
	}
	if perr.Category != provider.ErrUnavailable {
		t.Fatalf("category = %q, want unavailable (detail: %s)", perr.Category, perr.Detail)
	}
	if !perr.Retryable() {
		t.Error("an unreachable backend should be retryable")
	}
	if !strings.Contains(perr.Message, "lmstudio") {
		t.Errorf("message = %q, want it to name the provider", perr.Message)
	}
}

func TestDNSFailureIsUnavailable(t *testing.T) {
	client := New(Options{
		Name:    "remote",
		BaseURL: "http://boop-nonexistent-host.invalid/v1",
		Timeout: 3 * time.Second,
	})
	err := client.Health(context.Background())

	var perr *provider.Error
	if !asProviderError(err, &perr) {
		t.Fatalf("error = %v, want *provider.Error", err)
	}
	if perr.Category != provider.ErrUnavailable && perr.Category != provider.ErrTimeout {
		t.Errorf("category = %q, want unavailable", perr.Category)
	}
}

func TestContextCancelledIsCancelled(t *testing.T) {
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
	err := client.Health(ctx)
	<-release

	var perr *provider.Error
	if !asProviderError(err, &perr) {
		t.Fatalf("error = %v, want *provider.Error", err)
	}
	if perr.Category != provider.ErrCancelled {
		t.Errorf("category = %q, want cancelled", perr.Category)
	}
	if perr.Retryable() {
		t.Error("a cancelled request must not be retryable")
	}
}

func TestClientTimeoutIsTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)

	client := New(Options{Name: "slow", BaseURL: srv.URL + "/v1", Timeout: 100 * time.Millisecond})
	err := client.Health(context.Background())

	var perr *provider.Error
	if !asProviderError(err, &perr) {
		t.Fatalf("error = %v, want *provider.Error", err)
	}
	if perr.Category != provider.ErrTimeout {
		t.Errorf("category = %q, want timeout (detail: %s)", perr.Category, perr.Detail)
	}
}

func TestMalformedBodyIsMalformedResponse(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data": [ broken`))
	})

	_, err := client.ListModels(context.Background())
	var perr *provider.Error
	if !asProviderError(err, &perr) {
		t.Fatalf("error = %v, want *provider.Error", err)
	}
	if perr.Category != provider.ErrMalformedResponse {
		t.Errorf("category = %q, want malformed_response", perr.Category)
	}
}

// TestAPIKeyNeverLeaks is the §45 guarantee: no code path may put credential
// material into an error, a detail string or a String() rendering.
func TestAPIKeyNeverLeaks(t *testing.T) {
	const key = "sk-boop-test-000111222333444555666777"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A hostile or careless server echoing the credential back.
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprintf(w, `{"error":{"message":"Incorrect API key provided: %s. Check your key."}}`,
			strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	}))
	t.Cleanup(srv.Close)

	client := New(Options{Name: "cloud", BaseURL: srv.URL + "/v1", APIKey: key})

	if strings.Contains(client.String(), key) {
		t.Errorf("String() leaked the API key: %s", client.String())
	}

	err := client.Health(context.Background())
	var perr *provider.Error
	if !asProviderError(err, &perr) {
		t.Fatalf("error = %v, want *provider.Error", err)
	}
	surfaces := map[string]string{
		"Error()": perr.Error(),
		"Message": perr.Message,
		"Detail":  perr.Detail,
	}
	for name, s := range surfaces {
		if strings.Contains(s, key) {
			t.Errorf("%s leaked the API key: %s", name, s)
		}
		if !strings.Contains(s, redactedPlaceholder) && name != "Error()" {
			t.Errorf("%s = %q, want the key replaced by %s", name, s, redactedPlaceholder)
		}
	}
	if perr.Category != provider.ErrAuthentication {
		t.Errorf("category = %q, want authentication", perr.Category)
	}

	// The same guarantee on the chat path.
	events := collect(t, mustChat(t, context.Background(), client, simpleRequest(true)))
	for _, ev := range events {
		if ev.Err != nil && strings.Contains(ev.Err.Error(), key) {
			t.Errorf("chat error leaked the API key: %v", ev.Err)
		}
	}
}

func TestRedactCatchesUnconfiguredKeyShapes(t *testing.T) {
	client := New(Options{BaseURL: "http://127.0.0.1:1/v1"})
	tests := []struct {
		in   string
		want string
	}{
		{"token sk-abcdefghijklmnopqrstuvwx failed", "token " + redactedPlaceholder + " failed"},
		{"xai-0123456789abcdefghij rejected", redactedPlaceholder + " rejected"},
		{"nothing sensitive here", "nothing sensitive here"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := client.redact(tc.in); got != tc.want {
			t.Errorf("redact(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestExtractErrorMessage(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"openai object", `{"error":{"message":"boom","type":"x"}}`, "boom"},
		{"error string", `{"error":"boom"}`, "boom"},
		{"bare message", `{"message":"boom"}`, "boom"},
		{"fastapi detail", `{"detail":"boom"}`, "boom"},
		{"detail list", `{"detail":[{"message":"boom"}]}`, "boom"},
		{"plain text", "boom", "boom"},
		{"empty", "", ""},
		{"html", "<html>boom</html>", ""},
		{"unrecognised json", `{"status":"bad"}`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractErrorMessage([]byte(tc.body)); got != tc.want {
				t.Errorf("extractErrorMessage(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

func TestCategoryForStatus(t *testing.T) {
	tests := []struct {
		status int
		want   provider.ErrorCategory
	}{
		{401, provider.ErrAuthentication},
		{403, provider.ErrAuthentication},
		{404, provider.ErrInvalidRequest},
		{408, provider.ErrTimeout},
		{409, provider.ErrInvalidRequest},
		{422, provider.ErrInvalidRequest},
		{429, provider.ErrRateLimited},
		{500, provider.ErrServer},
		{502, provider.ErrServer},
		{503, provider.ErrServer},
		{504, provider.ErrTimeout},
	}
	for _, tc := range tests {
		if got := categoryForStatus(tc.status); got != tc.want {
			t.Errorf("categoryForStatus(%d) = %q, want %q", tc.status, got, tc.want)
		}
	}
}

func TestTruncateBoundsErrorText(t *testing.T) {
	long := strings.Repeat("x", maxErrorMessageChars*3)
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `{"error":{"message":%q}}`, long)
	})
	err := client.Health(context.Background())
	var perr *provider.Error
	if !asProviderError(err, &perr) {
		t.Fatalf("error = %v", err)
	}
	if len(perr.Message) > maxErrorMessageChars+8 {
		t.Errorf("message length = %d, want it truncated near %d", len(perr.Message), maxErrorMessageChars)
	}
}

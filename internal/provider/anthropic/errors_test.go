package anthropic

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/boop-dev/boop/internal/provider"
)

func TestStatusErrorCategories(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		status    int
		body      string
		want      provider.ErrorCategory
		retryable bool
		wantMsg   string
	}{
		{
			name:    "401 authentication",
			status:  http.StatusUnauthorized,
			body:    `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`,
			want:    provider.ErrAuthentication,
			wantMsg: "invalid x-api-key",
		},
		{
			name:   "403 permission",
			status: http.StatusForbidden,
			body:   `{"type":"error","error":{"type":"permission_error","message":"no access"}}`,
			want:   provider.ErrAuthentication,
		},
		{
			// Billing is a 403 too, and is still a credential-shaped problem
			// the user must fix rather than a request to retry.
			name:   "403 billing",
			status: http.StatusForbidden,
			body:   `{"type":"error","error":{"type":"billing_error","message":"credit balance too low"}}`,
			want:   provider.ErrAuthentication,
		},
		{
			name:   "400 invalid request",
			status: http.StatusBadRequest,
			body:   `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: field required"}}`,
			want:   provider.ErrInvalidRequest,
		},
		{
			name:   "404 unknown model is a caller mistake",
			status: http.StatusNotFound,
			body:   `{"type":"error","error":{"type":"not_found_error","message":"model not found"}}`,
			want:   provider.ErrInvalidRequest,
		},
		{
			name:   "413 request too large",
			status: http.StatusRequestEntityTooLarge,
			body:   `{"type":"error","error":{"type":"request_too_large","message":"too big"}}`,
			want:   provider.ErrInvalidRequest,
		},
		{
			name:      "429 rate limited",
			status:    http.StatusTooManyRequests,
			body:      `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`,
			want:      provider.ErrRateLimited,
			retryable: true,
		},
		{
			name:      "500 api error",
			status:    http.StatusInternalServerError,
			body:      `{"type":"error","error":{"type":"api_error","message":"internal"}}`,
			want:      provider.ErrServer,
			retryable: true,
		},
		{
			// 529 has no meaning outside this API, so the error type is what
			// classifies it.
			name:      "529 overloaded",
			status:    529,
			body:      `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			want:      provider.ErrServer,
			retryable: true,
		},
		{
			name:   "unrecognized body falls back to the status",
			status: http.StatusBadGateway,
			body:   `<html>bad gateway</html>`,
			want:   provider.ErrServer,
			// An HTML page from a proxy is noise; the default message is used.
			wantMsg:   "returned a server error",
			retryable: true,
		},
		{
			name:    "plain-text body is surfaced",
			status:  http.StatusBadRequest,
			body:    "something went wrong",
			want:    provider.ErrInvalidRequest,
			wantMsg: "something went wrong",
		},
		{
			name:    "empty body uses the default message",
			status:  http.StatusUnauthorized,
			body:    "",
			want:    provider.ErrAuthentication,
			wantMsg: "rejected the credentials",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, _ := newTestClient(t, jsonHandler(tc.status, tc.body))
			events := chatOnce(t, c, userReq("hi"))
			err := terminalError(t, events)

			var pe *provider.Error
			if !errors.As(err, &pe) {
				t.Fatalf("error %T is not a *provider.Error", err)
			}
			if pe.Category != tc.want {
				t.Errorf("category = %q, want %q", pe.Category, tc.want)
			}
			if pe.Status != tc.status {
				t.Errorf("status = %d, want %d", pe.Status, tc.status)
			}
			if pe.Provider != ProviderName {
				t.Errorf("provider = %q, want %q", pe.Provider, ProviderName)
			}
			if provider.IsRetryable(err) != tc.retryable {
				t.Errorf("IsRetryable = %v, want %v", provider.IsRetryable(err), tc.retryable)
			}
			if tc.wantMsg != "" && !strings.Contains(pe.Message, tc.wantMsg) {
				t.Errorf("message = %q, want it to contain %q", pe.Message, tc.wantMsg)
			}
			if pe.Detail == "" {
				t.Error("detail must carry the implementation specifics for debug mode")
			}
		})
	}
}

func TestUnsupportedCapabilityIsReportedAsSuch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    string
		withReq func(*provider.ChatRequest)
		want    provider.ErrorCategory
	}{
		{
			name: "refused tools when tools were sent",
			body: `{"type":"error","error":{"type":"invalid_request_error","message":"This model does not support tool use"}}`,
			withReq: func(r *provider.ChatRequest) {
				r.Tools = []provider.ToolDefinition{{Name: "t"}}
			},
			want: provider.ErrUnsupportedCapability,
		},
		{
			name: "refused images when an image was sent",
			body: `{"type":"error","error":{"type":"invalid_request_error","message":"image input is not supported by this model"}}`,
			withReq: func(r *provider.ChatRequest) {
				r.Messages = []provider.Message{{Role: provider.RoleUser, Parts: []provider.ContentPart{
					{Kind: provider.PartImage, MIMEType: "image/png", Data: []byte{1, 2, 3}},
				}}}
			},
			want: provider.ErrUnsupportedCapability,
		},
		{
			// The same wording without the feature having been requested is a
			// genuine request bug and must stay an invalid request.
			name:    "unsupported wording for a feature we never sent",
			body:    `{"type":"error","error":{"type":"invalid_request_error","message":"tool use is not supported"}}`,
			withReq: func(*provider.ChatRequest) {},
			want:    provider.ErrInvalidRequest,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, _ := newTestClient(t, jsonHandler(http.StatusBadRequest, tc.body))
			req := userReq("hi")
			tc.withReq(&req)

			events := chatOnce(t, c, req)
			err := terminalError(t, events)
			if cat, ok := provider.CategoryOf(err); !ok || cat != tc.want {
				t.Errorf("category = %q, want %q", cat, tc.want)
			}
		})
	}
}

// TestAPIKeyIsSentButNeverLeaks is the §45 guarantee: the credential goes on
// the wire in the x-api-key header and appears nowhere a user, a log or a
// transcript can see it.
func TestAPIKeyIsSentButNeverLeaks(t *testing.T) {
	t.Parallel()

	// Obvious fakes. The second is shaped like a real credential so the
	// pattern-based redaction is exercised too.
	const configuredKey = "test-key-do-not-use"
	const echoedKey = "sk-ant-api00-FAKE-KEY-FOR-TESTS-0000000000"

	body := fmt.Sprintf(`{"type":"error","error":{"type":"authentication_error","message":"invalid key %s (also saw %s)"}}`,
		configuredKey, echoedKey)

	c, cap := newTestClientWith(t, Options{APIKey: configuredKey},
		jsonHandler(http.StatusUnauthorized, body))

	events := chatOnce(t, c, userReq("hi"))
	err := terminalError(t, events)

	if got := cap.header("X-Api-Key"); got != configuredKey {
		t.Fatalf("x-api-key header = %q, want the configured key to be sent", got)
	}

	var pe *provider.Error
	if !errors.As(err, &pe) {
		t.Fatalf("error %T is not a *provider.Error", err)
	}
	surfaces := map[string]string{
		"Error()": err.Error(),
		"Message": pe.Message,
		"Detail":  pe.Detail,
		"String()": func() string {
			return c.String()
		}(),
	}
	for name, text := range surfaces {
		for _, secret := range []string{configuredKey, echoedKey} {
			if strings.Contains(text, secret) {
				t.Errorf("%s leaks a credential: %q", name, text)
			}
		}
	}
	if !strings.Contains(pe.Message, redactedPlaceholder) {
		t.Errorf("message = %q, want the redacted placeholder where the key was", pe.Message)
	}
	if !strings.Contains(c.String(), "x-api-key(redacted)") {
		t.Errorf("String() = %q, want it to report the auth mode without the key", c.String())
	}
}

func TestUnreachableEndpoint(t *testing.T) {
	t.Parallel()

	// A port that nothing is listening on: this exercises the transport error
	// path without any network egress.
	c := New(Options{APIKey: testAPIKey, BaseURL: "http://127.0.0.1:1"})

	events := chatOnce(t, c, userReq("hi"))
	err := terminalError(t, events)
	if cat, ok := provider.CategoryOf(err); !ok || cat != provider.ErrUnavailable {
		t.Errorf("category = %q, want %q", cat, provider.ErrUnavailable)
	}
	if !provider.IsRetryable(err) {
		t.Error("an unreachable endpoint is worth retrying elsewhere")
	}
	if err := c.Health(context.Background()); err == nil {
		t.Error("Health() should fail against an unreachable endpoint")
	}
}

func TestMalformedSuccessBody(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t, jsonHandler(http.StatusOK, `{"content": [`))
	events := chatOnce(t, c, userReq("hi"))
	err := terminalError(t, events)
	if cat, ok := provider.CategoryOf(err); !ok || cat != provider.ErrMalformedResponse {
		t.Errorf("category = %q, want %q", cat, provider.ErrMalformedResponse)
	}
}

func TestErrorObjectInsideA200(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t, jsonHandler(http.StatusOK,
		`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))
	events := chatOnce(t, c, userReq("hi"))
	err := terminalError(t, events)
	if cat, ok := provider.CategoryOf(err); !ok || cat != provider.ErrServer {
		t.Errorf("category = %q, want %q", cat, provider.ErrServer)
	}
	if !strings.Contains(err.Error(), "Overloaded") {
		t.Errorf("error = %q", err)
	}
}

func TestCategoryForTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		errType string
		status  int
		want    provider.ErrorCategory
	}{
		{errType: "authentication_error", status: 200, want: provider.ErrAuthentication},
		{errType: "timeout_error", status: 200, want: provider.ErrTimeout},
		{errType: "", status: http.StatusRequestTimeout, want: provider.ErrTimeout},
		{errType: "", status: http.StatusGatewayTimeout, want: provider.ErrTimeout},
		{errType: "", status: http.StatusTeapot, want: provider.ErrInvalidRequest},
		{errType: "", status: 200, want: provider.ErrServer},
		{errType: "unheard_of_error", status: http.StatusTooManyRequests, want: provider.ErrRateLimited},
	}
	for _, tc := range tests {
		if got := categoryFor(tc.errType, tc.status); got != tc.want {
			t.Errorf("categoryFor(%q, %d) = %q, want %q", tc.errType, tc.status, got, tc.want)
		}
	}
}

func TestParseErrorBody(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		body     string
		wantType string
		wantMsg  string
	}{
		{name: "empty", body: ""},
		{name: "standard envelope", body: `{"type":"error","error":{"type":"api_error","message":"boom"}}`, wantType: "api_error", wantMsg: "boom"},
		{name: "json without an error object", body: `{"ok":true}`},
		{name: "html page", body: "<html>502</html>"},
		{name: "plain text", body: "upstream connect error", wantMsg: "upstream connect error"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotType, gotMsg := parseErrorBody([]byte(tc.body))
			if gotType != tc.wantType || gotMsg != tc.wantMsg {
				t.Errorf("parseErrorBody(%q) = (%q, %q), want (%q, %q)", tc.body, gotType, gotMsg, tc.wantType, tc.wantMsg)
			}
		})
	}
}

func TestTruncateBoundsErrorText(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", maxErrorMessageChars*2)
	_, msg := parseErrorBody([]byte(long))
	if len(msg) > maxErrorMessageChars+len("…") {
		t.Errorf("message length = %d, want it bounded at %d", len(msg), maxErrorMessageChars)
	}
}

package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"syscall"

	"github.com/boop-dev/boop/internal/provider"
)

const (
	// redactedPlaceholder replaces credential material in user-visible text.
	redactedPlaceholder = "[REDACTED]"
	// maxErrorBodyBytes bounds how much of a failing body reaches Detail.
	maxErrorBodyBytes = 2048
	// maxErrorMessageChars bounds the user-facing message.
	maxErrorMessageChars = 512
)

// secretPattern matches Anthropic-shaped credentials. It is defence in depth
// on top of exact-match redaction of the configured key: a server that echoes
// a key back, or a key pasted into a model id, must not reach a log or the UI
// (§45).
var secretPattern = regexp.MustCompile(`(?i)\bsk-ant-[A-Za-z0-9_\-]{8,}`)

// redact removes credential material from a string that may reach a user.
func (c *Client) redact(s string) string {
	if s == "" {
		return s
	}
	if c.apiKey != "" {
		s = strings.ReplaceAll(s, c.apiKey, redactedPlaceholder)
	}
	return secretPattern.ReplaceAllString(s, redactedPlaceholder)
}

// newError builds a normalized, redacted provider error.
func (c *Client) newError(category provider.ErrorCategory, model, message, detail string, status int, cause error) *provider.Error {
	return &provider.Error{
		Category: category,
		Provider: c.name,
		Model:    model,
		Message:  c.redact(message),
		Detail:   c.redact(detail),
		Status:   status,
		Err:      cause,
	}
}

// requestFeatures records which optional features a request actually used, so
// a 400 that says "not supported" can be attributed to a capability rather
// than to a protocol mistake.
type requestFeatures struct {
	Tools     bool
	Images    bool
	Documents bool
}

// transportError classifies a failure that happened before or during transport.
//
// reqCtx is the context used for the request (it may carry this client's own
// deadline); parent is the caller's. The distinction matters: a deadline Boop
// imposed is a timeout, a caller cancellation is not a fault.
func (c *Client) transportError(reqCtx, parent context.Context, model string, err error) *provider.Error {
	switch {
	case parent != nil && errors.Is(parent.Err(), context.Canceled):
		return c.newError(provider.ErrCancelled, model, "request cancelled", causeDetail(err), 0, context.Canceled)
	case errors.Is(err, context.Canceled):
		return c.newError(provider.ErrCancelled, model, "request cancelled", causeDetail(err), 0, err)
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return c.newError(provider.ErrTimeout, model, "request timed out", causeDetail(err), 0, err)
	case reqCtx != nil && errors.Is(reqCtx.Err(), context.DeadlineExceeded):
		return c.newError(provider.ErrTimeout, model, "request timed out", causeDetail(err), 0, err)
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return c.newError(provider.ErrUnavailable, model,
			fmt.Sprintf("cannot resolve host for %s", c.baseURL), causeDetail(err), 0, err)
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return c.newError(provider.ErrUnavailable, model,
			fmt.Sprintf("%s is not reachable at %s", c.name, c.baseURL), causeDetail(err), 0, err)
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return c.newError(provider.ErrTimeout, model, "request timed out", causeDetail(err), 0, err)
		}
		return c.newError(provider.ErrUnavailable, model,
			fmt.Sprintf("%s is not reachable at %s", c.name, c.baseURL), causeDetail(err), 0, err)
	}
	return c.newError(provider.ErrUnavailable, model, fmt.Sprintf("%s request failed", c.name), causeDetail(err), 0, err)
}

// statusError maps an HTTP status plus an Anthropic error body onto a
// normalized provider error.
func (c *Client) statusError(status int, model string, body []byte, feats requestFeatures) *provider.Error {
	errType, serverMsg := parseErrorBody(body)
	category := categoryFor(errType, status)
	if category == provider.ErrInvalidRequest && unsupportedFeature(serverMsg, feats) {
		// §8: a refused capability must be reported as such so the UI can
		// name the feature and offer compatible models, even though
		// Anthropic classifies it as invalid_request_error.
		category = provider.ErrUnsupportedCapability
	}
	message := serverMsg
	if message == "" {
		message = defaultMessage(c.name, category, status)
	}
	detail := fmt.Sprintf("HTTP %d %s from %s%s; type=%s; body: %s",
		status, http.StatusText(status), c.baseURL, messagesPath, errType,
		truncate(strings.TrimSpace(string(body)), maxErrorBodyBytes))
	return c.newError(category, model, message, detail, status, nil)
}

// streamErrorEvent converts an SSE "error" event into a normalized error.
func (c *Client) streamErrorEvent(model string, we *wireError) *provider.Error {
	errType := ""
	message := ""
	if we != nil {
		errType = we.Type
		message = we.Message
	}
	// A mid-stream error carries no HTTP status; the error type is the only
	// classifier available.
	category := categoryFor(errType, http.StatusOK)
	if message == "" {
		message = defaultMessage(c.name, category, 0)
	}
	return c.newError(category, model, message,
		fmt.Sprintf("error event in the message stream: type=%s", errType), 0, nil)
}

// malformedError reports a response body that could not be understood.
func (c *Client) malformedError(model string, body []byte, cause error) *provider.Error {
	return c.newError(provider.ErrMalformedResponse, model,
		fmt.Sprintf("%s returned a response Boop could not parse", c.name),
		fmt.Sprintf("%s; body: %s", causeDetail(cause), truncate(strings.TrimSpace(string(body)), maxErrorBodyBytes)),
		0, cause)
}

// categoryFor maps Anthropic's error type — and, where it is absent, the HTTP
// status — onto a normalized category.
//
// The error type is preferred because it is finer grained than the status:
// billing_error and permission_error share HTTP 403 but only one of them is
// about credentials, and overloaded_error (529) has no status-code meaning at
// all outside this API.
func categoryFor(errType string, status int) provider.ErrorCategory {
	switch strings.ToLower(strings.TrimSpace(errType)) {
	case "authentication_error", "permission_error", "billing_error":
		return provider.ErrAuthentication
	case "invalid_request_error", "not_found_error", "request_too_large":
		return provider.ErrInvalidRequest
	case "rate_limit_error":
		return provider.ErrRateLimited
	case "timeout_error":
		return provider.ErrTimeout
	case "overloaded_error", "api_error":
		return provider.ErrServer
	}
	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return provider.ErrAuthentication
	case status == http.StatusTooManyRequests:
		return provider.ErrRateLimited
	case status == http.StatusRequestTimeout, status == http.StatusGatewayTimeout:
		return provider.ErrTimeout
	case status == http.StatusNotFound:
		// The server answered; it just does not have that model or route.
		return provider.ErrInvalidRequest
	case status >= 500:
		return provider.ErrServer
	case status >= 400:
		return provider.ErrInvalidRequest
	default:
		return provider.ErrServer
	}
}

func defaultMessage(name string, category provider.ErrorCategory, status int) string {
	switch category {
	case provider.ErrAuthentication:
		return fmt.Sprintf("%s rejected the credentials", name)
	case provider.ErrRateLimited:
		return fmt.Sprintf("%s rate limit reached", name)
	case provider.ErrInvalidRequest:
		return fmt.Sprintf("%s rejected the request", name)
	case provider.ErrTimeout:
		return fmt.Sprintf("%s timed out", name)
	case provider.ErrServer:
		return fmt.Sprintf("%s returned a server error", name)
	case provider.ErrUnsupportedCapability:
		return fmt.Sprintf("%s does not support a feature this request needs", name)
	default:
		if status > 0 {
			return fmt.Sprintf("%s returned HTTP %d", name, status)
		}
		return fmt.Sprintf("%s reported a failure", name)
	}
}

// parseErrorBody extracts the error type and message from an Anthropic error
// body, tolerating a plain-text or HTML body from an intermediary proxy.
func parseErrorBody(body []byte) (errType, message string) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return "", ""
	}
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err == nil {
		if env.Error != nil {
			return env.Error.Type, truncate(strings.TrimSpace(env.Error.Message), maxErrorMessageChars)
		}
		// Valid JSON without the expected envelope: the raw object belongs
		// in Detail, not in a user-facing message.
		return "", ""
	}
	if strings.HasPrefix(trimmed, "<") {
		// An HTML error page from a proxy is noise for a user.
		return "", ""
	}
	return "", truncate(trimmed, maxErrorMessageChars)
}

// unsupportedPhrases are the ways the API says "this model cannot do that".
var unsupportedPhrases = []string{
	"does not support",
	"doesn't support",
	"not supported",
	"unsupported",
	"no support for",
	"is not capable of",
}

// featureKeywords maps a requested feature onto the words used to reject it.
var featureKeywords = map[string][]string{
	"tools":     {"tool", "tool_use", "input_schema"},
	"images":    {"image", "vision", "multimodal"},
	"documents": {"document", "pdf", "file"},
}

// unsupportedFeature reports whether msg is the server refusing a feature the
// request actually used.
//
// It is deliberately conservative: an unsupported-sounding message that does
// not name a feature Boop asked for stays an invalid request, because guessing
// wrong would hide genuine request bugs behind a capability warning.
func unsupportedFeature(msg string, feats requestFeatures) bool {
	if msg == "" {
		return false
	}
	lower := strings.ToLower(msg)
	matched := false
	for _, phrase := range unsupportedPhrases {
		if strings.Contains(lower, phrase) {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}
	requested := map[string]bool{"tools": feats.Tools, "images": feats.Images, "documents": feats.Documents}
	for feature, asked := range requested {
		if !asked {
			continue
		}
		for _, kw := range featureKeywords[feature] {
			if strings.Contains(lower, kw) {
				return true
			}
		}
	}
	return false
}

func causeDetail(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

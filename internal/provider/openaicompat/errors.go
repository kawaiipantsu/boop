package openaicompat

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

	"github.com/kawaiipantsu/boop/internal/provider"
)

// maxErrorBodyBytes bounds how much of a failing response body is retained in
// Error.Detail, so a misbehaving server cannot flood logs or the UI.
const maxErrorBodyBytes = 2048

// maxErrorMessageChars bounds the user-facing message extracted from a server
// error payload.
const maxErrorMessageChars = 512

// secretPattern matches common API-key shapes (OpenAI sk-..., xAI xai-...,
// Anthropic sk-ant-...). It is defence in depth: the configured key is redacted
// by exact match, and this catches keys that leak in from a server echo.
var secretPattern = regexp.MustCompile(`(?i)\b(?:sk|xai|gsk|hf|pk|api)[-_][A-Za-z0-9_\-]{12,}`)

// redact removes credential material from a string that may reach a user, a
// log line or a transcript.
//
// Boop treats the API key as never-printable (§45), so this is applied to every
// message and detail the client produces rather than at the display layer.
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

// wrapError builds a normalized error whose detail is the underlying cause.
func (c *Client) wrapError(_ context.Context, category provider.ErrorCategory, model, message string, cause error) *provider.Error {
	detail := ""
	if cause != nil {
		detail = cause.Error()
	}
	return c.newError(category, model, message, detail, 0, cause)
}

// transportError classifies a failure that happened before or during transport.
//
// reqCtx is the context actually used for the request (it may carry the
// client's own timeout); parent is the caller's context. Distinguishing them
// matters: a deadline that Boop imposed is a timeout, while a caller
// cancellation is ErrCancelled and must not be reported as a fault.
func (c *Client) transportError(reqCtx, parent context.Context, model string, err error) *provider.Error {
	switch {
	case parent != nil && parent.Err() != nil && errIsAny(parent.Err(), context.Canceled):
		return c.newError(provider.ErrCancelled, model, "request cancelled", causeDetail(err), 0, context.Canceled)
	case errIsAny(err, context.Canceled):
		return c.newError(provider.ErrCancelled, model, "request cancelled", causeDetail(err), 0, err)
	case errIsAny(err, context.DeadlineExceeded, os.ErrDeadlineExceeded):
		return c.newError(provider.ErrTimeout, model, "request timed out", causeDetail(err), 0, err)
	case reqCtx != nil && errIsAny(reqCtx.Err(), context.DeadlineExceeded):
		return c.newError(provider.ErrTimeout, model, "request timed out", causeDetail(err), 0, err)
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return c.newError(provider.ErrUnavailable, model,
			fmt.Sprintf("cannot resolve host for %s", c.baseURL), causeDetail(err), 0, err)
	}
	if errIsAny(err, syscall.ECONNREFUSED) {
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
	return c.newError(provider.ErrUnavailable, model,
		fmt.Sprintf("%s request failed", c.name), causeDetail(err), 0, err)
}

// requestFeatures records the optional model features a request actually asked
// for. It exists so a 4xx "does not support X" answer can be reclassified as a
// capability problem instead of a malformed request — but only when X was
// genuinely requested, so real protocol mistakes keep mapping to
// ErrInvalidRequest.
type requestFeatures struct {
	Tools     bool
	Images    bool
	Documents bool
	Streaming bool
}

// statusError maps an HTTP status plus response body onto a provider error.
func (c *Client) statusError(status int, model string, body []byte) *provider.Error {
	return c.statusErrorFor(status, model, body, requestFeatures{})
}

// statusErrorFor is statusError with knowledge of what the request contained.
func (c *Client) statusErrorFor(status int, model string, body []byte, feats requestFeatures) *provider.Error {
	category := categoryForStatus(status)
	serverMsg := extractErrorMessage(body)
	if category == provider.ErrInvalidRequest && unsupportedFeature(serverMsg, feats) {
		// PROJECT.md §8: a missing capability must be reported as such so the
		// UI can name the feature and offer compatible models, even though the
		// server calls it an invalid_request_error.
		category = provider.ErrUnsupportedCapability
	}
	message := serverMsg
	if message == "" {
		message = defaultStatusMessage(c.name, category, status)
	}
	detail := fmt.Sprintf("HTTP %d %s from %s; body: %s",
		status, http.StatusText(status), c.baseURL, truncate(strings.TrimSpace(string(body)), maxErrorBodyBytes))
	return c.newError(category, model, message, detail, status, nil)
}

// malformedError reports a response body that could not be understood.
func (c *Client) malformedError(model string, body []byte, cause error) *provider.Error {
	return c.newError(provider.ErrMalformedResponse, model,
		fmt.Sprintf("%s returned a response Boop could not parse", c.name),
		fmt.Sprintf("%s; body: %s", causeDetail(cause), truncate(strings.TrimSpace(string(body)), maxErrorBodyBytes)),
		0, cause)
}

// categoryForStatus maps an HTTP status code onto a normalized category.
func categoryForStatus(status int) provider.ErrorCategory {
	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return provider.ErrAuthentication
	case status == http.StatusTooManyRequests:
		return provider.ErrRateLimited
	case status == http.StatusRequestTimeout, status == http.StatusGatewayTimeout:
		return provider.ErrTimeout
	case status == http.StatusNotFound:
		// A missing endpoint or model is a caller mistake, not an outage:
		// the server answered, it just does not have what was asked for.
		return provider.ErrInvalidRequest
	case status >= 500:
		return provider.ErrServer
	case status >= 400:
		// Covers 400 and 422 explicitly plus every other client error.
		return provider.ErrInvalidRequest
	default:
		return provider.ErrServer
	}
}

func defaultStatusMessage(name string, category provider.ErrorCategory, status int) string {
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
		return fmt.Sprintf("%s returned HTTP %d", name, status)
	}
}

// errorEnvelope covers the shapes OpenAI-compatible servers use for failures:
// the OpenAI {"error":{...}} object, a bare {"message":...}, FastAPI's
// {"detail":...} (vLLM, Lemonade) and Ollama's {"error":"..."}.
type errorEnvelope struct {
	Error   json.RawMessage `json:"error"`
	Message string          `json:"message"`
	Detail  json.RawMessage `json:"detail"`
}

type errorObject struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
	Param   string `json:"param"`
}

// extractErrorMessage pulls a human-usable message out of an error body,
// falling back to a trimmed snippet of plain-text bodies.
func extractErrorMessage(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ""
	}
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err == nil {
		if msg := messageFromRaw(env.Error); msg != "" {
			return truncate(msg, maxErrorMessageChars)
		}
		if env.Message != "" {
			return truncate(env.Message, maxErrorMessageChars)
		}
		if msg := messageFromRaw(env.Detail); msg != "" {
			return truncate(msg, maxErrorMessageChars)
		}
		// Valid JSON with no recognized message field: do not surface the
		// raw object to the user, the detail field carries it.
		return ""
	}
	if strings.HasPrefix(trimmed, "<") {
		// An HTML error page is noise for a user; keep it in Detail only.
		return ""
	}
	return truncate(trimmed, maxErrorMessageChars)
}

// messageFromRaw understands both a string and an object error field.
func messageFromRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var obj errorObject
	if err := json.Unmarshal(raw, &obj); err == nil {
		return strings.TrimSpace(obj.Message)
	}
	var list []errorObject
	if err := json.Unmarshal(raw, &list); err == nil && len(list) > 0 {
		return strings.TrimSpace(list[0].Message)
	}
	return ""
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

// unsupportedPhrases are the ways OpenAI-compatible servers say "this model
// cannot do that".
var unsupportedPhrases = []string{
	"does not support",
	"doesn't support",
	"not supported",
	"unsupported",
	"no support for",
	"is not capable of",
}

// featureKeywords maps a requested feature onto the words a server uses when it
// rejects that feature.
var featureKeywords = map[string][]string{
	"tools":     {"tool", "function call", "function_call", "function calling", "functions"},
	"images":    {"image", "vision", "multimodal", "image_url"},
	"documents": {"file", "document", "pdf", "attachment"},
	"streaming": {"stream"},
}

// unsupportedFeature reports whether msg is a server refusing a feature that
// the request actually used.
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
	requested := map[string]bool{
		"tools":     feats.Tools,
		"images":    feats.Images,
		"documents": feats.Documents,
		"streaming": feats.Streaming,
	}
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

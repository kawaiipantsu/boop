//go:build integration

package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/boop-dev/boop/internal/provider"
	"github.com/boop-dev/boop/tests/fixtures"
)

// referenceClient is a deliberately minimal OpenAI-compatible client.
//
// It exists so the integration suite can exercise the fake server across a
// real HTTP boundary before any adapter exists, and so the harness's failure
// injection can be shown to produce the normalized categories of §57. It is a
// test fixture, not a shipping adapter: internal/provider/openaicompat owns
// that job and must not import this.
type referenceClient struct {
	baseURL string
	http    *http.Client
	name    string
}

// newReferenceClient wires a client to a fake server.
func newReferenceClient(srv *fixtures.Server) *referenceClient {
	return &referenceClient{baseURL: srv.OpenAIBaseURL(), http: srv.Client(), name: "reference"}
}

// chatRequest is the wire body the reference client sends.
type chatRequest struct {
	Model    string          `json:"model"`
	Messages []wireMessage   `json:"messages"`
	Tools    []wireTool      `json:"tools,omitempty"`
	Stream   bool            `json:"stream"`
	Options  *wireStreamOpts `json:"stream_options,omitempty"`
}

type wireStreamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

// toolCallMessage builds the assistant message that carries a tool call back
// into the conversation.
func toolCallMessage(tc provider.ToolCall) wireMessage {
	var out wireToolCall
	out.ID, out.Type = tc.ID, "function"
	out.Function.Name, out.Function.Arguments = tc.Name, tc.Arguments
	return wireMessage{Role: "assistant", ToolCalls: []wireToolCall{out}}
}

// stream posts a streaming chat request and folds the SSE response into one
// summary, classifying every failure onto a provider.ErrorCategory.
func (c *referenceClient) stream(ctx context.Context, req chatRequest) (fixtures.StreamSummary, error) {
	req.Stream = true
	// Opt into streamed usage the way api.openai.com requires; local servers
	// send it regardless, so this is safe everywhere.
	req.Options = &wireStreamOpts{IncludeUsage: true}
	body, err := json.Marshal(req)
	if err != nil {
		return fixtures.StreamSummary{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", strings.NewReader(string(body)))
	if err != nil {
		return fixtures.StreamSummary{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return fixtures.StreamSummary{}, c.transportError(ctx, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		return fixtures.StreamSummary{}, c.statusError(resp.StatusCode, payload)
	}

	// Read incrementally, the way a real adapter must, rather than buffering
	// the whole body: that is what makes truncation and drops observable.
	var frames []fixtures.SSEFrame
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 32*1024), 1<<20)
	var data []string
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		if line == "" {
			if len(data) > 0 {
				frames = append(frames, fixtures.SSEFrame{Data: strings.Join(data, "\n")})
				data = nil
			}
			continue
		}
		if v, ok := strings.CutPrefix(line, "data: "); ok {
			data = append(data, v)
		}
	}
	if err := sc.Err(); err != nil {
		return fixtures.StreamSummary{}, c.transportError(ctx, err)
	}
	if len(data) > 0 {
		frames = append(frames, fixtures.SSEFrame{Data: strings.Join(data, "\n")})
	}

	sum, err := fixtures.ReassembleOpenAIStream(frames)
	if err != nil {
		return sum, provider.NewError(provider.ErrMalformedResponse, c.name,
			"the model server sent a response this client could not parse", err)
	}
	if !sum.Done {
		return sum, provider.NewError(provider.ErrMalformedResponse, c.name,
			"the model server closed the stream before it finished", nil)
	}
	return sum, nil
}

// transportError normalizes a transport-level failure.
func (c *referenceClient) transportError(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return provider.NewError(provider.ErrCancelled, c.name, "request cancelled", err)
	case errors.Is(err, context.DeadlineExceeded):
		return provider.NewError(provider.ErrTimeout, c.name, "the model server did not respond in time", err)
	case errors.Is(err, io.ErrUnexpectedEOF):
		return provider.NewError(provider.ErrMalformedResponse, c.name,
			"the connection to the model server dropped mid-response", err)
	default:
		return provider.NewError(provider.ErrUnavailable, c.name, "could not reach the model server", err)
	}
}

// statusError normalizes an HTTP status onto a provider error category.
func (c *referenceClient) statusError(status int, body []byte) error {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &envelope)
	message := envelope.Error.Message
	if message == "" {
		message = http.StatusText(status)
	}

	var category provider.ErrorCategory
	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		category = provider.ErrAuthentication
	case status == http.StatusTooManyRequests:
		category = provider.ErrRateLimited
	case status == http.StatusRequestTimeout, status == http.StatusGatewayTimeout:
		category = provider.ErrTimeout
	case status == http.StatusServiceUnavailable:
		category = provider.ErrUnavailable
	case status >= 500:
		category = provider.ErrServer
	default:
		category = provider.ErrInvalidRequest
	}
	err := provider.NewError(category, c.name, message, nil)
	err.Status = status
	err.Detail = string(body)
	return err
}

// requireCategory asserts that err is a provider error of the wanted category.
func requireCategory(t *testing.T, err error, want provider.ErrorCategory) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a %s error, got nil", want)
	}
	got, ok := provider.CategoryOf(err)
	if !ok {
		t.Fatalf("error %v is not a normalized provider error", err)
	}
	if got != want {
		t.Fatalf("category = %q, want %q (err: %v)", got, want, err)
	}
}

// getJSON fetches path from the fake server and decodes it.
func getJSON(t *testing.T, srv *fixtures.Server, path string, v any) {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL() + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("GET %s: decode: %v", path, err)
	}
}

// stringsReader adapts a literal body for the standard client helpers.
func stringsReader(s string) io.Reader { return strings.NewReader(s) }

// decodeJSON decodes a response body.
func decodeJSON(resp *http.Response, v any) error {
	return json.NewDecoder(resp.Body).Decode(v)
}

// describe renders a summary compactly for failure messages.
func describe(s fixtures.StreamSummary) string {
	return fmt.Sprintf("text=%q tools=%d finish=%q done=%v", s.Text, len(s.ToolCalls), s.Finish, s.Done)
}

//go:build integration

package integration

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/boop-dev/boop/internal/provider"
	"github.com/boop-dev/boop/tests/fixtures"
)

// runTool is the tool schema used throughout these tests.
func runTool() wireTool {
	var tool wireTool
	tool.Type = "function"
	tool.Function.Name = "run"
	tool.Function.Description = "Run a shell command"
	tool.Function.Parameters = map[string]any{
		"type":       "object",
		"properties": map[string]any{"command": map[string]any{"type": "string"}},
		"required":   []any{"command"},
	}
	return tool
}

func TestStreamingChatOverHTTP(t *testing.T) {
	srv := fixtures.NewServer(t)
	srv.Enqueue(fixtures.TextResponse("").
		WithChunks("Boop ", "is ", "working").
		WithUsage(31, 3))

	client := newReferenceClient(srv)
	sum, err := client.stream(context.Background(), chatRequest{
		Model:    "boop-test-model",
		Messages: []wireMessage{{Role: "user", Content: "are you working?"}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if sum.Text != "Boop is working" {
		t.Fatalf("summary: %s", describe(sum))
	}
	if !sum.Done || sum.Finish != "stop" {
		t.Fatalf("summary: %s", describe(sum))
	}
	if sum.Usage == nil || sum.Usage.TotalTokens != 34 {
		t.Fatalf("usage = %+v", sum.Usage)
	}

	req := srv.MustLastChatRequest(t)
	if !req.Stream || req.Model != "boop-test-model" || len(req.Messages) != 1 {
		t.Fatalf("captured request = %+v", req)
	}
}

func TestFragmentedToolCallsSurviveTheWire(t *testing.T) {
	args := `{"command":"go test ./internal/provider/..."}`
	srv := fixtures.NewServer(t)
	srv.Enqueue(fixtures.ToolCallResponse(
		fixtures.ToolCall("call_a", "run", args),
		fixtures.ToolCall("call_b", "run", `{"command":"go vet ./..."}`),
	).WithArgumentFragments(6).Interleaved())

	client := newReferenceClient(srv)
	sum, err := client.stream(context.Background(), chatRequest{
		Model:    "boop-test-model",
		Messages: []wireMessage{{Role: "user", Content: "test and vet"}},
		Tools:    []wireTool{runTool()},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(sum.ToolCalls) != 2 {
		t.Fatalf("tool calls = %+v", sum.ToolCalls)
	}
	if sum.ToolCalls[0].ID != "call_a" || sum.ToolCalls[0].Arguments != args {
		t.Errorf("first tool call = %+v", sum.ToolCalls[0])
	}
	if sum.ToolCalls[1].ID != "call_b" || sum.ToolCalls[1].Name != "run" {
		t.Errorf("second tool call = %+v", sum.ToolCalls[1])
	}
	if sum.Finish != "tool_calls" {
		t.Errorf("finish = %q", sum.Finish)
	}

	// The tool definition must have reached the server intact.
	req := srv.MustLastChatRequest(t)
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != "run" {
		t.Fatalf("tools = %+v", req.Tools)
	}
	if _, ok := req.Tools[0].Function.Parameters["required"]; !ok {
		t.Errorf("tool schema lost its required list: %+v", req.Tools[0].Function.Parameters)
	}
}

func TestUnfragmentedOllamaShapedToolCall(t *testing.T) {
	srv := fixtures.NewServer(t)
	srv.Enqueue(fixtures.ToolCallResponse(
		fixtures.ToolCall("call_rjzv4ky2", "get_weather", `{"city":"Copenhagen"}`),
	).Whole().WithSystemFingerprint("fp_ollama").WithUsage(161, 24))

	client := newReferenceClient(srv)
	sum, err := client.stream(context.Background(), chatRequest{
		Model:    "llama3.1:8b",
		Messages: []wireMessage{{Role: "user", Content: "weather?"}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(sum.ToolCalls) != 1 || sum.ToolCalls[0].Arguments != `{"city":"Copenhagen"}` {
		t.Fatalf("tool calls = %+v", sum.ToolCalls)
	}
	// The trailing usage frame carries an empty choices array; a client that
	// survives this test does not index Choices[0] blindly.
	if sum.Usage == nil || sum.Usage.PromptTokens != 161 || sum.Usage.CompletionTokens != 24 {
		t.Fatalf("usage = %+v", sum.Usage)
	}
}

func TestMultiTurnToolExchange(t *testing.T) {
	srv := fixtures.NewServer(t)
	srv.Enqueue(
		fixtures.ToolCallResponse(fixtures.ToolCall("call_1", "run", `{"command":"go build ./..."}`)),
		fixtures.TextResponse("The build succeeded.").WithUsage(50, 5),
	)

	client := newReferenceClient(srv)
	ctx := context.Background()
	messages := []wireMessage{{Role: "user", Content: "build the project"}}

	first, err := client.stream(ctx, chatRequest{Model: "boop-test-model", Messages: messages, Tools: []wireTool{runTool()}})
	if err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if len(first.ToolCalls) != 1 {
		t.Fatalf("first turn: %s", describe(first))
	}

	// Feed the tool result back, exactly as the session layer will.
	tc := first.ToolCalls[0]
	messages = append(messages, toolCallMessage(tc), wireMessage{
		Role:       "tool",
		ToolCallID: tc.ID,
		Content:    `{"exit_code":0,"stdout":"","stderr":""}`,
	})

	second, err := client.stream(ctx, chatRequest{Model: "boop-test-model", Messages: messages, Tools: []wireTool{runTool()}})
	if err != nil {
		t.Fatalf("second turn: %v", err)
	}
	if second.Text != "The build succeeded." {
		t.Fatalf("second turn: %s", describe(second))
	}

	requests := srv.ChatRequests()
	if len(requests) != 2 {
		t.Fatalf("captured %d requests, want 2", len(requests))
	}
	last := requests[1]
	if len(last.Messages) != 3 {
		t.Fatalf("second request messages = %d, want 3", len(last.Messages))
	}
	if last.Messages[1].Role != "assistant" || len(last.Messages[1].ToolCalls) != 1 {
		t.Errorf("assistant tool call message not sent back: %+v", last.Messages[1])
	}
	if last.Messages[2].Role != "tool" || last.Messages[2].ToolCallID != "call_1" {
		t.Errorf("tool result message = %+v", last.Messages[2])
	}
	if srv.QueueLen() != 0 {
		t.Errorf("script not fully consumed: %d left", srv.QueueLen())
	}
}

func TestFailureInjectionMapsToNormalizedCategories(t *testing.T) {
	cases := []struct {
		name     string
		response *fixtures.Response
		want     provider.ErrorCategory
	}{
		{"unauthorized", fixtures.ErrorResponse(http.StatusUnauthorized, "bad key"), provider.ErrAuthentication},
		{"rate limited", fixtures.ErrorResponse(http.StatusTooManyRequests, "slow down"), provider.ErrRateLimited},
		{"server error", fixtures.ErrorResponse(http.StatusInternalServerError, "boom"), provider.ErrServer},
		{"unavailable", fixtures.ErrorResponse(http.StatusServiceUnavailable, "loading"), provider.ErrUnavailable},
		{"bad request", fixtures.ErrorResponse(http.StatusBadRequest, "no such model"), provider.ErrInvalidRequest},
		{"malformed frame", fixtures.MalformedResponse("data: {not json\n\n"), provider.ErrMalformedResponse},
		{"truncated stream", fixtures.TextResponse("").WithChunks("a", "b", "c").TruncateAfterFrames(2), provider.ErrMalformedResponse},
		{"dropped connection", fixtures.TextResponse("").WithChunks("a", "b", "c").DropAfterFrames(2), provider.ErrMalformedResponse},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := fixtures.NewServer(t)
			srv.Enqueue(tc.response)
			client := newReferenceClient(srv)

			_, err := client.stream(context.Background(), chatRequest{
				Model:    "boop-test-model",
				Messages: []wireMessage{{Role: "user", Content: "hi"}},
			})
			requireCategory(t, err, tc.want)

			var pe *provider.Error
			if errors.As(err, &pe) && pe.Message == "" {
				t.Error("a normalized error must carry a user-facing message")
			}
		})
	}
}

func TestTimeoutIsClassifiedAsTimeout(t *testing.T) {
	srv := fixtures.NewServer(t)
	srv.Enqueue(fixtures.TextResponse("too late").WithDelay(400 * time.Millisecond))
	client := newReferenceClient(srv)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	_, err := client.stream(ctx, chatRequest{
		Model:    "boop-test-model",
		Messages: []wireMessage{{Role: "user", Content: "hi"}},
	})
	requireCategory(t, err, provider.ErrTimeout)
	if !provider.IsRetryable(err) {
		t.Error("a timeout should be retryable")
	}
}

func TestRetryAfterRateLimitSucceeds(t *testing.T) {
	srv := fixtures.NewServer(t)
	srv.Enqueue(
		fixtures.ErrorResponse(http.StatusTooManyRequests, "slow down").WithHeader("Retry-After", "0"),
		fixtures.TextResponse("second attempt worked"),
	)
	client := newReferenceClient(srv)
	req := chatRequest{Model: "boop-test-model", Messages: []wireMessage{{Role: "user", Content: "hi"}}}

	var (
		sum      fixtures.StreamSummary
		err      error
		attempts int
	)
	for attempts = 1; attempts <= 3; attempts++ {
		sum, err = client.stream(context.Background(), req)
		if err == nil || !provider.IsRetryable(err) {
			break
		}
	}
	if err != nil {
		t.Fatalf("after %d attempts: %v", attempts, err)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2", attempts)
	}
	if sum.Text != "second attempt worked" {
		t.Errorf("summary: %s", describe(sum))
	}
}

func TestModelDiscoveryIsConsistentAcrossVendorEndpoints(t *testing.T) {
	srv := fixtures.NewServer(t)

	var openai struct {
		Data []struct {
			ID            string   `json:"id"`
			ContextLength int      `json:"context_length"`
			Capabilities  []string `json:"capabilities"`
		} `json:"data"`
	}
	getJSON(t, srv, "/v1/models", &openai)

	var ollama struct {
		Models []struct {
			Model   string `json:"model"`
			Details struct {
				ContextLength int `json:"context_length"`
			} `json:"details"`
			Capabilities []string `json:"capabilities"`
		} `json:"models"`
	}
	getJSON(t, srv, "/api/tags", &ollama)

	var lmstudio struct {
		Data []struct {
			ID               string `json:"id"`
			MaxContextLength int    `json:"max_context_length"`
		} `json:"data"`
	}
	getJSON(t, srv, "/api/v0/models", &lmstudio)

	if len(openai.Data) != len(ollama.Models) || len(openai.Data) != len(lmstudio.Data) {
		t.Fatalf("catalogue sizes differ: %d/%d/%d",
			len(openai.Data), len(ollama.Models), len(lmstudio.Data))
	}
	for i := range openai.Data {
		if openai.Data[i].ID != ollama.Models[i].Model || openai.Data[i].ID != lmstudio.Data[i].ID {
			t.Errorf("model %d ids differ: %q/%q/%q", i,
				openai.Data[i].ID, ollama.Models[i].Model, lmstudio.Data[i].ID)
		}
		if openai.Data[i].ContextLength != ollama.Models[i].Details.ContextLength ||
			openai.Data[i].ContextLength != lmstudio.Data[i].MaxContextLength {
			t.Errorf("model %d context windows differ", i)
		}
	}

	// Capability discovery through Ollama's /api/show must agree with /api/tags.
	var show struct {
		Capabilities []string `json:"capabilities"`
	}
	resp, err := srv.Client().Post(srv.URL()+"/api/show", "application/json",
		stringsReader(`{"model":"`+openai.Data[0].ID+`"}`))
	if err != nil {
		t.Fatalf("POST /api/show: %v", err)
	}
	defer resp.Body.Close()
	if err := decodeJSON(resp, &show); err != nil {
		t.Fatalf("decode /api/show: %v", err)
	}
	if len(show.Capabilities) != len(ollama.Models[0].Capabilities) {
		t.Errorf("/api/show capabilities %v differ from /api/tags %v",
			show.Capabilities, ollama.Models[0].Capabilities)
	}
}

func TestAnthropicSurfaceOverHTTP(t *testing.T) {
	srv := fixtures.NewServer(t)
	srv.Enqueue(fixtures.TextResponse("").
		WithChunks("All ", "good").
		WithToolCalls(fixtures.ToolCall("toolu_1", "run", `{"command":"ls -la"}`)).
		WithArgumentFragments(3).
		WithUsage(15, 7))

	resp, err := srv.Client().Post(srv.URL()+"/v1/messages", "application/json",
		stringsReader(`{"model":"claude-test","max_tokens":256,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("POST /v1/messages: %v", err)
	}
	frames, err := fixtures.ReadSSE(resp)
	if err != nil {
		t.Fatalf("read sse: %v", err)
	}
	sum, err := fixtures.ReassembleAnthropicStream(frames)
	if err != nil {
		t.Fatalf("reassemble: %v", err)
	}
	if sum.Text != "All good" || !sum.Done {
		t.Fatalf("summary: %s", describe(sum))
	}
	if len(sum.ToolCalls) != 1 || sum.ToolCalls[0].Arguments != `{"command":"ls -la"}` {
		t.Fatalf("tool calls = %+v", sum.ToolCalls)
	}
	if sum.Finish != "tool_use" {
		t.Errorf("stop reason = %q, want Anthropic's tool_use", sum.Finish)
	}

	reqs := srv.AnthropicRequests()
	if len(reqs) != 1 || reqs[0].MaxTokens != 256 || !reqs[0].Stream {
		t.Fatalf("captured request = %+v", reqs)
	}
}

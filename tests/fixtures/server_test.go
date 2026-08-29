package fixtures_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/boop-dev/boop/internal/provider"
	"github.com/boop-dev/boop/tests/fixtures"
)

// post is a small helper: send body to path and return the response.
func post(t *testing.T, srv *fixtures.Server, path, body string, headers ...string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL()+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

// get fetches path and returns the response.
func get(t *testing.T, srv *fixtures.Server, path string) *http.Response {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL() + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

// decode reads and JSON-decodes a response body.
func decode(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
}

func TestModelsEndpoint(t *testing.T) {
	srv := fixtures.NewServer(t)

	for _, path := range []string{"/v1/models", "/api/v1/models"} {
		var out struct {
			Object string `json:"object"`
			Data   []struct {
				ID            string   `json:"id"`
				Object        string   `json:"object"`
				OwnedBy       string   `json:"owned_by"`
				ContextLength int      `json:"context_length"`
				Capabilities  []string `json:"capabilities"`
			} `json:"data"`
		}
		decode(t, get(t, srv, path), &out)
		if out.Object != "list" {
			t.Fatalf("%s: object = %q, want list", path, out.Object)
		}
		if len(out.Data) != 2 {
			t.Fatalf("%s: got %d models, want 2", path, len(out.Data))
		}
		first := out.Data[0]
		if first.ID != "boop-test-model" || first.Object != "model" {
			t.Errorf("%s: first model = %+v", path, first)
		}
		if first.ContextLength != 8192 {
			t.Errorf("%s: context_length = %d, want 8192", path, first.ContextLength)
		}
		if strings.Join(first.Capabilities, ",") != "completion,tools" {
			t.Errorf("%s: capabilities = %v", path, first.Capabilities)
		}
	}
}

func TestModelsEndpointHonoursCustomCatalogue(t *testing.T) {
	srv := fixtures.NewServer(t, fixtures.WithModels(fixtures.TextOnlyModel("tiny")))
	var out struct {
		Data []struct {
			ID           string   `json:"id"`
			Capabilities []string `json:"capabilities"`
		} `json:"data"`
	}
	decode(t, get(t, srv, "/v1/models"), &out)
	if len(out.Data) != 1 || out.Data[0].ID != "tiny" {
		t.Fatalf("data = %+v", out.Data)
	}
	if len(out.Data[0].Capabilities) != 1 || out.Data[0].Capabilities[0] != "completion" {
		t.Errorf("capabilities = %v, want [completion]", out.Data[0].Capabilities)
	}
}

func TestNonStreamingChatCompletion(t *testing.T) {
	srv := fixtures.NewServer(t)
	srv.Enqueue(fixtures.TextResponse("hello world").WithUsage(11, 3).WithCachedTokens(4))

	resp := post(t, srv, "/v1/chat/completions",
		`{"model":"boop-test-model","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			TotalTokens         int `json:"total_tokens"`
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	decode(t, resp, &out)

	if out.Object != "chat.completion" || out.Model != "boop-test-model" {
		t.Fatalf("envelope = %+v", out)
	}
	if len(out.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(out.Choices))
	}
	if got := out.Choices[0].Message.Content; got != "hello world" {
		t.Errorf("content = %q", got)
	}
	if out.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q", out.Choices[0].FinishReason)
	}
	if out.Usage.TotalTokens != 14 || out.Usage.PromptTokensDetails.CachedTokens != 4 {
		t.Errorf("usage = %+v", out.Usage)
	}
	if srv.QueueLen() != 0 {
		t.Errorf("queue not drained: %d left", srv.QueueLen())
	}
}

func TestNonStreamingToolCalls(t *testing.T) {
	srv := fixtures.NewServer(t)
	srv.Enqueue(fixtures.ToolCallResponse(
		fixtures.ToolCall("call_1", "run", `{"command":"go test ./..."}`),
	))

	var out struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	decode(t, post(t, srv, "/v1/chat/completions", `{"model":"m","messages":[]}`), &out)

	if len(out.Choices) != 1 || len(out.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("out = %+v", out)
	}
	tc := out.Choices[0].Message.ToolCalls[0]
	if tc.ID != "call_1" || tc.Type != "function" || tc.Function.Name != "run" {
		t.Errorf("tool call = %+v", tc)
	}
	if tc.Function.Arguments != `{"command":"go test ./..."}` {
		t.Errorf("arguments = %q", tc.Function.Arguments)
	}
	if out.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", out.Choices[0].FinishReason)
	}
}

func TestRequestCapture(t *testing.T) {
	srv := fixtures.NewServer(t)
	srv.Enqueue(fixtures.TextResponse("ok"), fixtures.TextResponse("ok"))

	body := `{"model":"boop-test-model","stream":false,"temperature":0.25,"max_tokens":512,
	 "messages":[{"role":"system","content":"be terse"},
	             {"role":"user","content":[{"type":"text","text":"hello "},{"type":"text","text":"there"}]},
	             {"role":"tool","tool_call_id":"call_1","content":"exit 0"}],
	 "tools":[{"type":"function","function":{"name":"run","description":"run a command",
	           "parameters":{"type":"object","properties":{"command":{"type":"string"}}}}}]}`
	post(t, srv, "/v1/chat/completions", body, "Authorization", "Bearer dummy-token").Body.Close()

	req := srv.MustLastChatRequest(t)
	if req.Model != "boop-test-model" {
		t.Errorf("model = %q", req.Model)
	}
	if req.Stream {
		t.Error("stream should be false")
	}
	if req.Temperature == nil || *req.Temperature != 0.25 {
		t.Errorf("temperature = %v", req.Temperature)
	}
	if req.MaxTokens != 512 {
		t.Errorf("max_tokens = %d", req.MaxTokens)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(req.Messages))
	}
	if got := req.Messages[0].Text(); got != "be terse" {
		t.Errorf("system text = %q", got)
	}
	if got := req.Messages[1].Text(); got != "hello there" {
		t.Errorf("multimodal text flattening = %q", got)
	}
	if req.Messages[2].ToolCallID != "call_1" {
		t.Errorf("tool_call_id = %q", req.Messages[2].ToolCallID)
	}
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != "run" {
		t.Fatalf("tools = %+v", req.Tools)
	}
	if _, ok := req.Tools[0].Function.Parameters["properties"]; !ok {
		t.Errorf("tool schema not captured: %+v", req.Tools[0].Function.Parameters)
	}

	last, ok := srv.LastRequest()
	if !ok {
		t.Fatal("no captured request")
	}
	if last.Path != "/v1/chat/completions" || last.Method != http.MethodPost {
		t.Errorf("captured = %s", last)
	}
	if last.BearerToken() != "dummy-token" {
		t.Errorf("bearer = %q", last.BearerToken())
	}
	if !bytes.Contains(last.Body, []byte("be terse")) {
		t.Error("raw body not captured")
	}
	if n := srv.RequestCount(); n != 1 {
		t.Errorf("request count = %d, want 1", n)
	}
	if srv.QueueLen() != 1 {
		t.Errorf("queue = %d, want 1 unconsumed", srv.QueueLen())
	}
}

func TestQueueOrderAndDefaultResponse(t *testing.T) {
	srv := fixtures.NewServer(t)
	srv.EnqueueText("first").EnqueueText("second")

	for _, want := range []string{"first", "second", "ok"} {
		var out struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		decode(t, post(t, srv, "/v1/chat/completions", `{"model":"m","messages":[]}`), &out)
		if got := out.Choices[0].Message.Content; got != want {
			t.Fatalf("content = %q, want %q (queue fell through to default?)", got, want)
		}
	}
}

func TestResetClearsQueueAndCaptures(t *testing.T) {
	srv := fixtures.NewServer(t)
	srv.EnqueueText("a").EnqueueText("b")
	get(t, srv, "/v1/models").Body.Close()
	srv.Reset()

	if srv.QueueLen() != 0 || srv.RequestCount() != 0 {
		t.Fatalf("after Reset: queue=%d requests=%d", srv.QueueLen(), srv.RequestCount())
	}
	if _, ok := srv.LastRequest(); ok {
		t.Error("LastRequest should report no requests after Reset")
	}
}

func TestCustomHandlerOverridesBuiltin(t *testing.T) {
	srv := fixtures.NewServer(t)
	srv.SetHandler("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		io.WriteString(w, `{"object":"list","data":[]}`)
	})
	resp := get(t, srv, "/v1/models")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("status = %d, want 418", resp.StatusCode)
	}
	if len(srv.RequestsTo("/v1/models")) != 1 {
		t.Error("custom handler bypassed request capture")
	}
}

func TestResponseCloneIsIndependent(t *testing.T) {
	base := fixtures.TextResponse("a").WithToolCalls(fixtures.ToolCall("1", "run", "{}"))
	clone := base.Clone().WithText("b").WithToolCalls(fixtures.ToolCall("2", "run", "{}"))
	if base.Text != "a" || len(base.ToolCalls) != 1 {
		t.Fatalf("clone mutated the original: %+v", base)
	}
	if clone.Text != "b" || len(clone.ToolCalls) != 2 {
		t.Fatalf("clone = %+v", clone)
	}
}

func TestProviderUsageRoundTrip(t *testing.T) {
	// The harness speaks provider types directly, so a test can assert against
	// the same structs the adapter will produce.
	want := provider.Usage{PromptTokens: 7, CompletionTokens: 2, TotalTokens: 9}
	r := fixtures.TextResponse("x").WithUsage(7, 2)
	if *r.Usage != want {
		t.Fatalf("usage = %+v, want %+v", *r.Usage, want)
	}
}

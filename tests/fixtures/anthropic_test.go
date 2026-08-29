package fixtures_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/boop-dev/boop/tests/fixtures"
)

const anthropicReq = `{"model":"claude-test","max_tokens":1024,"system":"be terse",
 "messages":[{"role":"user","content":"hi"}],
 "tools":[{"name":"run","description":"run a command","input_schema":{"type":"object"}}]}`

func TestAnthropicNonStreaming(t *testing.T) {
	srv := fixtures.NewServer(t)
	srv.Enqueue(fixtures.TextResponse("hello").
		WithReasoning("pondering").
		WithToolCalls(fixtures.ToolCall("toolu_1", "run", `{"command":"ls"}`)).
		WithUsage(12, 4))

	var out struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Role    string `json:"role"`
		Model   string `json:"model"`
		Content []struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			ID       string          `json:"id"`
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	decode(t, post(t, srv, "/v1/messages", anthropicReq), &out)

	if out.Type != "message" || out.Role != "assistant" || !strings.HasPrefix(out.ID, "msg_") {
		t.Fatalf("envelope = %+v", out)
	}
	if out.Model != "claude-test" {
		t.Errorf("model = %q", out.Model)
	}
	if len(out.Content) != 3 {
		t.Fatalf("content blocks = %+v", out.Content)
	}
	if out.Content[0].Type != "thinking" || out.Content[0].Thinking != "pondering" {
		t.Errorf("thinking block = %+v", out.Content[0])
	}
	if out.Content[1].Type != "text" || out.Content[1].Text != "hello" {
		t.Errorf("text block = %+v", out.Content[1])
	}
	tu := out.Content[2]
	if tu.Type != "tool_use" || tu.ID != "toolu_1" || tu.Name != "run" {
		t.Errorf("tool_use block = %+v", tu)
	}
	if string(tu.Input) != `{"command":"ls"}` {
		t.Errorf("tool_use input = %s (must be a JSON object, not a string)", tu.Input)
	}
	// tool_use present means the stop reason is Anthropic's, not OpenAI's.
	if out.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", out.StopReason)
	}
	if out.Usage.InputTokens != 12 || out.Usage.OutputTokens != 4 {
		t.Errorf("usage = %+v", out.Usage)
	}

	reqs := srv.AnthropicRequests()
	if len(reqs) != 1 {
		t.Fatalf("captured %d anthropic requests", len(reqs))
	}
	if reqs[0].MaxTokens != 1024 || len(reqs[0].Tools) != 1 || reqs[0].Tools[0].Name != "run" {
		t.Errorf("captured request = %+v", reqs[0])
	}
	if reqs[0].Tools[0].InputSchema["type"] != "object" {
		t.Errorf("input_schema = %+v", reqs[0].Tools[0].InputSchema)
	}
}

func TestAnthropicStreamingFraming(t *testing.T) {
	srv := fixtures.NewServer(t)
	srv.Enqueue(fixtures.TextResponse("").
		WithChunks("Hel", "lo").
		WithToolCalls(fixtures.ToolCall("toolu_9", "run", `{"command":"go test ./..."}`)).
		WithArgumentFragments(4).
		WithUsage(20, 6))

	resp := post(t, srv, "/v1/messages", `{"model":"claude-test","max_tokens":64,"stream":true,"messages":[]}`)
	frames, err := fixtures.ReadSSE(resp)
	if err != nil {
		t.Fatalf("read sse: %v", err)
	}

	// Anthropic frames are named events and there is no [DONE] sentinel.
	var names []string
	for _, f := range frames {
		if f.Event == "" {
			t.Fatalf("anthropic frames must carry an event name: %+v", f)
		}
		if f.Data == "[DONE]" {
			t.Fatal("anthropic streams must not emit [DONE]")
		}
		names = append(names, f.Event)
	}
	if names[0] != "message_start" || names[len(names)-1] != "message_stop" {
		t.Fatalf("event sequence = %v", names)
	}
	want := "message_start,content_block_start,content_block_delta,content_block_delta,content_block_stop," +
		"content_block_start,content_block_delta,content_block_delta,content_block_delta,content_block_delta," +
		"content_block_stop,message_delta,message_stop"
	if got := strings.Join(names, ","); got != want {
		t.Errorf("event sequence =\n%s\nwant\n%s", got, want)
	}

	sum, err := fixtures.ReassembleAnthropicStream(frames)
	if err != nil {
		t.Fatalf("reassemble: %v", err)
	}
	if sum.Text != "Hello" {
		t.Errorf("text = %q", sum.Text)
	}
	if !sum.Done || sum.Finish != "tool_use" {
		t.Errorf("done=%v finish=%q", sum.Done, sum.Finish)
	}
	if len(sum.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", sum.ToolCalls)
	}
	tc := sum.ToolCalls[0]
	if tc.ID != "toolu_9" || tc.Name != "run" || tc.Arguments != `{"command":"go test ./..."}` {
		t.Errorf("tool call = %+v", tc)
	}
	if sum.Usage == nil || sum.Usage.PromptTokens != 20 || sum.Usage.CompletionTokens != 6 {
		t.Errorf("usage = %+v", sum.Usage)
	}

	// Arguments must really have been split across input_json_delta events.
	for _, f := range frames {
		if strings.Contains(f.Data, `go test ./...`) {
			t.Fatalf("arguments arrived whole in one frame: %s", f.Data)
		}
	}
}

func TestAnthropicSharesTheResponseQueue(t *testing.T) {
	srv := fixtures.NewServer(t)
	srv.Enqueue(fixtures.TextResponse("openai turn"), fixtures.TextResponse("anthropic turn"))

	var openai struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	decode(t, post(t, srv, "/v1/chat/completions", `{"model":"m","messages":[]}`), &openai)
	if openai.Choices[0].Message.Content != "openai turn" {
		t.Fatalf("openai content = %q", openai.Choices[0].Message.Content)
	}

	var anth struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	decode(t, post(t, srv, "/v1/messages", anthropicReq), &anth)
	if anth.Content[0].Text != "anthropic turn" {
		t.Fatalf("anthropic content = %q", anth.Content[0].Text)
	}
}

func TestAnthropicFailureInjection(t *testing.T) {
	srv := fixtures.NewServer(t)
	srv.Enqueue(fixtures.ErrorResponse(429, "rate limited"))

	resp := post(t, srv, "/v1/messages", anthropicReq)
	if resp.StatusCode != 429 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	decode(t, resp, &out)
	if out.Type != "error" || out.Error.Type != "rate_limit_error" || out.Error.Message != "rate limited" {
		t.Errorf("error envelope = %+v", out)
	}
}

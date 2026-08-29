package fixtures_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/boop-dev/boop/tests/fixtures"
)

// streamChat posts a streaming request and returns the parsed SSE frames.
func streamChat(t *testing.T, srv *fixtures.Server, body string) []fixtures.SSEFrame {
	t.Helper()
	resp := post(t, srv, "/v1/chat/completions", body)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	frames, err := fixtures.ReadSSE(resp)
	if err != nil {
		t.Fatalf("read sse: %v", err)
	}
	return frames
}

const streamReq = `{"model":"boop-test-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`

func TestStreamTextFrames(t *testing.T) {
	srv := fixtures.NewServer(t)
	srv.Enqueue(fixtures.TextResponse("").
		WithChunks("Hel", "lo, ", "world").
		WithReasoning("thinking", "think", "ing").
		WithUsage(5, 3))

	frames := streamChat(t, srv, streamReq)
	if len(frames) < 7 {
		t.Fatalf("got %d frames, want at least 7: %+v", len(frames), frames)
	}
	if got := frames[len(frames)-1].Data; got != "[DONE]" {
		t.Fatalf("last frame = %q, want [DONE]", got)
	}
	// Every payload frame must be valid JSON on its own: a client parses one
	// frame at a time.
	for i, f := range frames[:len(frames)-1] {
		if !json.Valid([]byte(f.Data)) {
			t.Fatalf("frame %d is not valid JSON: %q", i, f.Data)
		}
	}

	sum, err := fixtures.ReassembleOpenAIStream(frames)
	if err != nil {
		t.Fatalf("reassemble: %v", err)
	}
	if sum.Text != "Hello, world" {
		t.Errorf("text = %q", sum.Text)
	}
	if sum.Reasoning != "thinking" {
		t.Errorf("reasoning = %q", sum.Reasoning)
	}
	if sum.Finish != "stop" {
		t.Errorf("finish = %q", sum.Finish)
	}
	if !sum.Done {
		t.Error("stream did not terminate with [DONE]")
	}
	if sum.Usage == nil || sum.Usage.TotalTokens != 8 {
		t.Errorf("usage = %+v", sum.Usage)
	}

	// The chunk boundaries must be respected exactly, otherwise a test cannot
	// prove an adapter accumulates rather than overwrites.
	var contents []string
	for _, f := range frames {
		if f.Data == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content *string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(f.Data), &chunk); err != nil {
			t.Fatal(err)
		}
		for _, c := range chunk.Choices {
			if c.Delta.Content != nil && *c.Delta.Content != "" {
				contents = append(contents, *c.Delta.Content)
			}
		}
	}
	if strings.Join(contents, "|") != "Hel|lo, |world" {
		t.Errorf("content deltas = %v", contents)
	}
}

func TestStreamFragmentsToolCallArguments(t *testing.T) {
	args := `{"command":"go build ./...","timeout":30}`
	srv := fixtures.NewServer(t)
	srv.Enqueue(fixtures.ToolCallResponse(fixtures.ToolCall("call_abc", "run", args)).
		WithArgumentFragments(5))

	frames := streamChat(t, srv, streamReq)
	sum, err := fixtures.ReassembleOpenAIStream(frames)
	if err != nil {
		t.Fatalf("reassemble: %v", err)
	}
	if len(sum.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", sum.ToolCalls)
	}
	got := sum.ToolCalls[0]
	if got.ID != "call_abc" || got.Name != "run" || got.Arguments != args {
		t.Fatalf("reassembled = %+v", got)
	}
	if sum.Finish != "tool_calls" {
		t.Errorf("finish = %q", sum.Finish)
	}

	// The whole point: no single frame may carry the complete arguments.
	fragments := 0
	for _, f := range frames {
		if f.Data == "[DONE]" {
			continue
		}
		if strings.Contains(f.Data, `go build ./...`) && strings.Contains(f.Data, `timeout`) {
			t.Fatalf("arguments were not fragmented: %s", f.Data)
		}
		if strings.Contains(f.Data, `"arguments"`) {
			fragments++
		}
	}
	if fragments != 5 {
		t.Errorf("argument fragments = %d, want 5", fragments)
	}
}

func TestStreamSplitToolCallIdentity(t *testing.T) {
	srv := fixtures.NewServer(t)
	srv.Enqueue(fixtures.ToolCallResponse(fixtures.ToolCall("call_abc", "run", `{"a":1}`)).
		SplitToolCallIdentity())

	frames := streamChat(t, srv, streamReq)
	for _, f := range frames {
		if strings.Contains(f.Data, `"id":"call_abc"`) {
			t.Fatalf("identity was not split: %s", f.Data)
		}
	}
	sum, err := fixtures.ReassembleOpenAIStream(frames)
	if err != nil {
		t.Fatalf("reassemble: %v", err)
	}
	if len(sum.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", sum.ToolCalls)
	}
	if sum.ToolCalls[0].ID != "call_abc" || sum.ToolCalls[0].Name != "run" {
		t.Errorf("identity did not reassemble: %+v", sum.ToolCalls[0])
	}
}

func TestStreamInterleavesMultipleToolCalls(t *testing.T) {
	srv := fixtures.NewServer(t)
	srv.Enqueue(fixtures.ToolCallResponse(
		fixtures.ToolCall("call_1", "read_file", `{"path":"a.go"}`),
		fixtures.ToolCall("call_2", "read_file", `{"path":"b.go"}`),
	).WithArgumentFragments(3).Interleaved())

	frames := streamChat(t, srv, streamReq)
	sum, err := fixtures.ReassembleOpenAIStream(frames)
	if err != nil {
		t.Fatalf("reassemble: %v", err)
	}
	if len(sum.ToolCalls) != 2 {
		t.Fatalf("tool calls = %+v", sum.ToolCalls)
	}
	if sum.ToolCalls[0].Arguments != `{"path":"a.go"}` || sum.ToolCalls[1].Arguments != `{"path":"b.go"}` {
		t.Fatalf("interleaved fragments mis-assembled: %+v", sum.ToolCalls)
	}

	// Prove the frames really do alternate between indexes.
	var indexes []int
	for _, f := range frames {
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index int `json:"index"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(f.Data), &chunk); err != nil {
			continue
		}
		for _, c := range chunk.Choices {
			for _, tc := range c.Delta.ToolCalls {
				indexes = append(indexes, tc.Index)
			}
		}
	}
	if len(indexes) < 4 || indexes[0] != 0 || indexes[1] != 1 {
		t.Errorf("indexes = %v, want alternating 0,1,...", indexes)
	}
}

func TestStreamWholeToolCallMatchesOllama(t *testing.T) {
	args := `{"city":"Copenhagen"}`
	srv := fixtures.NewServer(t)
	srv.Enqueue(fixtures.ToolCallResponse(fixtures.ToolCall("call_rjzv4ky2", "get_weather", args)).Whole())

	frames := streamChat(t, srv, streamReq)
	var found string
	for _, f := range frames {
		if strings.Contains(f.Data, `"tool_calls":[`) {
			if found != "" {
				t.Fatalf("expected a single tool_calls frame, got another: %s", f.Data)
			}
			found = f.Data
		}
	}
	if found == "" {
		t.Fatal("no tool_calls frame emitted")
	}
	var chunk struct {
		SystemFingerprint string `json:"system_fingerprint"`
		Choices           []struct {
			Delta struct {
				Role      string  `json:"role"`
				Content   *string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Index    int    `json:"index"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(found), &chunk); err != nil {
		t.Fatalf("decode: %v", err)
	}
	d := chunk.Choices[0].Delta
	if d.Role != "assistant" || d.Content == nil || *d.Content != "" {
		t.Errorf("delta should carry role and an explicit empty content: %s", found)
	}
	if len(d.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %+v", d.ToolCalls)
	}
	tc := d.ToolCalls[0]
	if tc.ID != "call_rjzv4ky2" || tc.Type != "function" || tc.Function.Name != "get_weather" {
		t.Errorf("tool call = %+v", tc)
	}
	if tc.Function.Arguments != args {
		t.Errorf("arguments = %q, want the complete object in one delta", tc.Function.Arguments)
	}
	if chunk.Choices[0].FinishReason != nil {
		t.Errorf("finish_reason must be null on the tool call frame, got %v", *chunk.Choices[0].FinishReason)
	}
	if chunk.SystemFingerprint == "" {
		t.Error("system_fingerprint missing")
	}
}

func TestStreamUsageFrameHasEmptyChoices(t *testing.T) {
	srv := fixtures.NewServer(t)
	srv.Enqueue(fixtures.TextResponse("hi").WithUsage(161, 24).WithSystemFingerprint("fp_ollama"))

	frames := streamChat(t, srv, streamReq)
	var usageFrame, finishFrame string
	for _, f := range frames {
		if strings.Contains(f.Data, `"usage"`) {
			usageFrame = f.Data
		}
		if strings.Contains(f.Data, `"finish_reason":"stop"`) {
			finishFrame = f.Data
		}
	}
	if usageFrame == "" {
		t.Fatal("no usage frame")
	}
	if !strings.Contains(usageFrame, `"choices":[]`) {
		t.Fatalf("usage frame must carry an empty choices array: %s", usageFrame)
	}
	if !strings.Contains(usageFrame, `"total_tokens":185`) {
		t.Errorf("usage frame = %s", usageFrame)
	}
	if !strings.Contains(usageFrame, `"system_fingerprint":"fp_ollama"`) {
		t.Errorf("per-response fingerprint override ignored: %s", usageFrame)
	}
	if finishFrame == "" {
		t.Fatal("finish_reason must arrive on its own trailing frame")
	}
	if strings.Contains(finishFrame, `"usage"`) {
		t.Errorf("finish frame should not carry usage: %s", finishFrame)
	}
}

func TestStrictStreamUsageRequiresStreamOptions(t *testing.T) {
	srv := fixtures.NewServer(t, fixtures.WithStrictStreamUsage())
	srv.Enqueue(fixtures.TextResponse("hi").WithUsage(1, 1), fixtures.TextResponse("hi").WithUsage(1, 1))

	frames := streamChat(t, srv, streamReq)
	sum, err := fixtures.ReassembleOpenAIStream(frames)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Usage != nil {
		t.Errorf("usage reported without stream_options.include_usage: %+v", sum.Usage)
	}

	frames = streamChat(t, srv, `{"model":"m","stream":true,"stream_options":{"include_usage":true},"messages":[]}`)
	sum, err = fixtures.ReassembleOpenAIStream(frames)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Usage == nil || sum.Usage.TotalTokens != 2 {
		t.Errorf("usage = %+v, want reported", sum.Usage)
	}
}

func TestStreamRespectsRequestedModelAndCapturesStreamFlag(t *testing.T) {
	srv := fixtures.NewServer(t)
	srv.EnqueueText("hi")
	streamChat(t, srv, streamReq)

	req := srv.MustLastChatRequest(t)
	if !req.Stream {
		t.Error("stream flag not captured")
	}
	if req.Model != "boop-test-model" {
		t.Errorf("model = %q", req.Model)
	}
}

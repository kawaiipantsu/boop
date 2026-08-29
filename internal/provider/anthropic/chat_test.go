package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/boop-dev/boop/internal/provider"
)

// okResponse is a minimal successful Messages API body.
const okResponse = `{
  "id": "msg_test",
  "type": "message",
  "role": "assistant",
  "model": "claude-test-model",
  "content": [{"type": "text", "text": "hello"}],
  "stop_reason": "end_turn",
  "usage": {"input_tokens": 10, "output_tokens": 4}
}`

func TestChatSendsRequiredHeaders(t *testing.T) {
	t.Parallel()

	c, cap := newTestClientWith(t, Options{Beta: []string{"feature-a", "feature-b"}},
		jsonHandler(http.StatusOK, okResponse))

	chatOnce(t, c, userReq("hi"))

	tests := []struct {
		header string
		want   string
	}{
		// Anthropic authenticates with x-api-key, not an Authorization
		// bearer token; sending the wrong one is the classic porting bug.
		{header: "X-Api-Key", want: testAPIKey},
		{header: "Anthropic-Version", want: APIVersion},
		{header: "Anthropic-Beta", want: "feature-a,feature-b"},
		{header: "Content-Type", want: "application/json"},
		{header: "Authorization", want: ""},
	}
	for _, tc := range tests {
		if got := cap.header(tc.header); got != tc.want {
			t.Errorf("header %s = %q, want %q", tc.header, got, tc.want)
		}
	}
	if cap.path != messagesPath {
		t.Errorf("path = %q, want %q", cap.path, messagesPath)
	}
	if cap.method != http.MethodPost {
		t.Errorf("method = %q, want POST", cap.method)
	}
}

func TestChatLiftsSystemMessages(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		messages   []provider.Message
		wantSystem string
		wantRoles  []string
		wantTexts  []string
	}{
		{
			name: "single system message becomes the top-level field",
			messages: []provider.Message{
				{Role: provider.RoleSystem, Content: "be terse"},
				{Role: provider.RoleUser, Content: "hi"},
			},
			wantSystem: "be terse",
			wantRoles:  []string{"user"},
			wantTexts:  []string{"hi"},
		},
		{
			name: "several system messages are joined in order",
			messages: []provider.Message{
				{Role: provider.RoleSystem, Content: "first"},
				{Role: provider.RoleUser, Content: "hi"},
				{Role: provider.RoleSystem, Content: "second"},
			},
			wantSystem: "first\n\nsecond",
			wantRoles:  []string{"user"},
			wantTexts:  []string{"hi"},
		},
		{
			name: "multimodal system parts are flattened to text",
			messages: []provider.Message{
				{Role: provider.RoleSystem, Parts: []provider.ContentPart{
					{Kind: provider.PartText, Text: "line one"},
					{Kind: provider.PartText, Text: "line two"},
				}},
				{Role: provider.RoleUser, Content: "hi"},
			},
			wantSystem: "line one\nline two",
			wantRoles:  []string{"user"},
			wantTexts:  []string{"hi"},
		},
		{
			name: "no system message leaves the field unset",
			messages: []provider.Message{
				{Role: provider.RoleUser, Content: "hi"},
			},
			wantSystem: "",
			wantRoles:  []string{"user"},
			wantTexts:  []string{"hi"},
		},
		{
			name: "consecutive same-role turns are merged so roles alternate",
			messages: []provider.Message{
				{Role: provider.RoleUser, Content: "a"},
				{Role: provider.RoleUser, Content: "b"},
				{Role: provider.RoleAssistant, Content: "c"},
			},
			wantRoles: []string{"user", "assistant"},
			wantTexts: []string{"a", "b", "c"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, cap := newTestClient(t, jsonHandler(http.StatusOK, okResponse))
			chatOnce(t, c, provider.ChatRequest{Model: "claude-test-model", Messages: tc.messages})

			body := cap.request(t)
			gotSystem, _ := body["system"].(string)
			if gotSystem != tc.wantSystem {
				t.Errorf("system = %q, want %q", gotSystem, tc.wantSystem)
			}
			if tc.wantSystem == "" {
				if _, present := body["system"]; present {
					t.Error("an empty system prompt must be omitted, not sent as an empty string")
				}
			}

			msgs, _ := body["messages"].([]any)
			if len(msgs) != len(tc.wantRoles) {
				t.Fatalf("messages = %v, want %d turn(s)", msgs, len(tc.wantRoles))
			}
			var texts []string
			for i, raw := range msgs {
				m, _ := raw.(map[string]any)
				role, _ := m["role"].(string)
				if role != tc.wantRoles[i] {
					t.Errorf("messages[%d].role = %q, want %q", i, role, tc.wantRoles[i])
				}
				if role == "system" {
					t.Fatal("a system message must never appear in the messages array")
				}
				blocks, _ := m["content"].([]any)
				for _, b := range blocks {
					blk, _ := b.(map[string]any)
					if txt, ok := blk["text"].(string); ok {
						texts = append(texts, txt)
					}
				}
			}
			if strings.Join(texts, "|") != strings.Join(tc.wantTexts, "|") {
				t.Errorf("block texts = %v, want %v", texts, tc.wantTexts)
			}
		})
	}
}

func TestChatMaxTokensIsAlwaysSent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		optsMax   int
		requested int
		want      float64
	}{
		{name: "unset falls back to the package default", want: DefaultMaxTokens},
		{name: "explicit request value wins", requested: 512, want: 512},
		{name: "client default overrides the package default", optsMax: 8192, want: 8192},
		{name: "request beats the client default", optsMax: 8192, requested: 99, want: 99},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, cap := newTestClientWith(t, Options{DefaultMaxTokens: tc.optsMax},
				jsonHandler(http.StatusOK, okResponse))

			req := userReq("hi")
			req.MaxTokens = tc.requested
			chatOnce(t, c, req)

			body := cap.request(t)
			got, ok := body["max_tokens"].(float64)
			if !ok {
				t.Fatalf("max_tokens missing from %v; the Messages API requires it", body)
			}
			if got != tc.want {
				t.Errorf("max_tokens = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMaxTokensDefaultIsReported(t *testing.T) {
	t.Parallel()

	if got := New(Options{}).MaxTokensDefault(); got != DefaultMaxTokens {
		t.Errorf("MaxTokensDefault() = %d, want %d", got, DefaultMaxTokens)
	}
	if got := New(Options{DefaultMaxTokens: 1234}).MaxTokensDefault(); got != 1234 {
		t.Errorf("MaxTokensDefault() = %d, want 1234", got)
	}
}

func TestChatTranslatesTools(t *testing.T) {
	t.Parallel()

	c, cap := newTestClient(t, jsonHandler(http.StatusOK, okResponse))

	req := userReq("weather?")
	req.Tools = []provider.ToolDefinition{
		{
			Name:        "get_weather",
			Description: "Get the weather",
			Schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"location": map[string]any{"type": "string"}},
				"required":   []any{"location"},
			},
		},
		{Name: "no_args"},
	}
	chatOnce(t, c, req)

	body := cap.request(t)
	tools, _ := body["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools = %v, want 2", tools)
	}

	first, _ := tools[0].(map[string]any)
	if _, wrong := first["parameters"]; wrong {
		t.Error("tools must use input_schema; the OpenAI-dialect parameters field is not accepted")
	}
	if _, wrong := first["function"]; wrong {
		t.Error("tools must be flat; there is no function wrapper in this dialect")
	}
	schema, ok := first["input_schema"].(map[string]any)
	if !ok {
		t.Fatalf("tools[0] = %v, want an input_schema object", first)
	}
	if schema["type"] != "object" {
		t.Errorf("input_schema.type = %v, want object", schema["type"])
	}
	if first["name"] != "get_weather" || first["description"] != "Get the weather" {
		t.Errorf("tools[0] name/description = %v/%v", first["name"], first["description"])
	}

	second, _ := tools[1].(map[string]any)
	emptySchema, ok := second["input_schema"].(map[string]any)
	if !ok || emptySchema["type"] != "object" {
		t.Errorf("a tool with no schema must still declare an empty object schema, got %v", second)
	}
}

func TestChatTranslatesToolCallsAndResults(t *testing.T) {
	t.Parallel()

	c, cap := newTestClient(t, jsonHandler(http.StatusOK, okResponse))

	chatOnce(t, c, provider.ChatRequest{
		Model: "claude-test-model",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: "weather?"},
			{Role: provider.RoleAssistant, Content: "checking", ToolCalls: []provider.ToolCall{
				{ID: "toolu_1", Name: "get_weather", Arguments: `{"location":"Paris"}`},
			}},
			{Role: provider.RoleTool, ToolCallID: "toolu_1", Content: "18C"},
			{Role: provider.RoleTool, ToolCallID: "toolu_2", Content: "sunny"},
		},
	})

	body := cap.request(t)
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %v, want user, assistant and one merged tool-result turn", msgs)
	}

	assistant, _ := msgs[1].(map[string]any)
	if assistant["role"] != "assistant" {
		t.Fatalf("messages[1].role = %v, want assistant", assistant["role"])
	}
	blocks, _ := assistant["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("assistant content = %v, want a text block and a tool_use block", blocks)
	}
	toolUse, _ := blocks[1].(map[string]any)
	if toolUse["type"] != "tool_use" || toolUse["id"] != "toolu_1" || toolUse["name"] != "get_weather" {
		t.Fatalf("tool_use block = %v", toolUse)
	}
	input, ok := toolUse["input"].(map[string]any)
	if !ok {
		t.Fatalf("tool_use.input = %v, want a parsed object, not the raw arguments string", toolUse["input"])
	}
	if input["location"] != "Paris" {
		t.Errorf("tool_use.input.location = %v, want Paris", input["location"])
	}

	// Tool results go back as a user turn of tool_result blocks; consecutive
	// results merge into that one turn so roles keep alternating.
	results, _ := msgs[2].(map[string]any)
	if results["role"] != "user" {
		t.Fatalf("messages[2].role = %v, want user: there is no tool role in this dialect", results["role"])
	}
	resultBlocks, _ := results["content"].([]any)
	if len(resultBlocks) != 2 {
		t.Fatalf("tool-result turn = %v, want both results merged", resultBlocks)
	}
	for i, want := range []struct{ id, content string }{{"toolu_1", "18C"}, {"toolu_2", "sunny"}} {
		blk, _ := resultBlocks[i].(map[string]any)
		if blk["type"] != "tool_result" || blk["tool_use_id"] != want.id || blk["content"] != want.content {
			t.Errorf("tool_result[%d] = %v, want %+v", i, blk, want)
		}
	}
}

func TestToolInputNormalization(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args string
		want string
	}{
		{name: "object passes through", args: `{"a":1}`, want: `{"a":1}`},
		{name: "empty becomes an empty object", args: "", want: "{}"},
		{name: "whitespace becomes an empty object", args: "   ", want: "{}"},
		{name: "malformed JSON becomes an empty object", args: `{"a":`, want: "{}"},
		{name: "a non-object becomes an empty object", args: `[1,2]`, want: "{}"},
		{name: "a bare string becomes an empty object", args: `"hi"`, want: "{}"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := string(toolInput(tc.args)); got != tc.want {
				t.Errorf("toolInput(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestChatTranslatesMultimodalParts(t *testing.T) {
	t.Parallel()

	c, cap := newTestClient(t, jsonHandler(http.StatusOK, okResponse))

	pngHeader := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	chatOnce(t, c, provider.ChatRequest{
		Model: "claude-test-model",
		Messages: []provider.Message{{Role: provider.RoleUser, Parts: []provider.ContentPart{
			{Kind: provider.PartText, Text: "look"},
			{Kind: provider.PartImage, MIMEType: "image/jpeg", Data: []byte{0xff, 0xd8, 0xff}},
			{Kind: provider.PartImage, Data: pngHeader},
			{Kind: provider.PartImage, Text: "https://example.test/cat.png"},
			{Kind: provider.PartDocument, MIMEType: "application/pdf", Data: []byte("%PDF-1.4")},
			{Kind: provider.PartDocument, MIMEType: "text/plain", Data: []byte("notes")},
		}}},
	})

	body := cap.request(t)
	msgs, _ := body["messages"].([]any)
	turn, _ := msgs[0].(map[string]any)
	blocks, _ := turn["content"].([]any)
	if len(blocks) != 6 {
		t.Fatalf("content blocks = %v, want 6", blocks)
	}

	type check struct {
		blockType  string
		sourceType string
		mediaType  string
		url        string
		data       string
	}
	want := []check{
		{blockType: "text"},
		{blockType: "image", sourceType: "base64", mediaType: "image/jpeg"},
		{blockType: "image", sourceType: "base64", mediaType: "image/png"},
		{blockType: "image", sourceType: "url", url: "https://example.test/cat.png"},
		{blockType: "document", sourceType: "base64", mediaType: "application/pdf"},
		{blockType: "document", sourceType: "text", mediaType: "text/plain", data: "notes"},
	}
	for i, w := range want {
		blk, _ := blocks[i].(map[string]any)
		if blk["type"] != w.blockType {
			t.Errorf("block[%d].type = %v, want %q", i, blk["type"], w.blockType)
			continue
		}
		if w.sourceType == "" {
			continue
		}
		src, ok := blk["source"].(map[string]any)
		if !ok {
			t.Errorf("block[%d] has no source object: %v", i, blk)
			continue
		}
		if src["type"] != w.sourceType {
			t.Errorf("block[%d].source.type = %v, want %q", i, src["type"], w.sourceType)
		}
		if w.mediaType != "" && src["media_type"] != w.mediaType {
			t.Errorf("block[%d].source.media_type = %v, want %q", i, src["media_type"], w.mediaType)
		}
		if w.url != "" && src["url"] != w.url {
			t.Errorf("block[%d].source.url = %v, want %q", i, src["url"], w.url)
		}
		if w.data != "" && src["data"] != w.data {
			t.Errorf("block[%d].source.data = %v, want %q", i, src["data"], w.data)
		}
	}
}

func TestChatOptionalParameters(t *testing.T) {
	t.Parallel()

	c, cap := newTestClient(t, jsonHandler(http.StatusOK, okResponse))

	temp, topP := 0.25, 0.9
	req := userReq("hi")
	req.Temperature = &temp
	req.TopP = &topP
	req.Stop = []string{"END"}
	chatOnce(t, c, req)

	body := cap.request(t)
	if body["temperature"] != 0.25 || body["top_p"] != 0.9 {
		t.Errorf("temperature/top_p = %v/%v", body["temperature"], body["top_p"])
	}
	stops, _ := body["stop_sequences"].([]any)
	if len(stops) != 1 || stops[0] != "END" {
		t.Errorf("stop_sequences = %v, want [END]; this dialect does not use \"stop\"", body["stop_sequences"])
	}
	if _, wrong := body["stop"]; wrong {
		t.Error("request must not carry an OpenAI-style stop field")
	}
	if stream, present := body["stream"]; present && stream != false {
		t.Errorf("stream = %v, want it omitted for a non-streaming request", stream)
	}
}

func TestChatRejectsInvalidRequests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		req     provider.ChatRequest
		wantMsg string
	}{
		{
			name:    "no model",
			req:     provider.ChatRequest{Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}}},
			wantMsg: "no model selected",
		},
		{
			name:    "no messages",
			req:     provider.ChatRequest{Model: "claude-test-model"},
			wantMsg: "empty conversation",
		},
		{
			name: "only system messages",
			req: provider.ChatRequest{Model: "claude-test-model", Messages: []provider.Message{
				{Role: provider.RoleSystem, Content: "be terse"},
			}},
			wantMsg: "only system instructions",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, cap := newTestClient(t, jsonHandler(http.StatusOK, okResponse))

			ch, err := c.Chat(context.Background(), tc.req)
			if err == nil {
				t.Fatal("Chat() succeeded, want a validation error")
			}
			if ch != nil {
				t.Error("Chat() returned a channel alongside a validation error")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantMsg)
			}
			if cat, ok := provider.CategoryOf(err); !ok || cat != provider.ErrInvalidRequest {
				t.Errorf("category = %q, want %q", cat, provider.ErrInvalidRequest)
			}
			if cap.count() != 0 {
				t.Error("an invalid request must not reach the network")
			}
		})
	}
}

func TestChatNonStreamingResponse(t *testing.T) {
	t.Parallel()

	body := `{
	  "id": "msg_test",
	  "type": "message",
	  "role": "assistant",
	  "model": "claude-test-model",
	  "content": [
	    {"type": "thinking", "thinking": "let me check"},
	    {"type": "text", "text": "It is sunny."},
	    {"type": "tool_use", "id": "toolu_9", "name": "get_weather", "input": {"location": "Paris"}}
	  ],
	  "stop_reason": "tool_use",
	  "usage": {"input_tokens": 12, "output_tokens": 30, "cache_read_input_tokens": 8, "cache_creation_input_tokens": 2}
	}`
	c, _ := newTestClient(t, jsonHandler(http.StatusOK, body))

	events := chatOnce(t, c, userReq("weather?"))

	if got := reasoningOf(events); got != "let me check" {
		t.Errorf("reasoning = %q, want %q", got, "let me check")
	}
	if got := textOf(events); got != "It is sunny." {
		t.Errorf("text = %q", got)
	}
	calls := toolCallsOf(events)
	if len(calls) != 1 {
		t.Fatalf("tool calls = %v, want 1", calls)
	}
	if calls[0].ID != "toolu_9" || calls[0].Name != "get_weather" {
		t.Errorf("tool call = %+v", calls[0])
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatalf("tool arguments are not valid JSON: %v (%q)", err, calls[0].Arguments)
	}
	if args["location"] != "Paris" {
		t.Errorf("tool arguments = %v", args)
	}

	usage := usageOf(events)
	if usage == nil {
		t.Fatal("no usage event")
	}
	// The prompt is split across three counters; PromptTokens is their sum
	// and CachedTokens is the read portion of it.
	if usage.PromptTokens != 22 || usage.CompletionTokens != 30 || usage.TotalTokens != 52 || usage.CachedTokens != 8 {
		t.Errorf("usage = %+v, want prompt 22, completion 30, total 52, cached 8", *usage)
	}

	if finish := events[len(events)-1].Finish; finish != provider.FinishToolCalls {
		t.Errorf("finish = %q, want %q", finish, provider.FinishToolCalls)
	}
}

func TestFinishReasonMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw     string
		sawTool bool
		want    provider.FinishReason
	}{
		{raw: "end_turn", want: provider.FinishStop},
		{raw: "stop_sequence", want: provider.FinishStop},
		{raw: "max_tokens", want: provider.FinishLength},
		{raw: "tool_use", want: provider.FinishToolCalls},
		{raw: "end_turn", sawTool: true, want: provider.FinishToolCalls},
		// A refusal is a completed turn, not a fault.
		{raw: "refusal", want: provider.FinishStop},
		{raw: "pause_turn", want: provider.FinishStop},
		{raw: "", want: provider.FinishStop},
		{raw: "something_new", want: provider.FinishStop},
		{raw: "something_new", sawTool: true, want: provider.FinishToolCalls},
	}
	for _, tc := range tests {
		if got := finishReason(tc.raw, tc.sawTool); got != tc.want {
			t.Errorf("finishReason(%q, %v) = %q, want %q", tc.raw, tc.sawTool, got, tc.want)
		}
	}
}

func TestConvertUsage(t *testing.T) {
	t.Parallel()

	if got := convertUsage(nil); got != nil {
		t.Errorf("convertUsage(nil) = %v, want nil", got)
	}
	if got := convertUsage(&wireUsage{}); got != nil {
		t.Errorf("convertUsage(empty) = %v, want nil so no empty usage event is emitted", got)
	}
	got := convertUsage(&wireUsage{InputTokens: 5, OutputTokens: 7, CacheReadInputTokens: 3, CacheCreationInputTokens: 1})
	want := provider.Usage{PromptTokens: 9, CompletionTokens: 7, TotalTokens: 16, CachedTokens: 3}
	if got == nil || *got != want {
		t.Errorf("convertUsage() = %+v, want %+v", got, want)
	}
}

func TestChatContextCancellationClosesTheStream(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t, jsonHandler(http.StatusOK, okResponse))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ch, err := c.Chat(ctx, userReq("hi"))
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	// The adapter owns the channel and must always close it, even when the
	// caller abandoned the request before it started.
	for range ch {
	}
}

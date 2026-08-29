package openaicompat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/provider"
)

func TestChatRequestValidation(t *testing.T) {
	client := New(Options{BaseURL: "http://127.0.0.1:1/v1"})
	tests := []struct {
		name string
		req  provider.ChatRequest
	}{
		{"no model", provider.ChatRequest{Messages: []provider.Message{{Role: provider.RoleUser, Content: "hi"}}}},
		{"no messages", provider.ChatRequest{Model: "m"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ch, err := client.Chat(context.Background(), tc.req)
			if err == nil {
				t.Fatal("Chat succeeded, want an invalid-request error")
			}
			if ch != nil {
				t.Error("Chat returned a channel alongside an error")
			}
			if cat, ok := provider.CategoryOf(err); !ok || cat != provider.ErrInvalidRequest {
				t.Errorf("category = %v (ok=%v), want invalid_request", cat, ok)
			}
		})
	}
}

func TestChatRequestBodyShape(t *testing.T) {
	var body map[string]any
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		body = decodeBody(t, r)
		_, _ = w.Write([]byte(`{"choices":[{"index":0,"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	})

	temp, topP := 0.25, 0.9
	req := provider.ChatRequest{
		Model: "test-model",
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: "be brief"},
			{Role: provider.RoleUser, Content: "weather?"},
			{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "c1", Name: "get_weather", Arguments: `{"city":"Aarhus"}`}}},
			{Role: provider.RoleTool, ToolCallID: "c1", Name: "get_weather", Content: `{"temp":17}`},
		},
		Tools: []provider.ToolDefinition{{
			Name:        "get_weather",
			Description: "look up weather",
			Schema:      map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
		}},
		Temperature: &temp,
		TopP:        &topP,
		MaxTokens:   256,
		Stop:        []string{"</done>"},
	}
	collect(t, mustChat(t, context.Background(), client, req))

	if body["model"] != "test-model" {
		t.Errorf("model = %v", body["model"])
	}
	if body["stream"] != false {
		t.Errorf("stream = %v, want false", body["stream"])
	}
	if _, ok := body["stream_options"]; ok {
		t.Error("stream_options must be absent on a non-streaming request")
	}
	if body["temperature"] != 0.25 || body["top_p"] != 0.9 || body["max_tokens"] != float64(256) {
		t.Errorf("sampling params = %v/%v/%v", body["temperature"], body["top_p"], body["max_tokens"])
	}
	if stop, _ := body["stop"].([]any); len(stop) != 1 || stop[0] != "</done>" {
		t.Errorf("stop = %v", body["stop"])
	}

	msgs, _ := body["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages = %d, want 4", len(msgs))
	}
	assistant, _ := msgs[2].(map[string]any)
	calls, _ := assistant["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("assistant tool_calls = %v", assistant["tool_calls"])
	}
	call, _ := calls[0].(map[string]any)
	fn, _ := call["function"].(map[string]any)
	if call["id"] != "c1" || call["type"] != "function" || fn["name"] != "get_weather" || fn["arguments"] != `{"city":"Aarhus"}` {
		t.Errorf("tool call on the wire = %v", call)
	}
	if _, hasContent := assistant["content"]; hasContent {
		t.Error("assistant tool-call turn must not carry an empty content field")
	}
	toolMsg, _ := msgs[3].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "c1" || toolMsg["name"] != "get_weather" {
		t.Errorf("tool result message = %v", toolMsg)
	}

	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", body["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	tfn, _ := tool["function"].(map[string]any)
	if tool["type"] != "function" || tfn["name"] != "get_weather" || tfn["description"] != "look up weather" {
		t.Errorf("tool on the wire = %v", tool)
	}
	if _, ok := tfn["parameters"].(map[string]any); !ok {
		t.Errorf("tool parameters = %v, want the JSON schema object", tfn["parameters"])
	}
}

func TestChatStreamRequestAsksForUsage(t *testing.T) {
	var body map[string]any
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		body = decodeBody(t, r)
		sse := newSSEWriter(t, w)
		sse.send(`{"choices":[{"delta":{"content":"x"},"finish_reason":"stop"}]}`)
		sse.done()
	})
	collect(t, mustChat(t, context.Background(), client, simpleRequest(true)))

	if body["stream"] != true {
		t.Errorf("stream = %v, want true", body["stream"])
	}
	opts, _ := body["stream_options"].(map[string]any)
	if opts == nil || opts["include_usage"] != true {
		t.Errorf("stream_options = %v, want include_usage true", body["stream_options"])
	}
}

func TestChatMultimodalRequestBody(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x01, 0x02, 0x03}
	pdf := []byte("%PDF-1.7 fake")

	var body map[string]any
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		body = decodeBody(t, r)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"seen"},"finish_reason":"stop"}]}`))
	})

	req := provider.ChatRequest{
		Model: "vision-model",
		Messages: []provider.Message{{
			Role: provider.RoleUser,
			Parts: []provider.ContentPart{
				{Kind: provider.PartText, Text: "what is this?"},
				{Kind: provider.PartImage, MIMEType: "image/png", Data: png},
				{Kind: provider.PartImage, Data: png}, // MIME sniffed
				{Kind: provider.PartImage, Text: "https://example.invalid/cat.png"},
				{Kind: provider.PartDocument, MIMEType: "application/pdf", Filename: "spec.pdf", Data: pdf},
			},
		}},
	}
	collect(t, mustChat(t, context.Background(), client, req))

	msgs, _ := body["messages"].([]any)
	msg, _ := msgs[0].(map[string]any)
	parts, _ := msg["content"].([]any)
	if len(parts) != 5 {
		t.Fatalf("content parts = %d, want 5: %v", len(parts), msg["content"])
	}

	text, _ := parts[0].(map[string]any)
	if text["type"] != "text" || text["text"] != "what is this?" {
		t.Errorf("text part = %v", text)
	}

	wantDataURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	for i := 1; i <= 2; i++ {
		img, _ := parts[i].(map[string]any)
		url, _ := img["image_url"].(map[string]any)
		if img["type"] != "image_url" {
			t.Errorf("part %d type = %v, want image_url", i, img["type"])
		}
		if url["url"] != wantDataURI {
			t.Errorf("part %d url = %v, want a base64 data URI", i, url["url"])
		}
	}

	remote, _ := parts[3].(map[string]any)
	remoteURL, _ := remote["image_url"].(map[string]any)
	if remoteURL["url"] != "https://example.invalid/cat.png" {
		t.Errorf("remote image url = %v", remoteURL["url"])
	}

	doc, _ := parts[4].(map[string]any)
	file, _ := doc["file"].(map[string]any)
	if doc["type"] != "file" || file["filename"] != "spec.pdf" {
		t.Errorf("document part = %v", doc)
	}
	if data, _ := file["file_data"].(string); !strings.HasPrefix(data, "data:application/pdf;base64,") {
		t.Errorf("document data uri = %v", file["file_data"])
	}
}

func TestChatNonStreamingEventSequence(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"index": 0,
				"message": map[string]any{
					"role":              "assistant",
					"content":           "the answer",
					"reasoning_content": "let me think",
					"tool_calls": []any{map[string]any{
						"id": "call_1", "type": "function",
						"function": map[string]any{"name": "lookup", "arguments": `{"q":"x"}`},
					}},
				},
				"finish_reason": "tool_calls",
			}},
			"usage": map[string]any{
				"prompt_tokens": 13, "completion_tokens": 2, "total_tokens": 15,
				"prompt_tokens_details": map[string]any{"cached_tokens": 8},
			},
		})
	})

	events := collect(t, mustChat(t, context.Background(), client, simpleRequest(false)))
	want := []provider.EventType{
		provider.EventReasoning, provider.EventDelta,
		provider.EventToolCall, provider.EventUsage, provider.EventDone,
	}
	got := types(events)
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events = %v, want %v", got, want)
		}
	}
	if textOf(events, provider.EventDelta) != "the answer" {
		t.Errorf("delta = %q", textOf(events, provider.EventDelta))
	}
	if textOf(events, provider.EventReasoning) != "let me think" {
		t.Errorf("reasoning = %q", textOf(events, provider.EventReasoning))
	}
	usage := eventsOf(events, provider.EventUsage)[0].Usage
	if *usage != (provider.Usage{PromptTokens: 13, CompletionTokens: 2, TotalTokens: 15, CachedTokens: 8}) {
		t.Errorf("usage = %+v", *usage)
	}
	assertSingleDone(t, events, provider.FinishToolCalls)
}

// TestChatWithoutToolsSucceeds mirrors the live Ollama behaviour: the identical
// request works once tools are omitted.
func TestChatWithoutToolsSucceeds(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		body := decodeBody(t, r)
		if _, hasTools := body["tools"]; hasTools {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"registry.ollama.ai/library/qwen:7b does not support tools","type":"invalid_request_error","param":null,"code":null}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"Hello!"},"finish_reason":"stop"}],"usage":{"prompt_tokens":13,"completion_tokens":2,"total_tokens":15}}`))
	})

	events := collect(t, mustChat(t, context.Background(), client, simpleRequest(false)))
	if got := textOf(events, provider.EventDelta); got != "Hello!" {
		t.Errorf("content = %q", got)
	}
	usage := eventsOf(events, provider.EventUsage)
	if len(usage) != 1 || usage[0].Usage.TotalTokens != 15 {
		t.Fatalf("usage events = %v", usage)
	}
	assertSingleDone(t, events, provider.FinishStop)
}

func TestChatNoChoicesIsMalformed(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[]}`))
	})
	events := collect(t, mustChat(t, context.Background(), client, simpleRequest(false)))
	if len(events) != 1 || events[0].Type != provider.EventError {
		t.Fatalf("events = %v, want one error", types(events))
	}
	if cat, _ := provider.CategoryOf(events[0].Err); cat != provider.ErrMalformedResponse {
		t.Errorf("category = %v, want malformed_response", cat)
	}
}

func TestFinishReasonMapping(t *testing.T) {
	tests := []struct {
		raw     string
		sawTool bool
		want    provider.FinishReason
	}{
		{"stop", false, provider.FinishStop},
		{"stop", true, provider.FinishToolCalls},
		{"length", false, provider.FinishLength},
		{"max_tokens", false, provider.FinishLength},
		{"tool_calls", false, provider.FinishToolCalls},
		{"function_call", false, provider.FinishToolCalls},
		{"", false, provider.FinishStop},
		{"", true, provider.FinishToolCalls},
		{"weird", false, provider.FinishStop},
		{"cancelled", false, provider.FinishCancelled},
	}
	for _, tc := range tests {
		if got := finishReason(tc.raw, tc.sawTool); got != tc.want {
			t.Errorf("finishReason(%q, %v) = %q, want %q", tc.raw, tc.sawTool, got, tc.want)
		}
	}
}

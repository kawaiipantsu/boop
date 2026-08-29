package openaicompat

import (
	"encoding/json"
	"strings"
)

// This file holds the OpenAI wire shapes. They are unexported on purpose:
// vendor adapters express differences through Options hooks and the raw
// GetJSON/PostJSON escape hatches, never by reaching into these structs.

// chatRequest is the JSON body of a chat completion request.
type chatRequest struct {
	Model         string         `json:"model"`
	Messages      []chatMessage  `json:"messages"`
	Tools         []chatTool     `json:"tools,omitempty"`
	Temperature   *float64       `json:"temperature,omitempty"`
	TopP          *float64       `json:"top_p,omitempty"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	Stop          []string       `json:"stop,omitempty"`
	Stream        bool           `json:"stream"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

// streamOptions asks the server to include usage in the final stream chunk.
// Servers that do not know the field ignore it; those that do stop hiding token
// accounting behind the non-streaming path.
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// chatMessage is one conversation turn on the wire. Content is either a string
// or an array of content parts, which is why it is typed as any.
type chatMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content,omitempty"`
	Name       string         `json:"name,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

// textPart, imagePart and filePart are the multimodal content elements.
type textPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type imagePart struct {
	Type     string        `json:"type"`
	ImageURL imageURLValue `json:"image_url"`
}

type imageURLValue struct {
	URL string `json:"url"`
}

type filePart struct {
	Type string    `json:"type"`
	File fileValue `json:"file"`
}

type fileValue struct {
	Filename string `json:"filename,omitempty"`
	FileData string `json:"file_data,omitempty"`
}

// chatTool advertises a function tool.
type chatTool struct {
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

// wireToolCall is a tool call as sent back to the server in history.
type wireToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function wireToolFunction `json:"function"`
}

type wireToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// chatResponse is a non-streaming completion.
type chatResponse struct {
	ID      string       `json:"id"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   *wireUsage   `json:"usage"`
	Error   *errorObject `json:"error"`
}

type chatChoice struct {
	Index        int          `json:"index"`
	Message      *wireMessage `json:"message"`
	Delta        *wireMessage `json:"delta"`
	FinishReason string       `json:"finish_reason"`
}

// wireMessage covers both the non-streaming message and the streaming delta,
// which share a shape.
type wireMessage struct {
	Role      string          `json:"role"`
	Content   flexContent     `json:"content"`
	ToolCalls []deltaToolCall `json:"tool_calls"`

	// Reasoning traces have no standard field name yet: DeepSeek and vLLM use
	// reasoning_content, OpenRouter and xAI use reasoning, and several local
	// servers use thinking. All three are accepted.
	ReasoningContent string      `json:"reasoning_content"`
	Reasoning        flexContent `json:"reasoning"`
	Thinking         string      `json:"thinking"`
}

// reasoningText returns whichever reasoning field the server populated.
func (m *wireMessage) reasoningText() string {
	switch {
	case m.ReasoningContent != "":
		return m.ReasoningContent
	case m.Reasoning.Text != "":
		return m.Reasoning.Text
	default:
		return m.Thinking
	}
}

// deltaToolCall is a possibly partial tool call fragment. Index is a pointer so
// the accumulator can tell "index 0" from "field absent", which is the only
// signal available for servers that emit whole tool calls without indices.
type deltaToolCall struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function *struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// streamChunk is one SSE payload of a streaming completion.
type streamChunk struct {
	ID      string       `json:"id"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   *wireUsage   `json:"usage"`
	Error   *errorObject `json:"error"`
}

type wireUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// InputTokens/OutputTokens are the Responses-API spelling that some
	// gateways use even on the completions endpoint.
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	// Ollama-style aliases seen on some /v1 shims.
	PromptEvalCount int `json:"prompt_eval_count"`
	EvalCount       int `json:"eval_count"`
}

// empty reports whether the server sent no usable accounting.
func (u *wireUsage) empty() bool {
	return u == nil || (u.PromptTokens == 0 && u.CompletionTokens == 0 && u.TotalTokens == 0 &&
		u.InputTokens == 0 && u.OutputTokens == 0 && u.PromptEvalCount == 0 && u.EvalCount == 0)
}

// flexContent decodes a content field that may be null, a string, or an array
// of typed parts. Servers disagree, and a type error here would abort an
// otherwise good stream.
type flexContent struct {
	Text string
}

// UnmarshalJSON implements json.Unmarshaler.
func (f *flexContent) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" || trimmed == "null" {
		f.Text = ""
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		f.Text = s
		return nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b, &parts); err == nil {
		var sb strings.Builder
		for _, p := range parts {
			sb.WriteString(p.Text)
		}
		f.Text = sb.String()
		return nil
	}
	// Unknown shape: ignore rather than fail the whole response.
	f.Text = ""
	return nil
}

// MarshalJSON implements json.Marshaler so round-tripping stays lossless.
func (f flexContent) MarshalJSON() ([]byte, error) { return json.Marshal(f.Text) }

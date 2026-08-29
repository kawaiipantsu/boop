package anthropic

import "encoding/json"

// This file holds the Anthropic Messages API wire shapes. They are unexported:
// nothing outside this adapter may depend on the vendor dialect.

// messagesRequest is the JSON body of POST /v1/messages.
//
// Note the two shape differences from the OpenAI dialect that drive this whole
// package: System is a top-level field rather than a message, and MaxTokens is
// required rather than optional.
type messagesRequest struct {
	Model         string        `json:"model"`
	Messages      []wireMessage `json:"messages"`
	MaxTokens     int           `json:"max_tokens"`
	System        string        `json:"system,omitempty"`
	Tools         []wireTool    `json:"tools,omitempty"`
	Temperature   *float64      `json:"temperature,omitempty"`
	TopP          *float64      `json:"top_p,omitempty"`
	StopSequences []string      `json:"stop_sequences,omitempty"`
	Stream        bool          `json:"stream,omitempty"`
}

// wireMessage is one conversation turn. Anthropic only accepts the roles
// "user" and "assistant" here; system prompts and tool results are expressed
// differently (see chat.go).
type wireMessage struct {
	Role    string      `json:"role"`
	Content []wireBlock `json:"content"`
}

// wireTool advertises a tool. The schema field is input_schema, not the
// OpenAI-dialect function.parameters.
type wireTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

// wireBlock is a content block in either direction.
//
// It is one flat struct rather than a union because the field sets barely
// overlap and json omitempty keeps the emitted objects exact; decoding
// dispatches on Type.
type wireBlock struct {
	Type string `json:"type"`

	// type: "text"
	Text string `json:"text,omitempty"`

	// type: "thinking"
	Thinking string `json:"thinking,omitempty"`

	// type: "image" | "document"
	Source *wireSource `json:"source,omitempty"`

	// type: "tool_use"
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// type: "tool_result"
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

// wireSource carries binary or referenced media for image and document blocks.
type wireSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// messagesResponse is a non-streaming completion.
type messagesResponse struct {
	ID         string      `json:"id"`
	Type       string      `json:"type"`
	Role       string      `json:"role"`
	Model      string      `json:"model"`
	Content    []wireBlock `json:"content"`
	StopReason string      `json:"stop_reason"`
	Usage      *wireUsage  `json:"usage"`
	// Error is present when the server reports a failure in the body.
	Error *wireError `json:"error"`
}

// wireUsage is Anthropic token accounting.
//
// InputTokens counts only the uncached prompt; the cache fields hold the rest,
// which is why PromptTokens is assembled from all three (see convertUsage).
type wireUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

func (u *wireUsage) empty() bool {
	return u == nil || (u.InputTokens == 0 && u.OutputTokens == 0 &&
		u.CacheCreationInputTokens == 0 && u.CacheReadInputTokens == 0)
}

// wireError is the object inside Anthropic's {"type":"error","error":{...}}
// envelope, used for both HTTP failures and mid-stream error events.
type wireError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// errorEnvelope is the top-level error body.
type errorEnvelope struct {
	Type      string     `json:"type"`
	Error     *wireError `json:"error"`
	RequestID string     `json:"request_id"`
}

// streamEvent is one decoded SSE data frame.
//
// Anthropic's stream is typed: the frame's own "type" field selects the event,
// unlike the OpenAI dialect where every frame is a completion chunk. The
// per-event payload fields are all optional here and read according to Type.
type streamEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`

	// message_start
	Message *messagesResponse `json:"message"`

	// content_block_start
	ContentBlock *wireBlock `json:"content_block"`

	// content_block_delta and message_delta
	Delta *streamDelta `json:"delta"`

	// message_delta carries the output-token half of usage.
	Usage *wireUsage `json:"usage"`

	// error
	Error *wireError `json:"error"`
}

// streamDelta covers both content_block_delta payloads (discriminated by Type)
// and the message_delta payload (which carries stop_reason instead).
type streamDelta struct {
	Type string `json:"type"`
	// type: "text_delta"
	Text string `json:"text"`
	// type: "thinking_delta"
	Thinking string `json:"thinking"`
	// type: "input_json_delta" — a fragment of the tool input object.
	PartialJSON string `json:"partial_json"`
	// message_delta
	StopReason string `json:"stop_reason"`
}

// modelsResponse is GET /v1/models. It is cursor paginated.
type modelsResponse struct {
	Data    []wireModel `json:"data"`
	HasMore bool        `json:"has_more"`
	LastID  string      `json:"last_id"`
}

// wireModel is one entry of the model listing. MaxInputTokens and MaxTokens
// were added to the listing in 2026; older deployments omit them and the
// corresponding provider.Model fields stay zero.
type wireModel struct {
	ID             string `json:"id"`
	Type           string `json:"type"`
	DisplayName    string `json:"display_name"`
	CreatedAt      string `json:"created_at"`
	MaxInputTokens int    `json:"max_input_tokens"`
	MaxTokens      int    `json:"max_tokens"`
}

// Package provider defines the provider-neutral model runtime contract.
//
// Nothing outside this package may depend on a specific vendor. Adapters live
// in subpackages and are reached only through the Provider interface and the
// optional interfaces declared here.
package provider

import (
	"context"
	"time"
)

// Role identifies the author of a message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// PartKind discriminates the payload of a ContentPart.
type PartKind string

const (
	PartText     PartKind = "text"
	PartImage    PartKind = "image"
	PartDocument PartKind = "document"
)

// ContentPart is one element of a multimodal message body.
type ContentPart struct {
	Kind     PartKind `json:"kind"`
	Text     string   `json:"text,omitempty"`
	MIMEType string   `json:"mime_type,omitempty"`
	Data     []byte   `json:"data,omitempty"`
	Filename string   `json:"filename,omitempty"`
}

// Message is a single turn in a conversation.
//
// Content carries the common text-only case. Parts carries multimodal bodies;
// when Parts is non-empty it takes precedence over Content.
type Message struct {
	Role       Role          `json:"role"`
	Content    string        `json:"content,omitempty"`
	Parts      []ContentPart `json:"parts,omitempty"`
	Name       string        `json:"name,omitempty"`
	ToolCalls  []ToolCall    `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
}

// ToolCall is a model request to invoke a registered tool.
//
// Arguments is the raw JSON object emitted by the model; it is deliberately not
// decoded here so the tool runtime owns schema validation.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolDefinition advertises a callable tool to the model.
type ToolDefinition struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Schema is a JSON Schema object describing the tool arguments.
	Schema map[string]any `json:"schema"`
}

// Model describes a model offered by a provider.
type Model struct {
	ID            string       `json:"id"`
	Provider      string       `json:"provider"`
	DisplayName   string       `json:"display_name,omitempty"`
	ContextWindow int          `json:"context_window,omitempty"`
	MaxOutput     int          `json:"max_output,omitempty"`
	Capabilities  Capabilities `json:"capabilities,omitempty"`
}

// ChatRequest is a provider-neutral completion request.
type ChatRequest struct {
	Model       string           `json:"model"`
	Messages    []Message        `json:"messages"`
	Tools       []ToolDefinition `json:"tools,omitempty"`
	Temperature *float64         `json:"temperature,omitempty"`
	TopP        *float64         `json:"top_p,omitempty"`
	MaxTokens   int              `json:"max_tokens,omitempty"`
	Stop        []string         `json:"stop,omitempty"`
	// Stream requests incremental delivery. Adapters that cannot stream must
	// still emit a well-formed event sequence terminating in EventDone.
	Stream bool `json:"stream"`
}

// EventType discriminates a ChatEvent.
type EventType string

const (
	// EventDelta carries an incremental slice of assistant text.
	EventDelta EventType = "delta"
	// EventReasoning carries incremental reasoning/thinking text where the
	// model exposes it separately from the answer.
	EventReasoning EventType = "reasoning"
	// EventToolCall carries a fully assembled tool call.
	EventToolCall EventType = "tool_call"
	// EventUsage carries token accounting, usually just before EventDone.
	EventUsage EventType = "usage"
	// EventDone is the final event of a successful stream.
	EventDone EventType = "done"
	// EventError is the final event of a failed stream.
	EventError EventType = "error"
)

// Usage is token accounting for one exchange.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// CachedTokens is the prompt portion served from a provider-side cache.
	CachedTokens int `json:"cached_tokens,omitempty"`
}

// FinishReason explains why generation stopped.
type FinishReason string

const (
	FinishStop      FinishReason = "stop"
	FinishLength    FinishReason = "length"
	FinishToolCalls FinishReason = "tool_calls"
	FinishCancelled FinishReason = "cancelled"
	FinishError     FinishReason = "error"
)

// ChatEvent is one element of a streamed response.
//
// Exactly one of EventDone or EventError terminates a stream, after which the
// channel is closed by the adapter.
type ChatEvent struct {
	Type     EventType    `json:"type"`
	Text     string       `json:"text,omitempty"`
	ToolCall *ToolCall    `json:"tool_call,omitempty"`
	Usage    *Usage       `json:"usage,omitempty"`
	Finish   FinishReason `json:"finish,omitempty"`
	Err      error        `json:"-"`
	At       time.Time    `json:"at"`
}

// Provider is the contract every model backend implements.
//
// Chat returns a channel that the adapter owns and closes. Callers must drain
// it or cancel ctx; adapters must not block on an abandoned channel after ctx
// is cancelled.
type Provider interface {
	Name() string
	Health(ctx context.Context) error
	ListModels(ctx context.Context) ([]Model, error)
	Chat(ctx context.Context, req ChatRequest) (<-chan ChatEvent, error)
	Capabilities(ctx context.Context, model string) (Capabilities, error)
}

// ModelLifecycleProvider is implemented by backends that can load and unload
// models on demand, such as Lemonade and LM Studio.
type ModelLifecycleProvider interface {
	LoadModel(ctx context.Context, model string) error
	UnloadModel(ctx context.Context, model string) error
}

// EmbeddingRequest asks a provider to embed one or more inputs.
type EmbeddingRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// EmbeddingResponse carries embedding vectors in input order.
type EmbeddingResponse struct {
	Vectors [][]float32 `json:"vectors"`
	Usage   Usage       `json:"usage"`
}

// EmbeddingProvider is implemented by backends that expose embeddings.
type EmbeddingProvider interface {
	Embed(ctx context.Context, req EmbeddingRequest) (EmbeddingResponse, error)
}

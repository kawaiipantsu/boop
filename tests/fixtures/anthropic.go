package fixtures

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/boop-dev/boop/internal/provider"
)

// anthropicMessagesPath is the Anthropic Messages endpoint.
const anthropicMessagesPath = "/v1/messages"

// AnthropicRequest is the decoded body of a captured POST /v1/messages.
//
// System is raw because Anthropic accepts either a string or an array of
// content blocks there.
type AnthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	System        json.RawMessage    `json:"system,omitempty"`
	Messages      []AnthropicMessage `json:"messages"`
	Tools         []AnthropicTool    `json:"tools,omitempty"`
	Stream        bool               `json:"stream"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
}

// AnthropicMessage is one message of a captured Anthropic request.
type AnthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// AnthropicTool is a tool advertised on a captured Anthropic request. Note the
// field is input_schema, not OpenAI's function.parameters.
type AnthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// AnthropicRequests returns every captured /v1/messages request, decoded.
func (s *Server) AnthropicRequests() []AnthropicRequest {
	var out []AnthropicRequest
	for _, c := range s.RequestsTo(anthropicMessagesPath) {
		var req AnthropicRequest
		if err := c.JSON(&req); err != nil {
			continue
		}
		out = append(out, req)
	}
	return out
}

// anthropicErrorBody builds Anthropic's error envelope, which differs from
// OpenAI's and is a common source of adapter mistakes.
func anthropicErrorBody(kind, message string) map[string]any {
	return map[string]any{
		"type":  "error",
		"error": map[string]any{"type": kind, "message": message},
	}
}

// anthropicErrorType maps an HTTP status onto Anthropic's error type strings.
func anthropicErrorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable:
		return "overloaded_error"
	default:
		return "api_error"
	}
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
}

type anthropicBlock struct {
	Type string `json:"type"`
	// Text blocks.
	Text string `json:"text,omitempty"`
	// Thinking blocks.
	Thinking string `json:"thinking,omitempty"`
	// tool_use blocks.
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// handleAnthropicMessages serves POST /v1/messages from the same scripted
// queue as the OpenAI-compatible endpoint.
func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	var req AnthropicRequest
	body, _ := readBody(r)
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, anthropicErrorBody("invalid_request_error",
			fmt.Sprintf("could not parse request body: %v", err)))
		return
	}

	resp := s.nextResponse()
	if resp.Delay > 0 {
		time.Sleep(resp.Delay)
	}
	applyHeaders(w, resp)

	if resp.Status != 0 {
		if resp.RawBody != "" {
			writeRaw(w, resp.Status, "application/json", resp.RawBody)
			return
		}
		writeJSON(w, resp.Status, anthropicErrorBody(anthropicErrorType(resp.Status),
			orDefault(resp.Text, http.StatusText(resp.Status))))
		return
	}
	if resp.RawBody != "" {
		ct := "application/json"
		if req.Stream {
			ct = "text/event-stream"
		}
		writeRaw(w, http.StatusOK, ct, resp.RawBody)
		return
	}

	model := orDefault(resp.Model, orDefault(req.Model, "boop-test-model"))
	id := fmt.Sprintf("msg_%d", s.nextSeq())
	if req.Stream {
		s.streamAnthropic(w, resp, id, model)
		return
	}
	s.completeAnthropic(w, resp, id, model)
}

// anthropicStopReason maps the neutral finish reason onto Anthropic's names.
func anthropicStopReason(finish string) string {
	switch finish {
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	default:
		return "end_turn"
	}
}

// completeAnthropic renders the non-streaming Messages body.
func (s *Server) completeAnthropic(w http.ResponseWriter, resp *Response, id, model string) {
	blocks := make([]anthropicBlock, 0, 2+len(resp.ToolCalls))
	if resp.Reasoning != "" {
		blocks = append(blocks, anthropicBlock{Type: "thinking", Thinking: resp.Reasoning})
	}
	if resp.Text != "" {
		blocks = append(blocks, anthropicBlock{Type: "text", Text: resp.Text})
	}
	for _, tc := range resp.ToolCalls {
		blocks = append(blocks, anthropicBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Name,
			Input: rawInput(tc.Arguments),
		})
	}
	out := map[string]any{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       blocks,
		"stop_reason":   anthropicStopReason(resp.finishReason()),
		"stop_sequence": nil,
		"usage":         toAnthropicUsage(resp.Usage),
	}
	writeJSON(w, http.StatusOK, out)
}

// streamAnthropic renders Anthropic's SSE dialect: named events, one indexed
// content block per text/thinking/tool_use segment, input_json_delta for tool
// arguments, and message_stop instead of a [DONE] sentinel.
func (s *Server) streamAnthropic(w http.ResponseWriter, resp *Response, id, model string) {
	sw, err := newSSEWriter(w, resp)
	if err != nil {
		s.t.Errorf("fixtures: %v", err)
		return
	}
	defer sw.finish()

	event := func(name string, payload map[string]any) bool {
		payload["type"] = name
		data, err := json.Marshal(payload)
		if err != nil {
			s.t.Errorf("fixtures: marshal %s: %v", name, err)
			return false
		}
		return sw.frame(name, string(data))
	}

	usage := toAnthropicUsage(resp.Usage)
	if !event("message_start", map[string]any{"message": map[string]any{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       []anthropicBlock{},
		"stop_reason":   nil,
		"stop_sequence": nil,
		"usage":         anthropicUsage{InputTokens: usage.InputTokens},
	}}) {
		return
	}

	index := 0
	block := func(start map[string]any, deltas []map[string]any) bool {
		if !event("content_block_start", map[string]any{"index": index, "content_block": start}) {
			return false
		}
		for _, d := range deltas {
			if !event("content_block_delta", map[string]any{"index": index, "delta": d}) {
				return false
			}
		}
		if !event("content_block_stop", map[string]any{"index": index}) {
			return false
		}
		index++
		return true
	}

	if chunks := resp.reasoningChunks(); len(chunks) > 0 {
		deltas := make([]map[string]any, 0, len(chunks))
		for _, c := range chunks {
			deltas = append(deltas, map[string]any{"type": "thinking_delta", "thinking": c})
		}
		if !block(map[string]any{"type": "thinking", "thinking": ""}, deltas) {
			return
		}
	}
	if chunks := resp.textChunks(); len(chunks) > 0 {
		deltas := make([]map[string]any, 0, len(chunks))
		for _, c := range chunks {
			deltas = append(deltas, map[string]any{"type": "text_delta", "text": c})
		}
		if !block(map[string]any{"type": "text", "text": ""}, deltas) {
			return
		}
	}
	for _, tc := range resp.ToolCalls {
		// Anthropic always sends id and name whole in content_block_start;
		// only the JSON arguments are fragmented, as partial_json.
		fragments := []string{tc.Arguments}
		if !resp.WholeToolCalls {
			fragments = splitN(tc.Arguments, resp.argumentFragments())
		}
		deltas := make([]map[string]any, 0, len(fragments))
		for _, f := range fragments {
			if f == "" {
				continue
			}
			deltas = append(deltas, map[string]any{"type": "input_json_delta", "partial_json": f})
		}
		if !block(map[string]any{
			"type":  "tool_use",
			"id":    tc.ID,
			"name":  tc.Name,
			"input": map[string]any{},
		}, deltas) {
			return
		}
	}

	if !event("message_delta", map[string]any{
		"delta": map[string]any{
			"stop_reason":   anthropicStopReason(resp.finishReason()),
			"stop_sequence": nil,
		},
		"usage": anthropicUsage{OutputTokens: usage.OutputTokens},
	}) {
		return
	}
	event("message_stop", map[string]any{})
}

// toAnthropicUsage projects provider usage onto Anthropic's field names.
func toAnthropicUsage(u *provider.Usage) anthropicUsage {
	if u == nil {
		return anthropicUsage{}
	}
	return anthropicUsage{
		InputTokens:          u.PromptTokens,
		OutputTokens:         u.CompletionTokens,
		CacheReadInputTokens: u.CachedTokens,
	}
}

// rawInput renders scripted arguments as a JSON value, falling back to an
// empty object so a deliberately malformed argument string still produces a
// syntactically valid non-streaming body.
func rawInput(args string) json.RawMessage {
	if json.Valid([]byte(args)) {
		return json.RawMessage(args)
	}
	return json.RawMessage(`{}`)
}

// ReassembleAnthropicStream folds Anthropic-style SSE events back into one
// logical response, the counterpart to [ReassembleOpenAIStream].
func ReassembleAnthropicStream(frames []SSEFrame) (StreamSummary, error) {
	var (
		sum    StreamSummary
		blocks = map[int]*anthropicBlock{}
		args   = map[int]string{}
		order  []int
	)
	for _, f := range frames {
		var env struct {
			Type         string          `json:"type"`
			Index        int             `json:"index"`
			ContentBlock anthropicBlock  `json:"content_block"`
			Delta        json.RawMessage `json:"delta"`
			Usage        *anthropicUsage `json:"usage"`
			Message      struct {
				Usage *anthropicUsage `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(f.Data), &env); err != nil {
			return sum, fmt.Errorf("fixtures: frame %q is not an Anthropic event: %w", f.Event, err)
		}
		sum.Frames++
		switch env.Type {
		case "message_start":
			if env.Message.Usage != nil {
				sum.Usage = &provider.Usage{PromptTokens: env.Message.Usage.InputTokens}
			}
		case "content_block_start":
			b := env.ContentBlock
			blocks[env.Index] = &b
			order = append(order, env.Index)
		case "content_block_delta":
			var d struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
			}
			if err := json.Unmarshal(env.Delta, &d); err != nil {
				return sum, fmt.Errorf("fixtures: bad content_block_delta: %w", err)
			}
			switch d.Type {
			case "text_delta":
				sum.Text += d.Text
			case "thinking_delta":
				sum.Reasoning += d.Thinking
			case "input_json_delta":
				args[env.Index] += d.PartialJSON
			}
		case "message_delta":
			var d struct {
				StopReason string `json:"stop_reason"`
			}
			if err := json.Unmarshal(env.Delta, &d); err == nil {
				sum.Finish = d.StopReason
			}
			if env.Usage != nil {
				if sum.Usage == nil {
					sum.Usage = &provider.Usage{}
				}
				sum.Usage.CompletionTokens = env.Usage.OutputTokens
				sum.Usage.TotalTokens = sum.Usage.PromptTokens + env.Usage.OutputTokens
			}
		case "message_stop":
			sum.Done = true
		}
	}
	for _, idx := range order {
		b := blocks[idx]
		if b.Type != "tool_use" {
			continue
		}
		sum.ToolCalls = append(sum.ToolCalls, provider.ToolCall{
			ID: b.ID, Name: b.Name, Arguments: args[idx],
		})
	}
	return sum, nil
}

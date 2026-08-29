package anthropic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/boop-dev/boop/internal/provider"
)

// eventBuffer is the channel capacity handed to callers. A small buffer keeps a
// bursty server from stalling on a momentarily busy UI without hiding
// backpressure entirely.
const eventBuffer = 16

// Chat starts a completion and returns the event stream.
//
// The returned error is non-nil only when the request itself is invalid. Every
// failure that happens once the request is on the wire is delivered as a
// terminal EventError, because callers already have to handle that path.
//
// The adapter owns the channel and always closes it, emitting exactly one
// EventDone or EventError.
func (c *Client) Chat(ctx context.Context, req provider.ChatRequest) (<-chan provider.ChatEvent, error) {
	body, err := c.buildRequest(req)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, c.newError(provider.ErrInvalidRequest, req.Model,
			"could not encode chat request", causeDetail(err), 0, err)
	}

	feats := featuresOf(req)
	events := make(chan provider.ChatEvent, eventBuffer)
	if req.Stream {
		go c.runStream(ctx, req.Model, payload, feats, events)
	} else {
		go c.runComplete(ctx, req.Model, payload, feats, events)
	}
	return events, nil
}

// buildRequest translates the neutral request into the Messages API body.
func (c *Client) buildRequest(req provider.ChatRequest) (*messagesRequest, error) {
	if strings.TrimSpace(req.Model) == "" {
		return nil, c.newError(provider.ErrInvalidRequest, "", "no model selected for this request",
			"ChatRequest.Model is empty", 0, nil)
	}
	if len(req.Messages) == 0 {
		return nil, c.newError(provider.ErrInvalidRequest, req.Model, "cannot send an empty conversation",
			"ChatRequest.Messages is empty", 0, nil)
	}

	system, turns := splitSystem(req.Messages)
	if len(turns) == 0 {
		return nil, c.newError(provider.ErrInvalidRequest, req.Model,
			"conversation contains only system instructions",
			"every message was RoleSystem, so the Messages API would receive an empty messages array", 0, nil)
	}

	out := &messagesRequest{
		Model:         req.Model,
		Messages:      turns,
		MaxTokens:     c.resolveMaxTokens(req.MaxTokens),
		System:        system,
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		StopSequences: req.Stop,
		Stream:        req.Stream,
	}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, buildTool(t))
	}
	return out, nil
}

// resolveMaxTokens supplies the required max_tokens field.
//
// The Messages API rejects a request without it, so an unset ChatRequest field
// cannot simply be omitted the way it is in the OpenAI dialect.
func (c *Client) resolveMaxTokens(requested int) int {
	if requested > 0 {
		return requested
	}
	return c.maxTokens
}

// splitSystem lifts RoleSystem messages into the top-level system field and
// converts the remainder into Anthropic turns.
//
// This is the single most common porting bug against this API: a system message
// left in the array is rejected, because "system" is not a valid message role.
// Multiple system messages are joined with a blank line, preserving order.
func splitSystem(messages []provider.Message) (string, []wireMessage) {
	var (
		system []string
		turns  []wireMessage
	)
	for _, m := range messages {
		if m.Role == provider.RoleSystem {
			if text := systemText(m); text != "" {
				system = append(system, text)
			}
			continue
		}
		role, blocks := buildTurn(m)
		if len(blocks) == 0 {
			continue
		}
		// Anthropic requires user and assistant turns to alternate, so
		// consecutive same-role messages — which the neutral history
		// produces routinely, most obviously a run of tool results — are
		// merged into one turn rather than sent as-is and rejected.
		if n := len(turns); n > 0 && turns[n-1].Role == role {
			turns[n-1].Content = append(turns[n-1].Content, blocks...)
			continue
		}
		turns = append(turns, wireMessage{Role: role, Content: blocks})
	}
	return strings.Join(system, "\n\n"), turns
}

// systemText flattens a system message to plain text; the system field carries
// no multimodal content.
func systemText(m provider.Message) string {
	if len(m.Parts) == 0 {
		return m.Content
	}
	var b strings.Builder
	for _, p := range m.Parts {
		if p.Kind == provider.PartText && p.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// buildTurn renders one neutral message as an Anthropic role plus content
// blocks.
//
// A tool result becomes a user turn holding a tool_result block: Anthropic has
// no "tool" role, the result is fed back as user content keyed by tool_use_id.
func buildTurn(m provider.Message) (string, []wireBlock) {
	if m.Role == provider.RoleTool {
		return "user", []wireBlock{{
			Type:      "tool_result",
			ToolUseID: m.ToolCallID,
			Content:   m.Content,
		}}
	}

	role := "user"
	if m.Role == provider.RoleAssistant {
		role = "assistant"
	}

	var blocks []wireBlock
	if len(m.Parts) > 0 {
		blocks = append(blocks, buildParts(m.Parts)...)
	} else if m.Content != "" {
		blocks = append(blocks, wireBlock{Type: "text", Text: m.Content})
	}
	for _, tc := range m.ToolCalls {
		blocks = append(blocks, wireBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Name,
			Input: toolInput(tc.Arguments),
		})
	}
	return role, blocks
}

// toolInput turns the neutral raw-JSON arguments string into the parsed object
// the API expects.
//
// Anthropic carries tool input as a JSON object, not as a string, so an
// unparseable or empty argument set becomes an empty object rather than being
// forwarded as-is and rejected.
func toolInput(arguments string) json.RawMessage {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" {
		return json.RawMessage(`{}`)
	}
	if !json.Valid([]byte(trimmed)) {
		return json.RawMessage(`{}`)
	}
	var probe any
	if err := json.Unmarshal([]byte(trimmed), &probe); err != nil {
		return json.RawMessage(`{}`)
	}
	if _, isObject := probe.(map[string]any); !isObject {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(trimmed)
}

// buildParts renders a multimodal body as Anthropic content blocks.
func buildParts(parts []provider.ContentPart) []wireBlock {
	out := make([]wireBlock, 0, len(parts))
	for _, p := range parts {
		switch p.Kind {
		case provider.PartImage:
			out = append(out, wireBlock{Type: "image", Source: mediaSource(p, "image/png")})
		case provider.PartDocument:
			out = append(out, wireBlock{Type: "document", Source: documentSource(p)})
		default:
			if p.Text != "" {
				out = append(out, wireBlock{Type: "text", Text: p.Text})
			}
		}
	}
	if len(out) == 0 {
		// Never send an empty content array: the API rejects it.
		out = append(out, wireBlock{Type: "text", Text: ""})
	}
	return out
}

// mediaSource builds an image source block.
//
// A part with bytes becomes a base64 source; a part with only text is taken to
// be a URL already, which is how a remote image is passed through without
// re-fetching it.
func mediaSource(p provider.ContentPart, fallbackMIME string) *wireSource {
	if len(p.Data) == 0 {
		return &wireSource{Type: "url", URL: p.Text}
	}
	return &wireSource{
		Type:      "base64",
		MediaType: detectMIME(p, fallbackMIME),
		Data:      base64.StdEncoding.EncodeToString(p.Data),
	}
}

// documentSource builds a document source block.
//
// Anthropic accepts PDFs as base64 and plain text as an inline text source; a
// text document sent as base64 is rejected, so the two are distinguished here.
func documentSource(p provider.ContentPart) *wireSource {
	mime := detectMIME(p, "application/pdf")
	if len(p.Data) == 0 {
		if p.Text != "" && strings.HasPrefix(strings.ToLower(strings.TrimSpace(p.Text)), "http") {
			return &wireSource{Type: "url", URL: p.Text}
		}
		return &wireSource{Type: "text", MediaType: "text/plain", Data: p.Text}
	}
	if strings.HasPrefix(mime, "text/") {
		return &wireSource{Type: "text", MediaType: "text/plain", Data: string(p.Data)}
	}
	return &wireSource{Type: "base64", MediaType: mime, Data: base64.StdEncoding.EncodeToString(p.Data)}
}

// detectMIME resolves the media type of a part, sniffing the bytes when the
// caller did not declare one.
func detectMIME(p provider.ContentPart, fallback string) string {
	mime := strings.TrimSpace(p.MIMEType)
	if mime == "" && len(p.Data) > 0 {
		mime = http.DetectContentType(p.Data)
		if mime == "application/octet-stream" {
			mime = fallback
		}
	}
	if mime == "" {
		mime = fallback
	}
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	return mime
}

// buildTool renders a tool definition.
//
// The schema lives under input_schema, not under function.parameters; a nil
// schema becomes an empty object schema, the valid way to say "no arguments".
func buildTool(t provider.ToolDefinition) wireTool {
	schema := t.Schema
	if schema == nil {
		schema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return wireTool{Name: t.Name, Description: t.Description, InputSchema: schema}
}

// featuresOf records which optional features the request exercises, so a
// rejection can be attributed to the right missing capability.
func featuresOf(req provider.ChatRequest) requestFeatures {
	feats := requestFeatures{Tools: len(req.Tools) > 0}
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			switch p.Kind {
			case provider.PartImage:
				feats.Images = true
			case provider.PartDocument:
				feats.Documents = true
			}
		}
	}
	return feats
}

// runComplete performs a non-streaming completion and replays it as the same
// event sequence a stream produces, so callers need no special case.
func (c *Client) runComplete(ctx context.Context, model string, payload []byte, feats requestFeatures, events chan<- provider.ChatEvent) {
	defer close(events)

	reqCtx, cancel := c.withTimeout(ctx)
	defer cancel()

	req, err := c.newRequest(reqCtx, http.MethodPost, messagesPath, payload, "application/json")
	if err != nil {
		emitError(ctx, events, err)
		return
	}
	resp, err := c.http.Do(req)
	if err != nil {
		emitError(ctx, events, c.transportError(reqCtx, ctx, model, err))
		return
	}
	defer drainAndClose(resp.Body)

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		emitError(ctx, events, c.statusError(resp.StatusCode, model, raw, feats))
		return
	}
	if readErr != nil {
		emitError(ctx, events, c.transportError(reqCtx, ctx, model, readErr))
		return
	}

	var decoded messagesResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		emitError(ctx, events, c.malformedError(model, raw, err))
		return
	}
	if decoded.Error != nil && decoded.Error.Message != "" {
		// An error object inside a 200 response; classify it the same way.
		emitError(ctx, events, c.streamErrorEvent(model, decoded.Error))
		return
	}
	if decoded.Type == "error" {
		emitError(ctx, events, c.malformedError(model, raw, fmt.Errorf("error response without an error object")))
		return
	}

	sawTool := false
	for _, block := range decoded.Content {
		switch block.Type {
		case "text":
			if block.Text == "" {
				continue
			}
			if !emit(ctx, events, provider.ChatEvent{Type: provider.EventDelta, Text: block.Text}) {
				return
			}
		case "thinking":
			if block.Thinking == "" {
				continue
			}
			if !emit(ctx, events, provider.ChatEvent{Type: provider.EventReasoning, Text: block.Thinking}) {
				return
			}
		case "tool_use":
			call := provider.ToolCall{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: rawOrEmptyObject(block.Input),
			}
			if call.Name == "" {
				continue
			}
			if call.ID == "" {
				call.ID = syntheticToolCallID(len(decoded.Content))
			}
			sawTool = true
			if !emit(ctx, events, provider.ChatEvent{Type: provider.EventToolCall, ToolCall: &call}) {
				return
			}
		}
	}
	if u := convertUsage(decoded.Usage); u != nil {
		if !emit(ctx, events, provider.ChatEvent{Type: provider.EventUsage, Usage: u}) {
			return
		}
	}
	emit(ctx, events, provider.ChatEvent{Type: provider.EventDone, Finish: finishReason(decoded.StopReason, sawTool)})
}

// rawOrEmptyObject renders a tool input object as the neutral arguments string.
func rawOrEmptyObject(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	return string(raw)
}

// emit sends ev, aborting if ctx is done. It reports whether the send happened.
func emit(ctx context.Context, events chan<- provider.ChatEvent, ev provider.ChatEvent) bool {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	select {
	case events <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

// emitError sends the terminal error event.
func emitError(ctx context.Context, events chan<- provider.ChatEvent, err error) {
	finish := provider.FinishError
	if cat, ok := provider.CategoryOf(err); ok && cat == provider.ErrCancelled {
		finish = provider.FinishCancelled
	}
	emit(ctx, events, provider.ChatEvent{Type: provider.EventError, Err: err, Finish: finish})
}

// finishReason normalizes an Anthropic stop_reason.
func finishReason(raw string, sawToolCall bool) provider.FinishReason {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "max_tokens":
		return provider.FinishLength
	case "tool_use":
		return provider.FinishToolCalls
	case "end_turn", "stop_sequence", "pause_turn", "refusal":
		// A refusal is a completed turn, not a transport failure: the model
		// answered by declining. Reporting it as an error would send the
		// error-repair loop after something that is not broken.
		if sawToolCall {
			return provider.FinishToolCalls
		}
		return provider.FinishStop
	default:
		if sawToolCall {
			return provider.FinishToolCalls
		}
		return provider.FinishStop
	}
}

// convertUsage maps Anthropic accounting onto the neutral Usage.
//
// Anthropic splits the prompt across three counters: input_tokens holds only
// the uncached remainder, with cache reads and cache writes reported
// separately. PromptTokens is their sum so it means "how big was the prompt",
// and CachedTokens is the read portion, which the neutral contract defines as a
// subset of PromptTokens.
func convertUsage(u *wireUsage) *provider.Usage {
	if u.empty() {
		return nil
	}
	prompt := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
	return &provider.Usage{
		PromptTokens:     prompt,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      prompt + u.OutputTokens,
		CachedTokens:     u.CacheReadInputTokens,
	}
}

// syntheticToolCallID names a tool call for the pathological case of a server
// omitting the id; the tool runtime keys results by id, so an empty one would
// break the round trip.
func syntheticToolCallID(seq int) string {
	return fmt.Sprintf("toolu_%d_%d", time.Now().UnixNano(), seq)
}

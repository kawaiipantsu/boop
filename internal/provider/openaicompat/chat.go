package openaicompat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kawaiipantsu/boop/internal/provider"
)

// eventBuffer is the channel capacity handed to callers. A small buffer keeps a
// bursty server from stalling on a UI that is momentarily busy, without hiding
// backpressure entirely.
const eventBuffer = 16

// Chat starts a completion and returns the event stream.
//
// The returned error is non-nil only when the request itself is invalid — an
// empty model, no messages, an unencodable body. Every failure that happens
// once the request is on the wire is delivered as a terminal EventError,
// because callers already have to handle that path.
//
// The adapter owns the channel and always closes it. Sends are guarded by
// ctx.Done, so an abandoned stream never blocks the producing goroutine.
func (c *Client) Chat(ctx context.Context, req provider.ChatRequest) (<-chan provider.ChatEvent, error) {
	body, err := c.buildChatRequest(req)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, c.wrapError(ctx, provider.ErrInvalidRequest, req.Model, "could not encode chat request", err)
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

// buildChatRequest translates the provider-neutral request into OpenAI JSON.
func (c *Client) buildChatRequest(req provider.ChatRequest) (*chatRequest, error) {
	if strings.TrimSpace(req.Model) == "" {
		return nil, c.newError(provider.ErrInvalidRequest, "", "no model selected for this request", "ChatRequest.Model is empty", 0, nil)
	}
	if len(req.Messages) == 0 {
		return nil, c.newError(provider.ErrInvalidRequest, req.Model, "cannot send an empty conversation", "ChatRequest.Messages is empty", 0, nil)
	}

	out := &chatRequest{
		Model:       req.Model,
		Messages:    make([]chatMessage, 0, len(req.Messages)),
		Temperature: req.Temperature,
		TopP:        req.TopP,
		MaxTokens:   req.MaxTokens,
		Stop:        req.Stop,
		Stream:      req.Stream,
	}
	if req.Stream {
		out.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	for _, m := range req.Messages {
		out.Messages = append(out.Messages, buildMessage(m))
	}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, buildTool(t))
	}
	return out, nil
}

// featuresOf records which optional model features the request exercises, so a
// rejection can be attributed to the right missing capability.
func featuresOf(req provider.ChatRequest) requestFeatures {
	feats := requestFeatures{Tools: len(req.Tools) > 0, Streaming: req.Stream}
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

// buildMessage renders one neutral message onto the wire shape.
func buildMessage(m provider.Message) chatMessage {
	role := string(m.Role)
	if role == "" {
		role = string(provider.RoleUser)
	}
	wm := chatMessage{
		Role:       role,
		Name:       m.Name,
		ToolCallID: m.ToolCallID,
	}
	if len(m.Parts) > 0 {
		wm.Content = buildParts(m.Parts)
	} else if m.Content != "" {
		wm.Content = m.Content
	} else if len(m.ToolCalls) == 0 {
		// An assistant turn that only carries tool calls legitimately has no
		// content; anything else must still send an explicit empty string
		// because several servers reject a missing content field.
		wm.Content = ""
	}
	for _, tc := range m.ToolCalls {
		wm.ToolCalls = append(wm.ToolCalls, wireToolCall{
			ID:       tc.ID,
			Type:     "function",
			Function: wireToolFunction{Name: tc.Name, Arguments: tc.Arguments},
		})
	}
	return wm
}

// buildParts renders a multimodal body as an OpenAI content array.
func buildParts(parts []provider.ContentPart) []any {
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		switch p.Kind {
		case provider.PartImage:
			out = append(out, imagePart{Type: "image_url", ImageURL: imageURLValue{URL: mediaURL(p, "image/png")}})
		case provider.PartDocument:
			out = append(out, filePart{Type: "file", File: fileValue{
				Filename: p.Filename,
				FileData: mediaURL(p, "application/octet-stream"),
			}})
		default:
			if p.Text != "" {
				out = append(out, textPart{Type: "text", Text: p.Text})
			}
		}
	}
	if len(out) == 0 {
		// Never send an empty array: some servers treat it as a protocol error.
		out = append(out, textPart{Type: "text", Text: ""})
	}
	return out
}

// mediaURL builds a base64 data URI for binary part payloads. When a part has
// no bytes but does have text, the text is taken to be a URL already, which is
// how remote images are passed through without re-fetching them.
func mediaURL(p provider.ContentPart, fallbackMIME string) string {
	if len(p.Data) == 0 {
		return p.Text
	}
	mime := strings.TrimSpace(p.MIMEType)
	if mime == "" {
		mime = http.DetectContentType(p.Data)
		if mime == "application/octet-stream" && fallbackMIME != "" {
			mime = fallbackMIME
		}
	}
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(p.Data)
}

// buildTool renders a tool definition. A nil schema becomes an empty object
// schema, which is the OpenAI-valid way to say "no arguments".
func buildTool(t provider.ToolDefinition) chatTool {
	params := t.Schema
	if params == nil {
		params = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return chatTool{
		Type: "function",
		Function: chatFunction{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  params,
		},
	}
}

// runComplete performs a non-streaming completion and replays it as the same
// event sequence a stream would produce, so callers need no special case.
func (c *Client) runComplete(ctx context.Context, model string, payload []byte, feats requestFeatures, events chan<- provider.ChatEvent) {
	defer close(events)

	reqCtx, cancel := c.withTimeout(ctx)
	defer cancel()

	req, err := c.newRequest(reqCtx, http.MethodPost, c.chatPath, payload, "application/json")
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
		emitError(ctx, events, c.statusErrorFor(resp.StatusCode, model, raw, feats))
		return
	}
	if readErr != nil {
		emitError(ctx, events, c.transportError(reqCtx, ctx, model, readErr))
		return
	}

	var decoded chatResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		emitError(ctx, events, c.malformedError(model, raw, err))
		return
	}
	if decoded.Error != nil && decoded.Error.Message != "" {
		emitError(ctx, events, c.newError(provider.ErrServer, model, decoded.Error.Message,
			fmt.Sprintf("error object in a 200 response: type=%s", decoded.Error.Type), 0, nil))
		return
	}
	if len(decoded.Choices) == 0 {
		emitError(ctx, events, c.malformedError(model, raw, fmt.Errorf("response contained no choices")))
		return
	}

	choice := decoded.Choices[0]
	msg := choice.Message
	if msg == nil {
		msg = choice.Delta
	}
	if msg == nil {
		emitError(ctx, events, c.malformedError(model, raw, fmt.Errorf("choice contained no message")))
		return
	}

	if reasoning := msg.reasoningText(); reasoning != "" {
		if !emit(ctx, events, provider.ChatEvent{Type: provider.EventReasoning, Text: reasoning}) {
			return
		}
	}
	if msg.Content.Text != "" {
		if !emit(ctx, events, provider.ChatEvent{Type: provider.EventDelta, Text: msg.Content.Text}) {
			return
		}
	}
	emittedTool := false
	for _, tc := range msg.ToolCalls {
		call := provider.ToolCall{ID: tc.ID}
		if tc.Function != nil {
			call.Name = tc.Function.Name
			call.Arguments = tc.Function.Arguments
		}
		if call.Name == "" {
			continue
		}
		if call.ID == "" {
			call.ID = syntheticToolCallID(len(msg.ToolCalls))
		}
		emittedTool = true
		if !emit(ctx, events, provider.ChatEvent{Type: provider.EventToolCall, ToolCall: &call}) {
			return
		}
	}
	if u := convertUsage(decoded.Usage); u != nil {
		if !emit(ctx, events, provider.ChatEvent{Type: provider.EventUsage, Usage: u}) {
			return
		}
	}
	emit(ctx, events, provider.ChatEvent{Type: provider.EventDone, Finish: finishReason(choice.FinishReason, emittedTool)})
}

// maxResponseBytes caps a non-streaming completion body. 32 MiB is far beyond
// any legitimate completion and keeps a hostile or broken server from
// exhausting memory.
const maxResponseBytes = 32 << 20

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

// finishReason normalizes the server's finish_reason.
func finishReason(raw string, sawToolCall bool) provider.FinishReason {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "stop", "end_turn", "eos":
		if sawToolCall {
			return provider.FinishToolCalls
		}
		return provider.FinishStop
	case "length", "max_tokens", "model_length":
		return provider.FinishLength
	case "tool_calls", "function_call", "tool_use":
		return provider.FinishToolCalls
	case "cancel", "cancelled", "canceled", "abort":
		return provider.FinishCancelled
	case "error", "failed":
		return provider.FinishError
	default:
		if sawToolCall {
			return provider.FinishToolCalls
		}
		return provider.FinishStop
	}
}

// convertUsage maps wire accounting onto the neutral Usage, tolerating the
// alternative field names some gateways use. It returns nil when the server
// reported nothing, so no empty usage event is emitted.
func convertUsage(u *wireUsage) *provider.Usage {
	if u.empty() {
		return nil
	}
	out := &provider.Usage{
		PromptTokens:     firstNonZero(u.PromptTokens, u.InputTokens, u.PromptEvalCount),
		CompletionTokens: firstNonZero(u.CompletionTokens, u.OutputTokens, u.EvalCount),
		TotalTokens:      u.TotalTokens,
	}
	if u.PromptTokensDetails != nil {
		out.CachedTokens = u.PromptTokensDetails.CachedTokens
	}
	if out.TotalTokens == 0 {
		out.TotalTokens = out.PromptTokens + out.CompletionTokens
	}
	return out
}

func firstNonZero(vals ...int) int {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}

// syntheticToolCallID names a tool call for servers that omit ids. The tool
// runtime keys results by id, so an empty one would break the round trip.
func syntheticToolCallID(seq int) string {
	return fmt.Sprintf("call_%d_%d", time.Now().UnixNano(), seq)
}

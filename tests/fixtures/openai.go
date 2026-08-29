package fixtures

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/kawaiipantsu/boop/internal/provider"
)

// ---------------------------------------------------------------------------
// Captured request shapes
// ---------------------------------------------------------------------------

// ChatCompletionRequest is the decoded body of a captured
// POST /v1/chat/completions, exposed so tests can assert on exactly what an
// adapter serialized.
type ChatCompletionRequest struct {
	Model               string        `json:"model"`
	Messages            []ChatMessage `json:"messages"`
	Tools               []ChatTool    `json:"tools,omitempty"`
	ToolChoice          any           `json:"tool_choice,omitempty"`
	Stream              bool          `json:"stream"`
	StreamOptions       *StreamOption `json:"stream_options,omitempty"`
	Temperature         *float64      `json:"temperature,omitempty"`
	TopP                *float64      `json:"top_p,omitempty"`
	MaxTokens           int           `json:"max_tokens,omitempty"`
	MaxCompletionTokens int           `json:"max_completion_tokens,omitempty"`
	Stop                []string      `json:"stop,omitempty"`
}

// StreamOption mirrors OpenAI's stream_options object.
type StreamOption struct {
	IncludeUsage bool `json:"include_usage"`
}

// ChatMessage is one message of a captured request. Content is left raw
// because it is legally either a string or an array of content parts.
type ChatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  []ChatToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// Text flattens Content to plain text, concatenating the text parts of a
// multimodal body and ignoring non-text parts.
func (m ChatMessage) Text() string {
	if len(m.Content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// ChatToolCall is a tool call carried on a captured assistant message.
type ChatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ChatTool is a tool advertised on a captured request.
type ChatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

// ChatRequests returns every captured OpenAI-compatible chat request, decoded.
// Undecodable bodies are skipped, which only happens if a test posts garbage.
func (s *Server) ChatRequests() []ChatCompletionRequest {
	var out []ChatCompletionRequest
	for _, c := range s.Requests() {
		if !strings.HasSuffix(c.Path, "/chat/completions") {
			continue
		}
		var req ChatCompletionRequest
		if err := c.JSON(&req); err != nil {
			continue
		}
		out = append(out, req)
	}
	return out
}

// LastChatRequest returns the most recent decoded chat request.
func (s *Server) LastChatRequest() (ChatCompletionRequest, bool) {
	reqs := s.ChatRequests()
	if len(reqs) == 0 {
		return ChatCompletionRequest{}, false
	}
	return reqs[len(reqs)-1], true
}

// MustLastChatRequest returns the most recent decoded chat request, failing
// the test when none was received.
func (s *Server) MustLastChatRequest(t TB) ChatCompletionRequest {
	t.Helper()
	req, ok := s.LastChatRequest()
	if !ok {
		t.Fatalf("fixtures: no chat/completions request was received")
	}
	return req
}

// ---------------------------------------------------------------------------
// Emitted wire shapes
// ---------------------------------------------------------------------------

type openAIUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
}

type openAIFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type openAIToolCall struct {
	// Index is required on streamed deltas and harmless on complete messages.
	Index    *int            `json:"index,omitempty"`
	ID       string          `json:"id,omitempty"`
	Type     string          `json:"type,omitempty"`
	Function *openAIFunction `json:"function,omitempty"`
}

type openAIDelta struct {
	Role             string           `json:"role,omitempty"`
	Content          *string          `json:"content,omitempty"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	ToolCalls        []openAIToolCall `json:"tool_calls,omitempty"`
}

type openAIChunkChoice struct {
	Index        int         `json:"index"`
	Delta        openAIDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type openAIChunk struct {
	ID                string              `json:"id"`
	Object            string              `json:"object"`
	Created           int64               `json:"created"`
	Model             string              `json:"model"`
	SystemFingerprint string              `json:"system_fingerprint,omitempty"`
	Choices           []openAIChunkChoice `json:"choices"`
	Usage             *openAIUsage        `json:"usage,omitempty"`
}

type openAIMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	Reasoning string           `json:"reasoning_content,omitempty"`
	ToolCalls []openAIToolCall `json:"tool_calls,omitempty"`
}

type openAICompletion struct {
	ID                string `json:"id"`
	Object            string `json:"object"`
	Created           int64  `json:"created"`
	Model             string `json:"model"`
	SystemFingerprint string `json:"system_fingerprint,omitempty"`
	Choices           []struct {
		Index        int           `json:"index"`
		Message      openAIMessage `json:"message"`
		FinishReason string        `json:"finish_reason"`
	} `json:"choices"`
	Usage *openAIUsage `json:"usage,omitempty"`
}

type openAIModelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
	// The remaining fields are extensions local servers add; adapters must
	// tolerate them being absent.
	ContextLength   int      `json:"context_length,omitempty"`
	MaxOutputTokens int      `json:"max_output_tokens,omitempty"`
	Capabilities    []string `json:"capabilities,omitempty"`
}

// openAIErrorBody builds OpenAI's error envelope.
func openAIErrorBody(code, message string) map[string]any {
	return map[string]any{"error": map[string]any{
		"message": message,
		"type":    errorTypeForCode(code),
		"code":    code,
	}}
}

// errorTypeForCode maps a short code onto OpenAI's coarse error type.
func errorTypeForCode(code string) string {
	switch code {
	case "invalid_api_key":
		return "invalid_request_error"
	case "rate_limit_exceeded":
		return "rate_limit_error"
	case "server_error":
		return "server_error"
	default:
		return "invalid_request_error"
	}
}

// errorCodeForStatus picks a plausible code for an injected HTTP status.
func errorCodeForStatus(status int) string {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "invalid_api_key"
	case http.StatusTooManyRequests:
		return "rate_limit_exceeded"
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return "timeout"
	default:
		if status >= 500 {
			return "server_error"
		}
		return "invalid_request_error"
	}
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// handleOpenAIModels serves GET /v1/models and GET /api/v1/models.
func (s *Server) handleOpenAIModels(w http.ResponseWriter, _ *http.Request) {
	models := s.catalogue()
	entries := make([]openAIModelEntry, 0, len(models))
	for _, m := range models {
		entries = append(entries, openAIModelEntry{
			ID:              m.ID,
			Object:          "model",
			Created:         FixedTime.Unix(),
			OwnedBy:         orDefault(m.OwnedBy, "boop"),
			ContextLength:   m.ContextWindow,
			MaxOutputTokens: m.MaxOutput,
			Capabilities:    m.Capabilities,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": entries})
}

// handleChatCompletions serves the OpenAI-compatible chat endpoint, streaming
// or not according to the request, and applies the next scripted response.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	var req ChatCompletionRequest
	body, _ := readBody(r)
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, openAIErrorBody("invalid_request_error",
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
		writeJSON(w, resp.Status, openAIErrorBody(errorCodeForStatus(resp.Status),
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
	id := fmt.Sprintf("chatcmpl-%d", s.nextSeq())
	if req.Stream {
		s.streamOpenAI(w, resp, req, id, model)
		return
	}
	s.completeOpenAI(w, resp, id, model)
}

// completeOpenAI renders the non-streaming JSON body.
func (s *Server) completeOpenAI(w http.ResponseWriter, resp *Response, id, model string) {
	out := openAICompletion{
		ID:                id,
		Object:            "chat.completion",
		Created:           FixedTime.Unix(),
		Model:             model,
		SystemFingerprint: s.fingerprintFor(resp),
		Usage:             toOpenAIUsage(resp.Usage),
	}
	msg := openAIMessage{Role: "assistant", Content: resp.Text, Reasoning: resp.Reasoning}
	for i, tc := range resp.ToolCalls {
		idx := i
		msg.ToolCalls = append(msg.ToolCalls, openAIToolCall{
			Index:    &idx,
			ID:       tc.ID,
			Type:     "function",
			Function: &openAIFunction{Name: tc.Name, Arguments: tc.Arguments},
		})
	}
	out.Choices = append(out.Choices, struct {
		Index        int           `json:"index"`
		Message      openAIMessage `json:"message"`
		FinishReason string        `json:"finish_reason"`
	}{Index: 0, Message: msg, FinishReason: resp.finishReason()})
	writeJSON(w, http.StatusOK, out)
}

// streamOpenAI renders the SSE form: an opening role delta, reasoning and
// text deltas, tool call deltas, a trailing finish_reason frame, a usage
// frame whose choices array is empty, and finally data: [DONE].
func (s *Server) streamOpenAI(w http.ResponseWriter, resp *Response, req ChatCompletionRequest, id, model string) {
	sw, err := newSSEWriter(w, resp)
	if err != nil {
		s.t.Errorf("fixtures: %v", err)
		return
	}
	defer sw.finish()

	fp := s.fingerprintFor(resp)
	emit := func(choices []openAIChunkChoice, usage *openAIUsage) bool {
		chunk := openAIChunk{
			ID:                id,
			Object:            "chat.completion.chunk",
			Created:           FixedTime.Unix(),
			Model:             model,
			SystemFingerprint: fp,
			Choices:           choices,
			Usage:             usage,
		}
		data, err := json.Marshal(chunk)
		if err != nil {
			s.t.Errorf("fixtures: marshal chunk: %v", err)
			return false
		}
		return sw.frame("", string(data))
	}
	delta := func(d openAIDelta) bool {
		return emit([]openAIChunkChoice{{Index: 0, Delta: d}}, nil)
	}

	if !delta(openAIDelta{Role: "assistant", Content: strptr("")}) {
		return
	}
	for _, chunk := range resp.reasoningChunks() {
		if !delta(openAIDelta{ReasoningContent: chunk}) {
			return
		}
	}
	for _, chunk := range resp.textChunks() {
		if !delta(openAIDelta{Content: strptr(chunk)}) {
			return
		}
	}
	for _, tcs := range openAIToolCallDeltas(resp) {
		if !delta(openAIDelta{Role: "assistant", Content: strptr(""), ToolCalls: tcs}) {
			return
		}
	}

	finish := resp.finishReason()
	if !emit([]openAIChunkChoice{{
		Index:        0,
		Delta:        openAIDelta{Role: "assistant", Content: strptr("")},
		FinishReason: &finish,
	}}, nil) {
		return
	}

	if u := toOpenAIUsage(resp.Usage); u != nil && (!s.strictUsage || includeUsage(req)) {
		// Deliberately an empty choices array: real Ollama and OpenAI both do
		// this, and an adapter that indexes Choices[0] unconditionally panics.
		if !emit([]openAIChunkChoice{}, u) {
			return
		}
	}
	sw.frame("", "[DONE]")
}

// includeUsage reports whether the client opted into streamed usage.
func includeUsage(req ChatCompletionRequest) bool {
	return req.StreamOptions != nil && req.StreamOptions.IncludeUsage
}

// openAIToolCallDeltas turns the scripted tool calls into the sequence of
// delta tool_calls payloads to emit, one element per SSE frame.
//
// This is where the harness earns its keep: fragmenting arguments (and
// optionally identity) across frames, and interleaving several calls by
// index, are exactly the cases adapters get wrong.
func openAIToolCallDeltas(resp *Response) [][]openAIToolCall {
	if len(resp.ToolCalls) == 0 {
		return nil
	}
	perCall := make([][][]openAIToolCall, 0, len(resp.ToolCalls))
	for i, tc := range resp.ToolCalls {
		idx := i
		var frames [][]openAIToolCall
		add := func(c openAIToolCall) {
			c.Index = &idx
			frames = append(frames, []openAIToolCall{c})
		}

		switch {
		case resp.WholeToolCalls:
			// Ollama's shape: everything in one delta.
			add(openAIToolCall{
				ID:       tc.ID,
				Type:     "function",
				Function: &openAIFunction{Name: tc.Name, Arguments: tc.Arguments},
			})
		case resp.SplitIdentity:
			idHead, idTail := splitAt(tc.ID, 2)
			nameHead, nameTail := splitAt(tc.Name, 2)
			add(openAIToolCall{ID: idHead, Type: "function", Function: &openAIFunction{Name: nameHead}})
			add(openAIToolCall{ID: idTail, Function: &openAIFunction{Name: nameTail}})
			for _, frag := range splitN(tc.Arguments, resp.argumentFragments()) {
				add(openAIToolCall{Function: &openAIFunction{Arguments: frag}})
			}
		default:
			add(openAIToolCall{ID: tc.ID, Type: "function", Function: &openAIFunction{Name: tc.Name}})
			for _, frag := range splitN(tc.Arguments, resp.argumentFragments()) {
				add(openAIToolCall{Function: &openAIFunction{Arguments: frag}})
			}
		}
		perCall = append(perCall, frames)
	}

	if !resp.InterleaveToolCalls {
		var out [][]openAIToolCall
		for _, frames := range perCall {
			out = append(out, frames...)
		}
		return out
	}
	// Round-robin so a reassembler keyed on anything but the index breaks.
	var out [][]openAIToolCall
	for round := 0; ; round++ {
		emitted := false
		for _, frames := range perCall {
			if round < len(frames) {
				out = append(out, frames[round])
				emitted = true
			}
		}
		if !emitted {
			return out
		}
	}
}

// toOpenAIUsage projects provider usage onto the wire shape.
func toOpenAIUsage(u *provider.Usage) *openAIUsage {
	if u == nil {
		return nil
	}
	out := &openAIUsage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
	}
	if u.CachedTokens > 0 {
		out.PromptTokensDetails = &struct {
			CachedTokens int `json:"cached_tokens"`
		}{CachedTokens: u.CachedTokens}
	}
	return out
}

// ---------------------------------------------------------------------------
// Verification helper
// ---------------------------------------------------------------------------

// StreamSummary is the result of reassembling a streamed response.
type StreamSummary struct {
	Text      string
	Reasoning string
	ToolCalls []provider.ToolCall
	Usage     *provider.Usage
	Finish    string
	// Frames counts payload frames, excluding the [DONE] sentinel.
	Frames int
	// Done reports whether the stream terminated properly: data: [DONE] for
	// OpenAI, message_stop for Anthropic.
	Done bool
}

// ReassembleOpenAIStream folds OpenAI-style SSE frames back into one logical
// response.
//
// It exists so tests can assert the harness emits well-formed, reassemblable
// frames without each test re-implementing accumulation. It is emphatically
// not a substitute for an adapter's own reassembly: assert an adapter against
// its own output, not against this.
func ReassembleOpenAIStream(frames []SSEFrame) (StreamSummary, error) {
	var (
		sum   StreamSummary
		text  strings.Builder
		think strings.Builder
		calls = map[int]*provider.ToolCall{}
		order []int
	)
	for _, f := range frames {
		if f.Data == "[DONE]" {
			sum.Done = true
			continue
		}
		var chunk openAIChunk
		if err := json.Unmarshal([]byte(f.Data), &chunk); err != nil {
			return sum, fmt.Errorf("fixtures: frame %d is not a chat chunk: %w", sum.Frames, err)
		}
		sum.Frames++
		if chunk.Usage != nil {
			sum.Usage = &provider.Usage{
				PromptTokens:     chunk.Usage.PromptTokens,
				CompletionTokens: chunk.Usage.CompletionTokens,
				TotalTokens:      chunk.Usage.TotalTokens,
			}
			if d := chunk.Usage.PromptTokensDetails; d != nil {
				sum.Usage.CachedTokens = d.CachedTokens
			}
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.Content != nil {
				text.WriteString(*ch.Delta.Content)
			}
			think.WriteString(ch.Delta.ReasoningContent)
			if ch.FinishReason != nil && *ch.FinishReason != "" {
				sum.Finish = *ch.FinishReason
			}
			for _, tc := range ch.Delta.ToolCalls {
				idx := 0
				if tc.Index != nil {
					idx = *tc.Index
				}
				cur, ok := calls[idx]
				if !ok {
					cur = &provider.ToolCall{}
					calls[idx] = cur
					order = append(order, idx)
				}
				cur.ID += tc.ID
				if tc.Function != nil {
					cur.Name += tc.Function.Name
					cur.Arguments += tc.Function.Arguments
				}
			}
		}
	}
	sum.Text, sum.Reasoning = text.String(), think.String()
	for _, idx := range order {
		sum.ToolCalls = append(sum.ToolCalls, *calls[idx])
	}
	return sum, nil
}

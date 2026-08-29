package fixtures

import (
	"net/http"
	"time"

	"github.com/boop-dev/boop/internal/provider"
)

// DefaultArgumentFragments is how many pieces a tool call's JSON arguments are
// split into when a [Response] does not say otherwise.
//
// Fragmentation is the default because OpenAI-style servers stream tool call
// arguments a few characters at a time; adapters that forget to accumulate
// them only fail against a fragmenting server.
const DefaultArgumentFragments = 3

// Response is one scripted reply to a chat request.
//
// A Response is protocol-neutral: the same value drives the OpenAI-compatible
// endpoint and the Anthropic-shaped endpoint, and it describes both the
// non-streaming and the streaming rendering. Which one is used depends on what
// the request asked for, not on the Response.
//
// Build one with [TextResponse], [ToolCallResponse], [ErrorResponse] or
// [MalformedResponse] and refine it with the With* methods, which mutate the
// receiver and return it so they chain.
//
// The zero Response is valid and renders an empty assistant message.
type Response struct {
	// Model overrides the model id echoed back. Empty means echo the request.
	Model string

	// Text is the assistant's answer.
	Text string
	// TextChunks, when non-empty, replaces Text for streaming and is emitted
	// verbatim one chunk per frame. Use it to control exactly where the
	// delta boundaries fall (mid-word, mid-UTF8-sequence-free).
	TextChunks []string

	// Reasoning is separately-exposed thinking text. On the OpenAI surface it
	// is emitted as delta.reasoning_content, on the Anthropic surface as a
	// thinking content block.
	Reasoning string
	// ReasoningChunks plays the role of TextChunks for Reasoning.
	ReasoningChunks []string

	// ToolCalls are emitted after the text, in order.
	ToolCalls []provider.ToolCall
	// ArgumentFragments is how many streamed pieces each tool call's
	// arguments are split into. Zero means [DefaultArgumentFragments].
	ArgumentFragments int
	// WholeToolCalls emits each tool call in a single delta carrying id, name
	// and complete arguments together, the way Ollama does. It overrides
	// ArgumentFragments and SplitIdentity.
	WholeToolCalls bool
	// SplitIdentity additionally splits each tool call's id and name across
	// two deltas.
	//
	// No mainstream server does this today, so it is off by default: an
	// adapter that assigns rather than appends the id is correct against real
	// servers. Turn it on only to prove a concatenating reassembler.
	SplitIdentity bool
	// InterleaveToolCalls round-robins the fragments of multiple tool calls
	// instead of finishing one before starting the next, which is what
	// exercises an adapter's use of the delta index.
	InterleaveToolCalls bool

	// Usage, when set, is reported. On the OpenAI surface it arrives in a
	// trailing chunk whose choices array is empty — the shape that makes a
	// naive adapter index Choices[0] and panic.
	Usage *provider.Usage

	// Finish is the finish reason / stop reason. Empty selects "tool_calls"
	// when ToolCalls is non-empty and "stop" otherwise.
	Finish string

	// SystemFingerprint overrides the server-level system_fingerprint on
	// OpenAI-compatible payloads.
	SystemFingerprint string

	// Status, when non-zero, short-circuits rendering and returns this HTTP
	// status with an error body (RawBody if set, otherwise a vendor-shaped
	// error envelope built from Text).
	Status int
	// RawBody, when non-empty, is written verbatim as the whole response
	// body. This is how malformed-JSON and half-a-frame cases are scripted.
	RawBody string
	// Header carries extra response headers, e.g. Retry-After on a 429.
	Header http.Header

	// Delay is applied before any byte of the response is written, which is
	// what makes client-side timeout handling testable.
	Delay time.Duration
	// ChunkDelay is applied before each streamed frame.
	ChunkDelay time.Duration

	// TruncateAfter, when positive, stops a stream cleanly after that many
	// frames: no terminating [DONE], no message_stop. The client sees a
	// well-formed prefix and then EOF.
	TruncateAfter int
	// DropAfter, when positive, hijacks the connection and closes it after
	// that many frames, so the client sees an unexpected EOF mid-body rather
	// than a graceful end.
	DropAfter int
}

// TextResponse scripts a plain assistant answer.
func TextResponse(text string) *Response { return &Response{Text: text} }

// ToolCallResponse scripts an assistant turn that calls tools and says nothing.
func ToolCallResponse(calls ...provider.ToolCall) *Response {
	return &Response{ToolCalls: calls}
}

// ToolCall is a convenience constructor for a scripted tool call.
//
// args must be a JSON object literal; the harness streams it verbatim so tests
// can deliberately script invalid JSON to exercise repair paths.
func ToolCall(id, name, args string) provider.ToolCall {
	return provider.ToolCall{ID: id, Name: name, Arguments: args}
}

// ErrorResponse scripts an HTTP failure, e.g. ErrorResponse(429, "slow down").
func ErrorResponse(status int, message string) *Response {
	return &Response{Status: status, Text: message}
}

// MalformedResponse scripts a 200 whose body is the given garbage, for
// exercising an adapter's malformed-response classification (§57).
func MalformedResponse(body string) *Response { return &Response{RawBody: body} }

// WithModel sets the echoed model id.
func (r *Response) WithModel(model string) *Response { r.Model = model; return r }

// WithText sets the assistant answer.
func (r *Response) WithText(text string) *Response { r.Text = text; return r }

// WithChunks sets explicit streaming chunk boundaries for the answer text.
func (r *Response) WithChunks(chunks ...string) *Response {
	r.TextChunks = chunks
	if r.Text == "" {
		for _, c := range chunks {
			r.Text += c
		}
	}
	return r
}

// WithReasoning sets separately-streamed thinking text.
func (r *Response) WithReasoning(text string, chunks ...string) *Response {
	r.Reasoning = text
	r.ReasoningChunks = chunks
	return r
}

// WithToolCalls appends tool calls to the turn.
func (r *Response) WithToolCalls(calls ...provider.ToolCall) *Response {
	r.ToolCalls = append(r.ToolCalls, calls...)
	return r
}

// WithArgumentFragments sets how many streamed pieces each tool call's
// arguments are split into. One means "arguments arrive whole, but still in
// their own delta"; see [Response.Whole] for Ollama's single-delta shape.
func (r *Response) WithArgumentFragments(n int) *Response {
	r.ArgumentFragments = n
	return r
}

// Whole emits every tool call in one delta carrying id, name and complete
// arguments, which is what Ollama's OpenAI-compatible endpoint really does.
func (r *Response) Whole() *Response { r.WholeToolCalls = true; return r }

// SplitToolCallIdentity spreads each tool call's id and name over two deltas.
func (r *Response) SplitToolCallIdentity() *Response { r.SplitIdentity = true; return r }

// Interleaved round-robins the fragments of multiple tool calls.
func (r *Response) Interleaved() *Response { r.InterleaveToolCalls = true; return r }

// WithUsage reports token accounting. Total is derived.
func (r *Response) WithUsage(prompt, completion int) *Response {
	r.Usage = &provider.Usage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      prompt + completion,
	}
	return r
}

// WithCachedTokens records a prompt-cache hit alongside the usage set by
// [Response.WithUsage].
func (r *Response) WithCachedTokens(cached int) *Response {
	if r.Usage == nil {
		r.Usage = &provider.Usage{}
	}
	r.Usage.CachedTokens = cached
	return r
}

// WithFinish overrides the finish reason ("stop", "length", "tool_calls").
func (r *Response) WithFinish(reason string) *Response { r.Finish = reason; return r }

// WithSystemFingerprint overrides the OpenAI-compatible system_fingerprint.
func (r *Response) WithSystemFingerprint(fp string) *Response {
	r.SystemFingerprint = fp
	return r
}

// WithStatus turns the response into an HTTP failure.
func (r *Response) WithStatus(status int) *Response { r.Status = status; return r }

// WithRawBody replaces the whole body with a verbatim string.
func (r *Response) WithRawBody(body string) *Response { r.RawBody = body; return r }

// WithHeader adds a response header.
func (r *Response) WithHeader(key, value string) *Response {
	if r.Header == nil {
		r.Header = http.Header{}
	}
	r.Header.Add(key, value)
	return r
}

// WithDelay stalls the response before its first byte.
func (r *Response) WithDelay(d time.Duration) *Response { r.Delay = d; return r }

// WithChunkDelay stalls before every streamed frame.
func (r *Response) WithChunkDelay(d time.Duration) *Response { r.ChunkDelay = d; return r }

// TruncateAfterFrames ends the stream after n frames without its terminator.
func (r *Response) TruncateAfterFrames(n int) *Response { r.TruncateAfter = n; return r }

// DropAfterFrames closes the TCP connection after n frames.
func (r *Response) DropAfterFrames(n int) *Response { r.DropAfter = n; return r }

// Clone returns a deep-enough copy to enqueue the same script twice safely.
func (r *Response) Clone() *Response {
	if r == nil {
		return nil
	}
	out := *r
	out.TextChunks = append([]string(nil), r.TextChunks...)
	out.ReasoningChunks = append([]string(nil), r.ReasoningChunks...)
	out.ToolCalls = append([]provider.ToolCall(nil), r.ToolCalls...)
	if r.Usage != nil {
		u := *r.Usage
		out.Usage = &u
	}
	if r.Header != nil {
		out.Header = r.Header.Clone()
	}
	return &out
}

// DefaultResponse is served when the scripted queue is empty.
func DefaultResponse() *Response {
	return TextResponse("ok").WithUsage(10, 2)
}

// finishReason resolves the effective finish reason.
func (r *Response) finishReason() string {
	if r.Finish != "" {
		return r.Finish
	}
	if len(r.ToolCalls) > 0 {
		return "tool_calls"
	}
	return "stop"
}

// textChunks resolves the streamed answer pieces.
func (r *Response) textChunks() []string {
	if len(r.TextChunks) > 0 {
		return r.TextChunks
	}
	if r.Text == "" {
		return nil
	}
	return []string{r.Text}
}

// reasoningChunks resolves the streamed thinking pieces.
func (r *Response) reasoningChunks() []string {
	if len(r.ReasoningChunks) > 0 {
		return r.ReasoningChunks
	}
	if r.Reasoning == "" {
		return nil
	}
	return []string{r.Reasoning}
}

// argumentFragments resolves the fragment count for tool call arguments.
func (r *Response) argumentFragments() int {
	if r.ArgumentFragments > 0 {
		return r.ArgumentFragments
	}
	return DefaultArgumentFragments
}

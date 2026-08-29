package openaicompat

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/kawaiipantsu/boop/internal/provider"
)

// sseDoneToken terminates an OpenAI-style stream.
const sseDoneToken = "[DONE]"

// runStream performs a streaming completion and translates the SSE frames into
// the neutral event sequence.
//
// Invariants this function upholds, because the whole runtime depends on them:
// the channel is always closed; exactly one EventDone or EventError is emitted;
// every send is guarded by ctx.Done so an abandoned caller cannot wedge the
// goroutine; and tool calls are emitted once, fully assembled.
func (c *Client) runStream(ctx context.Context, model string, payload []byte, feats requestFeatures, events chan<- provider.ChatEvent) {
	defer close(events)

	// No client-side deadline is applied here: generation length is not a
	// health signal. The transport's ResponseHeaderTimeout covers a server
	// that accepts the connection and then says nothing.
	req, err := c.newRequest(ctx, http.MethodPost, c.chatPath, payload, "text/event-stream")
	if err != nil {
		emitError(ctx, events, err)
		return
	}
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := c.http.Do(req)
	if err != nil {
		emitError(ctx, events, c.transportError(ctx, ctx, model, err))
		return
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes*16))
		emitError(ctx, events, c.statusErrorFor(resp.StatusCode, model, raw, feats))
		return
	}

	// A server that answers a stream request with plain JSON (some proxies
	// downgrade silently) is handled rather than treated as malformed.
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.Contains(strings.ToLower(ct), "event-stream") {
		if strings.Contains(strings.ToLower(ct), "application/json") {
			c.replayJSONBody(ctx, model, resp.Body, events)
			return
		}
	}

	c.consumeStream(ctx, model, resp.Body, events)
}

// replayJSONBody handles a non-SSE answer to a streaming request by decoding it
// as a normal completion and emitting the same event shape.
func (c *Client) replayJSONBody(ctx context.Context, model string, body io.Reader, events chan<- provider.ChatEvent) {
	raw, err := io.ReadAll(io.LimitReader(body, maxResponseBytes))
	if err != nil {
		emitError(ctx, events, c.transportError(ctx, ctx, model, err))
		return
	}
	var decoded chatResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		emitError(ctx, events, c.malformedError(model, raw, err))
		return
	}
	st := newStreamState()
	if len(decoded.Choices) > 0 {
		choice := decoded.Choices[0]
		msg := choice.Message
		if msg == nil {
			msg = choice.Delta
		}
		if msg != nil {
			if !st.applyMessage(ctx, events, msg) {
				return
			}
		}
		st.noteFinish(choice.FinishReason)
	}
	st.noteUsage(decoded.Usage)
	st.complete(ctx, events)
}

// consumeStream reads SSE frames until [DONE], EOF or failure.
func (c *Client) consumeStream(ctx context.Context, model string, body io.Reader, events chan<- provider.ChatEvent) {
	scanner := newSSEScanner(body)
	st := newStreamState()

	for {
		data, err := scanner.next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if !st.sawChunk {
					emitError(ctx, events, c.malformedError(model, nil,
						fmt.Errorf("stream closed before any data frame was received")))
					return
				}
				// Servers that omit [DONE] are common; a clean EOF after
				// real frames is a normal end of stream.
				st.complete(ctx, events)
				return
			}
			emitError(ctx, events, c.streamReadError(ctx, model, err))
			return
		}
		if data == "" {
			continue
		}
		if strings.TrimSpace(data) == sseDoneToken {
			st.complete(ctx, events)
			return
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			emitError(ctx, events, c.malformedError(model, []byte(data), err))
			return
		}
		st.sawChunk = true

		if chunk.Error != nil && chunk.Error.Message != "" {
			emitError(ctx, events, c.newError(provider.ErrServer, model, chunk.Error.Message,
				fmt.Sprintf("error frame in SSE stream: type=%s", chunk.Error.Type), 0, nil))
			return
		}

		// Usage frequently arrives on a frame with an empty choices array
		// (Ollama, and OpenAI's stream_options final chunk), so it is read
		// independently of the choice loop.
		st.noteUsage(chunk.Usage)

		for i := range chunk.Choices {
			choice := chunk.Choices[i]
			msg := choice.Delta
			if msg == nil {
				msg = choice.Message
			}
			if msg != nil && !st.applyMessage(ctx, events, msg) {
				return
			}
			// finish_reason routinely arrives on its own later frame with an
			// empty delta; it is recorded and only acted on at termination.
			st.noteFinish(choice.FinishReason)
		}
	}
}

// streamReadError classifies a failure while reading the response body.
func (c *Client) streamReadError(ctx context.Context, model string, err error) error {
	if ctx.Err() != nil || errIsAny(err, context.Canceled, context.DeadlineExceeded) {
		return c.transportError(ctx, ctx, model, err)
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return c.newError(provider.ErrMalformedResponse, model,
			fmt.Sprintf("%s closed the stream unexpectedly", c.name), causeDetail(err), 0, err)
	}
	return c.transportError(ctx, ctx, model, err)
}

// toolAccumulator assembles one tool call from its fragments. OpenAI streams
// the id, the name and the arguments in separate pieces; other servers send the
// whole call in a single delta. Concatenation handles both.
type toolAccumulator struct {
	index int
	id    strings.Builder
	name  strings.Builder
	args  strings.Builder
}

// streamState holds everything accumulated across frames.
type streamState struct {
	sawChunk bool
	finished bool

	order   []*toolAccumulator
	byIndex map[int]*toolAccumulator

	usage        *provider.Usage
	finishReason string
}

func newStreamState() *streamState {
	return &streamState{byIndex: make(map[int]*toolAccumulator)}
}

// applyMessage emits the text-bearing parts of a delta and accumulates tool
// call fragments. It reports whether the stream should continue.
func (s *streamState) applyMessage(ctx context.Context, events chan<- provider.ChatEvent, msg *wireMessage) bool {
	if reasoning := msg.reasoningText(); reasoning != "" {
		if !emit(ctx, events, provider.ChatEvent{Type: provider.EventReasoning, Text: reasoning}) {
			return false
		}
	}
	if msg.Content.Text != "" {
		if !emit(ctx, events, provider.ChatEvent{Type: provider.EventDelta, Text: msg.Content.Text}) {
			return false
		}
	}
	for _, frag := range msg.ToolCalls {
		s.accumulateToolCall(frag)
	}
	return true
}

// accumulateToolCall folds one fragment into the call it belongs to.
func (s *streamState) accumulateToolCall(frag deltaToolCall) {
	idx := 0
	if frag.Index != nil {
		idx = *frag.Index
	}

	acc, ok := s.byIndex[idx]
	if ok && frag.Index == nil && frag.ID != "" && acc.id.Len() > 0 {
		// No index field at all and a fresh id: the server is emitting whole
		// tool calls back to back rather than fragments, so start a new one
		// instead of concatenating two distinct calls together.
		ok = false
	}
	if !ok {
		acc = &toolAccumulator{index: idx}
		s.byIndex[idx] = acc
		s.order = append(s.order, acc)
	}

	acc.id.WriteString(frag.ID)
	if frag.Function != nil {
		acc.name.WriteString(frag.Function.Name)
		acc.args.WriteString(frag.Function.Arguments)
	}
}

// noteUsage records the most complete accounting seen so far.
func (s *streamState) noteUsage(u *wireUsage) {
	if converted := convertUsage(u); converted != nil {
		s.usage = converted
	}
}

// noteFinish records the first non-empty finish reason.
func (s *streamState) noteFinish(reason string) {
	if s.finishReason == "" && strings.TrimSpace(reason) != "" {
		s.finishReason = reason
	}
}

// complete flushes tool calls and usage and emits the single terminal EventDone.
func (s *streamState) complete(ctx context.Context, events chan<- provider.ChatEvent) {
	if s.finished {
		return
	}
	s.finished = true

	sawTool := false
	for i, acc := range s.order {
		name := acc.name.String()
		if name == "" {
			// A fragment set that never carried a function name is not a
			// usable call; dropping it is better than inventing one.
			continue
		}
		call := provider.ToolCall{
			ID:        acc.id.String(),
			Name:      name,
			Arguments: acc.args.String(),
		}
		if call.ID == "" {
			call.ID = syntheticToolCallID(i)
		}
		sawTool = true
		if !emit(ctx, events, provider.ChatEvent{Type: provider.EventToolCall, ToolCall: &call}) {
			return
		}
	}
	if s.usage != nil {
		if !emit(ctx, events, provider.ChatEvent{Type: provider.EventUsage, Usage: s.usage}) {
			return
		}
	}
	emit(ctx, events, provider.ChatEvent{Type: provider.EventDone, Finish: finishReason(s.finishReason, sawTool)})
}

// sseScanner reads Server-Sent Events frames.
//
// It implements the parts of the SSE grammar that matter here: comment lines,
// multi-line data fields joined with newlines, and dispatch on a blank line. A
// trailing frame without its blank line is still delivered, because several
// servers close the connection immediately after the last frame.
type sseScanner struct {
	r    *bufio.Reader
	data strings.Builder
	done bool
}

func newSSEScanner(r io.Reader) *sseScanner {
	return &sseScanner{r: bufio.NewReaderSize(r, 32*1024)}
}

// next returns the data payload of the next frame, or io.EOF when the stream
// has ended. An empty string with a nil error means "keep reading".
func (s *sseScanner) next() (string, error) {
	if s.done {
		return "", io.EOF
	}
	for {
		line, err := s.r.ReadString('\n')
		if err != nil {
			s.done = true
			if line != "" {
				s.consumeLine(strings.TrimRight(line, "\r\n"))
			}
			if payload := s.flush(); payload != "" {
				return payload, nil
			}
			if errors.Is(err, io.EOF) {
				return "", io.EOF
			}
			return "", err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if payload := s.flush(); payload != "" {
				return payload, nil
			}
			continue
		}
		s.consumeLine(line)
	}
}

// consumeLine folds one non-blank SSE line into the pending frame.
func (s *sseScanner) consumeLine(line string) {
	if strings.HasPrefix(line, ":") {
		return // comment / keep-alive
	}
	name, value, found := strings.Cut(line, ":")
	if !found {
		// A bare field name has an empty value; nothing to accumulate.
		return
	}
	value = strings.TrimPrefix(value, " ")
	if name != "data" {
		return // event:, id:, retry: carry nothing this client needs
	}
	if s.data.Len() > 0 {
		s.data.WriteByte('\n')
	}
	s.data.WriteString(value)
}

// flush returns and clears the pending frame payload.
func (s *sseScanner) flush() string {
	payload := s.data.String()
	s.data.Reset()
	return payload
}

package anthropic

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

// runStream performs a streaming completion and translates Anthropic's typed
// event stream into the neutral event sequence.
//
// Invariants upheld here, because the whole runtime depends on them: the
// channel is always closed; exactly one EventDone or EventError is emitted;
// every send is guarded by ctx.Done so an abandoned caller cannot wedge the
// goroutine; and each tool call is emitted once, fully assembled.
func (c *Client) runStream(ctx context.Context, model string, payload []byte, feats requestFeatures, events chan<- provider.ChatEvent) {
	defer close(events)

	// No client-side deadline: generation length is not a health signal. The
	// transport's ResponseHeaderTimeout covers a server that accepts the
	// connection and then says nothing.
	req, err := c.newRequest(ctx, http.MethodPost, messagesPath, payload, "text/event-stream")
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
		emitError(ctx, events, c.statusError(resp.StatusCode, model, raw, feats))
		return
	}

	c.consumeStream(ctx, model, resp.Body, events)
}

// blockState accumulates one content block across its start/delta/stop events.
type blockState struct {
	kind     string
	toolID   string
	toolName string
	// args collects input_json_delta fragments. Tool arguments arrive as
	// partial JSON split at arbitrary points, so they are only parseable once
	// the block stops.
	args strings.Builder
	// emitted guards against a duplicate flush when a block is stopped
	// explicitly and again at end of stream.
	emitted bool
}

// streamState holds everything accumulated across the whole message.
type streamState struct {
	blocks   map[int]*blockState
	order    []int
	usage    provider.Usage
	haveUsag bool
	stop     string
	sawTool  bool
	sawEvent bool
	finished bool
}

func newStreamState() *streamState {
	return &streamState{blocks: make(map[int]*blockState)}
}

// consumeStream reads SSE frames until message_stop, an error event, EOF or a
// transport failure.
func (c *Client) consumeStream(ctx context.Context, model string, body io.Reader, events chan<- provider.ChatEvent) {
	scanner := newSSEScanner(body)
	st := newStreamState()

	for {
		data, err := scanner.next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if !st.sawEvent {
					emitError(ctx, events, c.malformedError(model, nil,
						fmt.Errorf("stream closed before any event was received")))
					return
				}
				// A stream that ends without message_stop still produced
				// real content; finish it rather than discarding the turn.
				st.complete(ctx, events)
				return
			}
			emitError(ctx, events, c.streamReadError(ctx, model, err))
			return
		}
		if data == "" {
			continue
		}

		var ev streamEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			emitError(ctx, events, c.malformedError(model, []byte(data), err))
			return
		}
		st.sawEvent = true

		done, ok := c.applyEvent(ctx, model, st, &ev, events)
		if !ok || done {
			return
		}
	}
}

// applyEvent folds one decoded stream event into the state, emitting whatever
// it makes available.
//
// It returns done=true when the stream is over (message_stop or an error
// event) and ok=false when the caller went away mid-send.
func (c *Client) applyEvent(ctx context.Context, model string, st *streamState, ev *streamEvent, events chan<- provider.ChatEvent) (done, ok bool) {
	switch ev.Type {
	case "message_start":
		// Usage arrives split: the prompt half is here, the completion half
		// only on message_delta at the end.
		if ev.Message != nil {
			st.noteInputUsage(ev.Message.Usage)
		}

	case "content_block_start":
		blk := st.block(ev.Index)
		if ev.ContentBlock != nil {
			blk.kind = ev.ContentBlock.Type
			blk.toolID = ev.ContentBlock.ID
			blk.toolName = ev.ContentBlock.Name
			if ev.ContentBlock.Type == "text" && ev.ContentBlock.Text != "" {
				if !emit(ctx, events, provider.ChatEvent{Type: provider.EventDelta, Text: ev.ContentBlock.Text}) {
					return false, false
				}
			}
		}

	case "content_block_delta":
		if ev.Delta == nil {
			break
		}
		blk := st.block(ev.Index)
		switch ev.Delta.Type {
		case "text_delta":
			if ev.Delta.Text == "" {
				break
			}
			if !emit(ctx, events, provider.ChatEvent{Type: provider.EventDelta, Text: ev.Delta.Text}) {
				return false, false
			}
		case "thinking_delta":
			if ev.Delta.Thinking == "" {
				break
			}
			if !emit(ctx, events, provider.ChatEvent{Type: provider.EventReasoning, Text: ev.Delta.Thinking}) {
				return false, false
			}
		case "input_json_delta":
			// Fragments of one tool call's arguments, split at arbitrary
			// points; they are only valid JSON once concatenated.
			blk.args.WriteString(ev.Delta.PartialJSON)
		case "signature_delta":
			// Thinking-block signatures are opaque and carry no user text.
		}

	case "content_block_stop":
		if !st.flushBlock(ctx, ev.Index, events) {
			return false, false
		}

	case "message_delta":
		if ev.Delta != nil && ev.Delta.StopReason != "" {
			st.stop = ev.Delta.StopReason
		}
		st.noteOutputUsage(ev.Usage)

	case "message_stop":
		st.complete(ctx, events)
		return true, true

	case "ping":
		// Keep-alive; nothing to do.

	case "error":
		emitError(ctx, events, c.streamErrorEvent(model, ev.Error))
		return true, true
	}
	return false, true
}

// block returns the accumulator for a content block index, creating it on
// first sight so a delta that arrives without its start event is not lost.
func (s *streamState) block(index int) *blockState {
	blk, ok := s.blocks[index]
	if !ok {
		blk = &blockState{}
		s.blocks[index] = blk
		s.order = append(s.order, index)
	}
	return blk
}

// flushBlock emits a completed tool_use block. Text and thinking blocks were
// already streamed and need no flush.
func (s *streamState) flushBlock(ctx context.Context, index int, events chan<- provider.ChatEvent) bool {
	blk, ok := s.blocks[index]
	if !ok || blk.emitted || blk.kind != "tool_use" || blk.toolName == "" {
		if ok {
			blk.emitted = true
		}
		return true
	}
	blk.emitted = true

	args := strings.TrimSpace(blk.args.String())
	if args == "" || !json.Valid([]byte(args)) {
		// A tool call whose arguments never assembled into valid JSON is
		// still worth delivering: the tool runtime owns schema validation
		// and can report a useful error, whereas dropping the call leaves
		// the model waiting for a result that never comes.
		if args == "" {
			args = "{}"
		}
	}
	call := provider.ToolCall{ID: blk.toolID, Name: blk.toolName, Arguments: args}
	if call.ID == "" {
		call.ID = syntheticToolCallID(index)
	}
	s.sawTool = true
	return emit(ctx, events, provider.ChatEvent{Type: provider.EventToolCall, ToolCall: &call})
}

// noteInputUsage records the prompt half of the accounting.
func (s *streamState) noteInputUsage(u *wireUsage) {
	if u == nil {
		return
	}
	s.usage.PromptTokens = u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
	s.usage.CachedTokens = u.CacheReadInputTokens
	if !u.empty() {
		s.haveUsag = true
	}
	// message_start also carries an initial output_tokens (usually 1); the
	// authoritative value replaces it on message_delta.
	if u.OutputTokens > 0 {
		s.usage.CompletionTokens = u.OutputTokens
	}
}

// noteOutputUsage records the completion half of the accounting.
func (s *streamState) noteOutputUsage(u *wireUsage) {
	if u == nil {
		return
	}
	if u.OutputTokens > 0 {
		s.usage.CompletionTokens = u.OutputTokens
		s.haveUsag = true
	}
	// A message_delta may also restate or extend the prompt counters.
	if prompt := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens; prompt > 0 {
		s.usage.PromptTokens = prompt
		s.usage.CachedTokens = u.CacheReadInputTokens
		s.haveUsag = true
	}
}

// complete flushes any unstopped blocks and emits the single terminal
// EventDone.
func (s *streamState) complete(ctx context.Context, events chan<- provider.ChatEvent) {
	if s.finished {
		return
	}
	s.finished = true

	for _, index := range s.order {
		if !s.flushBlock(ctx, index, events) {
			return
		}
	}
	if s.haveUsag {
		usage := s.usage
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		if !emit(ctx, events, provider.ChatEvent{Type: provider.EventUsage, Usage: &usage}) {
			return
		}
	}
	emit(ctx, events, provider.ChatEvent{Type: provider.EventDone, Finish: finishReason(s.stop, s.sawTool)})
}

// streamReadError classifies a failure while reading the response body.
func (c *Client) streamReadError(ctx context.Context, model string, err error) error {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return c.transportError(ctx, ctx, model, err)
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return c.newError(provider.ErrMalformedResponse, model,
			fmt.Sprintf("%s closed the stream unexpectedly", c.name), causeDetail(err), 0, err)
	}
	return c.transportError(ctx, ctx, model, err)
}

// sseScanner reads Server-Sent Events frames.
//
// Only the data field is collected: Anthropic repeats the event name inside the
// JSON payload's "type" field, so dispatching on the payload keeps the parser
// independent of whether an intermediary preserved the event: lines. A trailing
// frame without its blank line is still delivered, because a server may close
// the connection immediately after the last frame.
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
		return
	}
	if name != "data" {
		return // event:, id:, retry: carry nothing this parser needs
	}
	value = strings.TrimPrefix(value, " ")
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

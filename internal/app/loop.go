package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kawaiipantsu/boop/internal/permissions"
	"github.com/kawaiipantsu/boop/internal/provider"
	"github.com/kawaiipantsu/boop/internal/session"
	"github.com/kawaiipantsu/boop/internal/stats"
	"github.com/kawaiipantsu/boop/internal/tools"
)

// Loop drives one user turn to completion.
//
// It implements the cycle in §13: the model requests tools, each request is
// evaluated by the permission engine, approved calls run, and their structured
// results — including failures — go back to the model so it can diagnose,
// repair, retry and validate. A failed command is data, not a stopping point.
type Loop struct {
	Router    *provider.Router
	Tools     *tools.Registry
	Evaluator *permissions.Evaluator
	Approver  permissions.Approver
	Bus       *Bus
	// Stats aggregates usage. Nil disables recording.
	Stats *stats.Tracker

	// MaxIterations bounds how many times the model may call tools within a
	// single user turn, so a model that keeps calling tools cannot spin
	// forever (§13).
	MaxIterations int
	// SessionID labels emitted events.
	SessionID string
	// Selection pins the provider, model and required capabilities.
	Selection provider.Selection
	// Context bounds what is actually sent. Without one the whole
	// conversation goes to the model every turn, which §47 forbids: a long
	// session eventually exceeds the window and fails on a request that
	// could have succeeded.
	Context *session.ContextManager
	// Selected is the explicitly chosen files and tool output included in
	// every request (§47). It lives here rather than being injected as a
	// pseudo-message by each frontend, which is what happened before.
	Selected *session.Selection
	// MaxRetriesPerCommand bounds how many times the model may repeat an
	// identical failing tool call within one turn. Retrying the same broken
	// command is the most common way a repair loop wastes its budget.
	MaxRetriesPerCommand int

	// failures counts identical failing calls within the current turn.
	failures map[string]int
}

// Turn is the outcome of running one user turn.
type Turn struct {
	// Messages are the assistant and tool messages produced, in order, ready
	// to append to the conversation.
	Messages []provider.Message
	// Text is the assistant's final visible answer.
	Text string
	// Usage totals every model call made during the turn.
	Usage provider.Usage
	// ToolCalls counts tools actually executed.
	ToolCalls int
	// Iterations counts model round trips.
	Iterations int
	// Decision records how the provider was chosen.
	Decision provider.Decision
	// StoppedAtLimit reports that MaxIterations was reached with the model
	// still asking for tools, so the answer may be incomplete.
	StoppedAtLimit bool
}

// ErrDenied is returned when the user refuses a tool the model asked for.
//
// It is not a failure of the run: it is the permission engine working. The
// refusal is reported to the model so it can choose another approach.
var ErrDenied = errors.New("action denied")

// Run executes a full turn against history, which must already include the
// user's message.
func (l *Loop) Run(ctx context.Context, history []provider.Message) (*Turn, error) {
	if l.Router == nil {
		return nil, errors.New("loop: a router is required")
	}
	if l.Tools == nil {
		return nil, errors.New("loop: a tool registry is required")
	}
	maxIter := l.MaxIterations
	if maxIter <= 0 {
		maxIter = 50
	}

	turn := &Turn{}
	conversation := append([]provider.Message(nil), history...)
	l.failures = make(map[string]int)

	for turn.Iterations = 1; turn.Iterations <= maxIter; turn.Iterations++ {
		l.emit(EventModelRequestStarted, map[string]any{"iteration": turn.Iterations})

		req := provider.ChatRequest{
			Messages: l.prompt(ctx, conversation),
			Tools:    l.Tools.Definitions(nil),
			Stream:   true,
		}
		events, decision, err := l.Router.Chat(ctx, l.Selection, req)
		if err != nil {
			return turn, err
		}
		turn.Decision = decision

		assistant, calls, usage, err := l.collect(ctx, events)
		if err != nil {
			return turn, err
		}
		turn.Usage.PromptTokens += usage.PromptTokens
		turn.Usage.CompletionTokens += usage.CompletionTokens
		turn.Usage.TotalTokens += usage.TotalTokens
		l.record(usage, decision)

		conversation = append(conversation, assistant)
		turn.Messages = append(turn.Messages, assistant)
		l.emit(EventModelCompleted, map[string]any{"tool_calls": len(calls)})

		// No tool calls means the model has answered.
		if len(calls) == 0 {
			turn.Text = assistant.Content
			return turn, nil
		}

		for _, call := range calls {
			if over, msg := l.retryExhausted(call); over {
				conversation = append(conversation, provider.Message{
					Role: provider.RoleTool, ToolCallID: call.ID, Name: call.Name, Content: msg,
				})
				turn.Messages = append(turn.Messages, conversation[len(conversation)-1])
				continue
			}
			result := l.Invoke(ctx, call)
			l.recordTool(call.Name, result)
			if result.IsError {
				l.failures[retryKey(call)]++
			}
			turn.ToolCalls++
			msg := provider.Message{
				Role:       provider.RoleTool,
				ToolCallID: call.ID,
				Name:       call.Name,
				Content:    result.Content,
			}
			conversation = append(conversation, msg)
			turn.Messages = append(turn.Messages, msg)
		}
	}

	turn.Iterations = maxIter
	turn.StoppedAtLimit = true
	return turn, nil
}

// prompt bounds the conversation to what should actually be sent.
//
// Without a context manager the full history goes out unchanged, which is
// correct for a short session and fatal for a long one.
func (l *Loop) prompt(ctx context.Context, conversation []provider.Message) []provider.Message {
	if l.Context == nil {
		return conversation
	}
	var system string
	rest := conversation
	if len(rest) > 0 && rest[0].Role == provider.RoleSystem {
		system, rest = rest[0].Content, rest[1:]
	}
	assembly, err := l.Context.Build(ctx, session.Input{
		SystemPrompt: system, History: rest, Selection: l.Selected,
	})
	if err != nil {
		// A budget too small to hold the newest turn is a real problem, but
		// failing the request outright is worse than sending it unbounded and
		// letting the provider report the truth.
		l.emit(EventError, fmt.Sprintf("context assembly failed, sending unbounded: %v", err))
		return conversation
	}
	return assembly.Messages
}

// retryKey identifies a tool call for repeat detection.
func retryKey(call tools.Call) string { return call.Name + "\x00" + string(call.Arguments) }

// collect drains a provider stream into an assistant message and its tool calls.
func (l *Loop) collect(ctx context.Context, events <-chan provider.ChatEvent) (provider.Message, []tools.Call, provider.Usage, error) {
	var text strings.Builder
	var calls []tools.Call
	var usage provider.Usage
	var received, terminated bool
	msg := provider.Message{Role: provider.RoleAssistant}

	for ev := range events {
		received = true
		if ev.Type == provider.EventDone {
			terminated = true
		}
		switch ev.Type {
		case provider.EventDelta:
			text.WriteString(ev.Text)
			l.emit(EventModelToken, ev.Text)
		case provider.EventReasoning:
			l.emit(EventModelReasoning, ev.Text)
		case provider.EventToolCall:
			if ev.ToolCall == nil {
				continue
			}
			msg.ToolCalls = append(msg.ToolCalls, *ev.ToolCall)
			calls = append(calls, tools.Call{
				ID: ev.ToolCall.ID, Name: ev.ToolCall.Name,
				Arguments: []byte(ev.ToolCall.Arguments),
			})
		case provider.EventUsage:
			if ev.Usage != nil {
				usage = *ev.Usage
			}
		case provider.EventError:
			// Drain the rest so the adapter is never left blocked on a
			// channel nobody is reading.
			for range events {
			}
			return msg, nil, usage, ev.Err
		}
		if ctx.Err() != nil {
			return msg, nil, usage, ctx.Err()
		}
	}

	// Check cancellation after the loop as well as inside it. A stream that
	// yields nothing never enters the body, and returning an empty answer as
	// success would silently swallow a cancelled request.
	if err := ctx.Err(); err != nil {
		return msg, nil, usage, err
	}
	if !received {
		return msg, nil, usage, provider.NewError(provider.ErrMalformedResponse,
			"", "the provider produced no events", nil)
	}
	if !terminated && len(calls) == 0 && text.Len() == 0 {
		return msg, nil, usage, provider.NewError(provider.ErrMalformedResponse,
			"", "the stream ended without a completion", nil)
	}

	msg.Content = text.String()
	return msg, calls, usage, nil
}

// Invoke runs one tool call through the permission engine and returns the
// result.
//
// It is exported because frontends need it: a command a user types is the same
// privileged action as one a model requests, and must pass the same gate. When
// this was unexported the TUI reimplemented the classify, evaluate, approve,
// execute sequence, which is a security-relevant sequence to keep two copies
// of.
//
// Every outcome is a Result rather than an error, including a denial: the
// caller is told what happened so it can adapt, which is the whole point of
// returning structured failures (§2.6).
func (l *Loop) Invoke(ctx context.Context, call tools.Call) tools.Result {
	tool, ok := l.Tools.Get(call.Name)
	if !ok {
		return tools.Errorf(call, "no tool named %q is registered; available: %s",
			call.Name, strings.Join(l.Tools.Names(), ", "))
	}

	action, err := tool.Permission(call)
	if err != nil {
		return tools.Errorf(call, "invalid arguments for %s: %v", call.Name, err)
	}
	l.emit(EventToolRequested, action)

	if l.Evaluator != nil {
		switch decision := l.Evaluator.Evaluate(action); decision.Outcome {
		case permissions.OutcomeDeny:
			l.emit(EventApprovalReceived, map[string]any{"tool": call.Name, "approved": false})
			return tools.Errorf(call, "denied: %s", decision.Reason)
		case permissions.OutcomeConfirm:
			if l.Approver == nil {
				return tools.Errorf(call,
					"%s needs your approval but no approver is attached; run in an interactive mode or set execution.mode", call.Name)
			}
			l.emit(EventApprovalRequested, action)
			approved, err := l.Approver.Approve(action)
			l.emit(EventApprovalReceived, map[string]any{"tool": call.Name, "approved": approved})
			if err != nil {
				return tools.Errorf(call, "approval failed: %v", err)
			}
			if !approved {
				return tools.Errorf(call,
					"the user declined this action. Do not retry it; explain what you wanted to do, or propose a different approach.")
			}
		}
	}

	started := time.Now()
	result, err := tool.Execute(ctx, call)
	if err != nil {
		return tools.Errorf(call, "%s failed: %v", call.Name, err)
	}
	if result.Duration == 0 {
		result.Duration = time.Since(started)
	}
	l.emit(EventToolCompleted, map[string]any{
		"tool": call.Name, "error": result.IsError,
		"duration": result.Duration.String(), "display": result.Display,
	})
	return result
}

// retryExhausted reports whether an identical call has already failed too
// often in this turn, and the message to hand back instead of running it.
//
// A model that keeps re-issuing the same broken command burns its whole
// iteration budget on it; telling it plainly that repeating will not help is
// more useful than letting it fail again.
func (l *Loop) retryExhausted(call tools.Call) (bool, string) {
	limit := l.MaxRetriesPerCommand
	if limit <= 0 {
		return false, ""
	}
	if l.failures[retryKey(call)] < limit {
		return false, ""
	}
	return true, fmt.Sprintf(
		"This exact %s call has already failed %d times in this turn and was not run again. "+
			"Repeating it will not help — change the arguments, try a different approach, or "+
			"explain what is blocking you.", call.Name, limit)
}

// record feeds a completed model call into the shared usage tracker.
//
// Without this nothing ever wrote to it, so /stats and /api/stats reported an
// empty tracker no matter how much work had been done.
func (l *Loop) record(usage provider.Usage, decision provider.Decision) {
	if l.Stats == nil {
		return
	}
	scope := stats.Scope{
		SessionID: l.SessionID,
		Provider:  decision.Target.Provider,
		Model:     decision.Target.Model,
	}
	if scope.Provider == "" {
		scope.Provider, scope.Model = l.Selection.Provider, l.Selection.Model
	}
	// Providers do not always report usage; a zero reading is unreported
	// rather than a measurement of zero, and the tracker keeps that
	// distinction rather than filing a guess as fact.
	l.Stats.RecordModelCall(stats.ModelCall{Scope: scope, Usage: usage})
}

// recordTool feeds a completed tool call into the shared usage tracker.
func (l *Loop) recordTool(name string, result tools.Result) {
	if l.Stats == nil {
		return
	}
	l.Stats.RecordToolCall(stats.ToolInvocation{
		Scope:    stats.Scope{SessionID: l.SessionID},
		Tool:     name,
		Duration: result.Duration,
		Failed:   result.IsError,
	})
}

// emit publishes to the bus when one is attached.
func (l *Loop) emit(t EventType, payload any) {
	if l.Bus == nil {
		return
	}
	l.Bus.Emit(t, l.SessionID, payload)
}

// String renders a turn for logs and status output.
func (t *Turn) String() string {
	return fmt.Sprintf("%d iteration(s), %d tool call(s), %d tokens", t.Iterations, t.ToolCalls, t.Usage.TotalTokens)
}

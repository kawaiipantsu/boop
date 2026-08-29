package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kawaiipantsu/boop/internal/permissions"
	"github.com/kawaiipantsu/boop/internal/provider"
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

	// MaxIterations bounds how many times the model may call tools within a
	// single user turn, so a model that keeps calling tools cannot spin
	// forever (§13).
	MaxIterations int
	// SessionID labels emitted events.
	SessionID string
	// Selection pins the provider, model and required capabilities.
	Selection provider.Selection
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

	for turn.Iterations = 1; turn.Iterations <= maxIter; turn.Iterations++ {
		l.emit(EventModelRequestStarted, map[string]any{"iteration": turn.Iterations})

		req := provider.ChatRequest{
			Messages: conversation,
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

		conversation = append(conversation, assistant)
		turn.Messages = append(turn.Messages, assistant)
		l.emit(EventModelCompleted, map[string]any{"tool_calls": len(calls)})

		// No tool calls means the model has answered.
		if len(calls) == 0 {
			turn.Text = assistant.Content
			return turn, nil
		}

		for _, call := range calls {
			result := l.invoke(ctx, call)
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

// collect drains a provider stream into an assistant message and its tool calls.
func (l *Loop) collect(ctx context.Context, events <-chan provider.ChatEvent) (provider.Message, []tools.Call, provider.Usage, error) {
	var text strings.Builder
	var calls []tools.Call
	var usage provider.Usage
	msg := provider.Message{Role: provider.RoleAssistant}

	for ev := range events {
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
	msg.Content = text.String()
	return msg, calls, usage, nil
}

// invoke runs one tool call through the permission engine.
//
// Every outcome is a Result rather than an error, including a denial: the model
// is told what happened so it can adapt, which is the whole point of returning
// structured failures (§2.6).
func (l *Loop) invoke(ctx context.Context, call tools.Call) tools.Result {
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
		"tool": call.Name, "error": result.IsError, "duration": result.Duration.String(),
	})
	return result
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

//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/boop-dev/boop/internal/provider"
)

// toolResult is the structured outcome of a tool invocation.
//
// It mirrors the shape of execution.RunResult (§13) deliberately rather than
// importing it: these tests are about the conversation loop, and keeping the
// dependency at zero means an e2e failure always points at the loop or the
// provider, never at the executor.
type toolResult struct {
	Command   string        `json:"command"`
	ExitCode  int           `json:"exit_code"`
	Stdout    string        `json:"stdout"`
	Stderr    string        `json:"stderr"`
	Duration  time.Duration `json:"duration_ms"`
	TimedOut  bool          `json:"timed_out"`
	Cancelled bool          `json:"cancelled"`
}

// failed reports whether the result should drive the repair loop.
func (r toolResult) failed() bool { return r.ExitCode != 0 || r.TimedOut }

// toolFunc executes one tool call. Returning a failing result is normal: in
// Boop, failure is data fed back to the model, not an aborted run (§2.6).
type toolFunc func(call provider.ToolCall) toolResult

// loopConfig carries the bounds of §13's error-repair loop.
type loopConfig struct {
	// MaxToolIterations bounds how many times the loop will execute tools and
	// go back to the model. Zero means the default of 10.
	MaxToolIterations int
	// RequireToolCapability makes the loop refuse to run against a model that
	// does not advertise tool support (§8) instead of hoping for the best.
	RequireToolCapability bool
}

// loopOutcome is everything a test needs to assert about a completed run.
type loopOutcome struct {
	// FinalText is the assistant's closing answer.
	FinalText string
	// Turns counts model round-trips.
	Turns int
	// Executions records every tool result in execution order.
	Executions []toolResult
	// Deltas records streamed text chunks in arrival order, standing in for
	// what the event bus would publish to a UI (§25).
	Deltas []string
	// Transcript is the full conversation as it was last sent.
	Transcript []provider.Message
}

// errIterationLimit reports that the loop hit its configured bound.
var errIterationLimit = fmt.Errorf("tool iteration limit reached")

// runLoop drives the canonical agentic exchange of §13:
//
//	prompt → model → tool call → execute → structured result → model → …
//
// until the model answers without calling a tool, the iteration bound is hit,
// the provider fails, or the context is cancelled.
func runLoop(
	ctx context.Context,
	p provider.Provider,
	model, prompt string,
	tools []provider.ToolDefinition,
	exec toolFunc,
	cfg loopConfig,
) (loopOutcome, error) {
	if cfg.MaxToolIterations <= 0 {
		cfg.MaxToolIterations = 10
	}

	var out loopOutcome
	if cfg.RequireToolCapability && len(tools) > 0 {
		caps, err := p.Capabilities(ctx, model)
		if err != nil {
			return out, err
		}
		if missing := caps.Missing(provider.CapabilityTools); len(missing) > 0 {
			return out, &provider.UnsupportedCapabilityError{
				Provider: p.Name(), Model: model, Missing: missing,
			}
		}
	}

	messages := []provider.Message{
		{Role: provider.RoleSystem, Content: "You are Boop. Use tools when they help."},
		{Role: provider.RoleUser, Content: prompt},
	}

	for {
		out.Turns++
		events, err := p.Chat(ctx, provider.ChatRequest{
			Model:    model,
			Messages: messages,
			Tools:    tools,
			Stream:   true,
		})
		if err != nil {
			out.Transcript = messages
			return out, err
		}

		var (
			text  string
			calls []provider.ToolCall
		)
		for ev := range events {
			switch ev.Type {
			case provider.EventDelta:
				text += ev.Text
				out.Deltas = append(out.Deltas, ev.Text)
			case provider.EventToolCall:
				if ev.ToolCall != nil {
					calls = append(calls, *ev.ToolCall)
				}
			case provider.EventError:
				out.Transcript = messages
				return out, ev.Err
			}
		}

		messages = append(messages, provider.Message{
			Role:      provider.RoleAssistant,
			Content:   text,
			ToolCalls: calls,
		})

		if len(calls) == 0 {
			out.FinalText = text
			out.Transcript = messages
			return out, nil
		}
		if out.Turns >= cfg.MaxToolIterations {
			out.Transcript = messages
			return out, fmt.Errorf("%w after %d turns", errIterationLimit, out.Turns)
		}

		for _, call := range calls {
			result := exec(call)
			out.Executions = append(out.Executions, result)
			payload, err := json.Marshal(result)
			if err != nil {
				return out, err
			}
			messages = append(messages, provider.Message{
				Role:       provider.RoleTool,
				Name:       call.Name,
				ToolCallID: call.ID,
				Content:    string(payload),
			})
		}
	}
}

// runToolDefinition is the tool advertised throughout the e2e suite.
func runToolDefinition() provider.ToolDefinition {
	return provider.ToolDefinition{
		Name:        "run",
		Description: "Run a shell command and return its structured result",
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"command": map[string]any{"type": "string"}},
			"required":   []any{"command"},
		},
	}
}

// commandOf extracts the command argument from a tool call, tolerating the
// malformed JSON a model can legitimately produce.
func commandOf(call provider.ToolCall) (string, error) {
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
		return "", fmt.Errorf("invalid tool arguments %q: %w", call.Arguments, err)
	}
	return args.Command, nil
}

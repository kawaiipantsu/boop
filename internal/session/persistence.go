package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/boop-dev/boop/internal/execution"
	"github.com/boop-dev/boop/internal/provider"
	"github.com/boop-dev/boop/internal/store"
)

// Entry is one persisted transcript message together with its storage metadata.
//
// Seq is the stable ordering key and doubles as a resumption cursor: a resumed
// session asks for everything after the last sequence number it has seen.
type Entry struct {
	ID        int64            `json:"id"`
	Seq       int64            `json:"seq"`
	SessionID string           `json:"session_id"`
	AgentID   string           `json:"agent_id,omitempty"`
	Message   provider.Message `json:"message"`
	CreatedAt time.Time        `json:"created_at"`
}

// AppendMessage records a conversation turn and returns the stored entry.
func (m *Manager) AppendMessage(ctx context.Context, sessionID string, msg provider.Message) (Entry, error) {
	return m.AppendAgentMessage(ctx, sessionID, "", msg)
}

// AppendAgentMessage records a turn produced by a sub-agent.
//
// Agent turns share the session transcript but are tagged, so agent output can
// be reviewed separately without being replayed into the main conversation
// context (§10, agent context isolation).
func (m *Manager) AppendAgentMessage(ctx context.Context, sessionID, agentID string, msg provider.Message) (Entry, error) {
	rec, err := toMessageRecord(sessionID, agentID, msg, m.now().UTC())
	if err != nil {
		return Entry{}, err
	}
	if err := m.store.AppendMessage(ctx, rec); err != nil {
		return Entry{}, err
	}
	return Entry{
		ID:        rec.ID,
		Seq:       rec.Seq,
		SessionID: rec.SessionID,
		AgentID:   rec.AgentID,
		Message:   msg,
		CreatedAt: rec.CreatedAt,
	}, nil
}

// AppendMessages records several turns in order, stopping at the first failure.
func (m *Manager) AppendMessages(ctx context.Context, sessionID string, msgs ...provider.Message) ([]Entry, error) {
	out := make([]Entry, 0, len(msgs))
	for i, msg := range msgs {
		entry, err := m.AppendMessage(ctx, sessionID, msg)
		if err != nil {
			return out, fmt.Errorf("session: append message %d of %d: %w", i+1, len(msgs), err)
		}
		out = append(out, entry)
	}
	return out, nil
}

// ToolInvocation is a tool call as it is dispatched, before its result is known.
type ToolInvocation struct {
	Call provider.ToolCall
	// AgentID tags calls made by a sub-agent.
	AgentID string
	// MessageID links the call to the assistant message that requested it;
	// zero when the caller does not track it.
	MessageID int64
	// At defaults to now when zero.
	At time.Time
}

// AppendToolCall records that a tool call was requested.
func (m *Manager) AppendToolCall(ctx context.Context, sessionID string, inv ToolInvocation) error {
	if inv.Call.ID == "" {
		return errors.New("session: tool call ID is required")
	}
	at := inv.At
	if at.IsZero() {
		at = m.now().UTC()
	}
	return m.store.AppendToolCall(ctx, &store.ToolCallRecord{
		ID:        inv.Call.ID,
		SessionID: sessionID,
		AgentID:   inv.AgentID,
		MessageID: inv.MessageID,
		Name:      inv.Call.Name,
		Arguments: inv.Call.Arguments,
		CreatedAt: at,
	})
}

// ToolOutcome is the result of a dispatched tool call.
//
// IsError is a property of the result, not an error return: a failed tool is
// still a completed tool whose content goes back to the model (§13).
type ToolOutcome struct {
	CallID   string
	Content  string
	IsError  bool
	Duration time.Duration
	// At defaults to now when zero.
	At time.Time
}

// CompleteToolCall attaches a result to a previously recorded tool call.
func (m *Manager) CompleteToolCall(ctx context.Context, out ToolOutcome) error {
	if out.CallID == "" {
		return errors.New("session: tool call ID is required")
	}
	at := out.At
	if at.IsZero() {
		at = m.now().UTC()
	}
	return m.store.CompleteToolCall(ctx, store.ToolCallResult{
		ID:          out.CallID,
		Result:      out.Content,
		IsError:     out.IsError,
		Duration:    out.Duration,
		CompletedAt: at,
	})
}

// RecordToolCall records an invocation and its outcome in one step, for callers
// that only learn of a tool call once it has already finished.
func (m *Manager) RecordToolCall(ctx context.Context, sessionID string, inv ToolInvocation, out ToolOutcome) error {
	if err := m.AppendToolCall(ctx, sessionID, inv); err != nil {
		return err
	}
	if out.CallID == "" {
		out.CallID = inv.Call.ID
	}
	return m.CompleteToolCall(ctx, out)
}

// ToolCalls returns a session's tool calls, oldest first.
func (m *Manager) ToolCalls(ctx context.Context, sessionID string) ([]store.ToolCallRecord, error) {
	return m.store.ListToolCalls(ctx, sessionID)
}

// ExecutionEntry is a command execution to record.
type ExecutionEntry struct {
	// AgentID tags executions performed by a sub-agent.
	AgentID string
	// ToolCallID links the execution to the tool call that requested it.
	ToolCallID string
	// Result is the structured outcome produced by internal/execution.
	Result execution.RunResult
}

// AppendExecution records a command execution, failures included.
//
// Non-zero exit codes are kept deliberately: they drive the error-repair loop
// and feed the "command failures" statistic (§2.6, §28).
func (m *Manager) AppendExecution(ctx context.Context, sessionID string, entry ExecutionEntry) (int64, error) {
	r := entry.Result
	rec := &store.ExecutionRecord{
		SessionID:       sessionID,
		AgentID:         entry.AgentID,
		ToolCallID:      entry.ToolCallID,
		Command:         r.Command,
		WorkingDir:      r.WorkingDir,
		ExitCode:        r.ExitCode,
		Stdout:          r.Stdout,
		Stderr:          r.Stderr,
		Duration:        r.Duration,
		TimedOut:        r.TimedOut,
		Cancelled:       r.Cancelled,
		Signal:          r.Signal,
		StdoutTruncated: r.StdoutTruncated,
		StderrTruncated: r.StderrTruncated,
		StartedAt:       r.StartedAt,
		CreatedAt:       m.now().UTC(),
	}
	if err := m.store.AppendExecution(ctx, rec); err != nil {
		return 0, err
	}
	return rec.ID, nil
}

// Executions returns a session's command executions, oldest first.
func (m *Manager) Executions(ctx context.Context, sessionID string) ([]store.ExecutionRecord, error) {
	return m.store.ListExecutions(ctx, sessionID)
}

// UsageEntry is provider-reported token accounting for one exchange.
type UsageEntry struct {
	AgentID  string
	Provider string
	Model    string
	Usage    provider.Usage
	// CostUSD is only meaningful when pricing metadata is configured; local
	// providers record zero while still tracking tokens (§28).
	CostUSD float64
	// At defaults to now when zero.
	At time.Time
}

// RecordUsage stores the token accounting a provider reported for an exchange.
//
// These are the authoritative numbers. Any pre-flight estimate from a
// TokenCounter is a planning aid and is never written here.
func (m *Manager) RecordUsage(ctx context.Context, sessionID string, entry UsageEntry) error {
	at := entry.At
	if at.IsZero() {
		at = m.now().UTC()
	}
	return m.store.AppendUsage(ctx, &store.UsageRecord{
		SessionID:        sessionID,
		AgentID:          entry.AgentID,
		Provider:         entry.Provider,
		Model:            entry.Model,
		PromptTokens:     entry.Usage.PromptTokens,
		CompletionTokens: entry.Usage.CompletionTokens,
		TotalTokens:      entry.Usage.TotalTokens,
		CachedTokens:     entry.Usage.CachedTokens,
		CostUSD:          entry.CostUSD,
		CreatedAt:        at,
	})
}

// Usage aggregates a session's recorded token accounting.
func (m *Manager) Usage(ctx context.Context, sessionID string) (store.UsageTotals, error) {
	return m.store.SessionUsage(ctx, sessionID)
}

// AgentRecord is the persisted shape of agent metadata, aliased so the agent
// runtime can record activity through the session manager without opening its
// own store handle.
type AgentRecord = store.AgentRecord

// SaveAgent inserts or updates agent metadata for a session (§10).
func (m *Manager) SaveAgent(ctx context.Context, rec *AgentRecord) error {
	return m.store.SaveAgent(ctx, rec)
}

// Agents lists a session's agents, oldest first.
func (m *Manager) Agents(ctx context.Context, sessionID string) ([]AgentRecord, error) {
	return m.store.ListAgents(ctx, sessionID)
}

// EventEntry is an event to append to the searchable history (§4.7).
//
// It intentionally mirrors rather than imports the app event type: the event
// bus and process lifecycle depend on sessions, so an import in this direction
// would be a cycle.
type EventEntry struct {
	SessionID string
	AgentID   string
	Type      string
	// Payload is JSON-encoded before storage. A payload that cannot be encoded
	// is an error, because a silently dropped payload is worse than a noisy one.
	Payload any
	// At defaults to now when zero.
	At time.Time
}

// RecordEvent appends one event to the persistent history.
func (m *Manager) RecordEvent(ctx context.Context, entry EventEntry) error {
	if entry.Type == "" {
		return errors.New("session: event type is required")
	}
	var payload []byte
	if entry.Payload != nil {
		encoded, err := json.Marshal(entry.Payload)
		if err != nil {
			return fmt.Errorf("session: encode payload for event %q: %w", entry.Type, err)
		}
		payload = encoded
	}
	at := entry.At
	if at.IsZero() {
		at = m.now().UTC()
	}
	return m.store.AppendEvent(ctx, &store.EventRecord{
		SessionID: entry.SessionID,
		AgentID:   entry.AgentID,
		Type:      entry.Type,
		Payload:   payload,
		CreatedAt: at,
	})
}

// EventQuery narrows Events. It is the store's query, aliased for convenience.
type EventQuery = store.EventQuery

// Events returns recorded events, oldest first.
func (m *Manager) Events(ctx context.Context, q EventQuery) ([]store.EventRecord, error) {
	return m.store.ListEvents(ctx, q)
}

// toMessageRecord converts a provider message into its persisted form.
//
// Parts and tool calls are stored as raw JSON so the store stays free of
// provider domain types; the conversion lives here, on the domain side.
func toMessageRecord(sessionID, agentID string, msg provider.Message, at time.Time) (*store.MessageRecord, error) {
	if msg.Role == "" {
		return nil, errors.New("session: message role is required")
	}
	rec := &store.MessageRecord{
		SessionID:  sessionID,
		AgentID:    agentID,
		Role:       string(msg.Role),
		Content:    msg.Content,
		Name:       msg.Name,
		ToolCallID: msg.ToolCallID,
		CreatedAt:  at,
	}
	if len(msg.Parts) > 0 {
		encoded, err := json.Marshal(msg.Parts)
		if err != nil {
			return nil, fmt.Errorf("session: encode message parts: %w", err)
		}
		rec.Parts = encoded
	}
	if len(msg.ToolCalls) > 0 {
		encoded, err := json.Marshal(msg.ToolCalls)
		if err != nil {
			return nil, fmt.Errorf("session: encode tool calls: %w", err)
		}
		rec.ToolCalls = encoded
	}
	return rec, nil
}

// fromMessageRecord rebuilds a provider message from its persisted form.
func fromMessageRecord(rec store.MessageRecord) (Entry, error) {
	msg := provider.Message{
		Role:       provider.Role(rec.Role),
		Content:    rec.Content,
		Name:       rec.Name,
		ToolCallID: rec.ToolCallID,
	}
	if len(rec.Parts) > 0 {
		if err := json.Unmarshal(rec.Parts, &msg.Parts); err != nil {
			return Entry{}, fmt.Errorf("session: decode parts of message %d: %w", rec.ID, err)
		}
	}
	if len(rec.ToolCalls) > 0 {
		if err := json.Unmarshal(rec.ToolCalls, &msg.ToolCalls); err != nil {
			return Entry{}, fmt.Errorf("session: decode tool calls of message %d: %w", rec.ID, err)
		}
	}
	return Entry{
		ID:        rec.ID,
		Seq:       rec.Seq,
		SessionID: rec.SessionID,
		AgentID:   rec.AgentID,
		Message:   msg,
		CreatedAt: rec.CreatedAt,
	}, nil
}

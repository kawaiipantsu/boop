// Package store persists Boop's machine-oriented state in SQLite.
//
// The split between the two memories is an architectural invariant (§64.6):
// `Boop.md` holds compressed, human-readable project knowledge, while this
// package holds structured session state — messages, tool calls, executions,
// agent metadata, events and token usage. Raw transcripts belong here and
// never in `Boop.md`; durable prose belongs there and never here.
//
// Everything is expressed behind the Store interface so tests and any future
// backend are not locked to SQLite. All operations take a context and every
// query is parameterised.
package store

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned when a lookup or an append against a missing parent
// row finds nothing. Callers distinguish it with errors.Is.
var ErrNotFound = errors.New("store: record not found")

// SessionRecord is the persisted header of a conversation (§46).
//
// Provider and Model record the backend in use when the session was last
// saved; the full provider/model history is recoverable from the usage table,
// which stamps every exchange.
type SessionRecord struct {
	ID          string    `json:"id"`
	ProjectPath string    `json:"project_path"`
	Provider    string    `json:"provider"`
	Model       string    `json:"model"`
	Title       string    `json:"title"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// SessionFilter narrows ListSessions. The zero value lists every session,
// newest first.
type SessionFilter struct {
	// ProjectPath restricts results to one project root when non-empty.
	ProjectPath string
	// Since restricts results to sessions updated at or after this instant.
	Since time.Time
	// Limit caps the number of rows; zero means no limit.
	Limit int
	// Offset skips rows for pagination.
	Offset int
	// Oldest reverses the default newest-first ordering.
	Oldest bool
}

// MessageRecord is one persisted conversation turn.
//
// Parts and ToolCalls hold raw JSON rather than decoded provider types so this
// package stays free of provider domain knowledge; internal/session owns the
// conversion in both directions.
type MessageRecord struct {
	// ID is assigned by the store on append.
	ID int64 `json:"id"`
	// Seq is the per-session ordinal, assigned by the store on append. It is
	// dense and monotonic, which makes it a stable cursor for resumption.
	Seq        int64     `json:"seq"`
	SessionID  string    `json:"session_id"`
	AgentID    string    `json:"agent_id,omitempty"`
	Role       string    `json:"role"`
	Content    string    `json:"content,omitempty"`
	Name       string    `json:"name,omitempty"`
	ToolCallID string    `json:"tool_call_id,omitempty"`
	Parts      []byte    `json:"parts,omitempty"`
	ToolCalls  []byte    `json:"tool_calls,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// MessageQuery selects a slice of a session transcript.
type MessageQuery struct {
	SessionID string
	// AgentID restricts results to one agent's sub-transcript when non-empty.
	AgentID string
	// Roles restricts results to the listed roles when non-empty.
	Roles []string
	// AfterSeq returns only messages with a strictly greater sequence number,
	// which is how a resumed session picks up where it left off.
	AfterSeq int64
	// Limit caps the number of rows; zero means no limit.
	Limit int
	// Newest takes the Limit most recent messages instead of the oldest. The
	// result is still ordered oldest-first so it can be handed to a provider
	// unchanged.
	Newest bool
}

// SearchQuery is a case-insensitive substring search over message content.
type SearchQuery struct {
	// Text is matched as a substring; LIKE metacharacters are escaped so the
	// caller never has to think about them.
	Text string
	// SessionID restricts the search to one session when non-empty.
	SessionID string
	// Roles restricts the search to the listed roles when non-empty.
	Roles []string
	// Since and Until bound the creation time when non-zero.
	Since time.Time
	Until time.Time
	// Limit caps the number of rows; zero applies DefaultSearchLimit.
	Limit int
}

// DefaultSearchLimit bounds an unbounded SearchMessages call so a stray query
// cannot pull an entire history into memory.
const DefaultSearchLimit = 200

// ToolCallRecord is a model-requested tool invocation and, once known, its
// outcome. ID is the provider-supplied call identifier and is the primary key,
// so recording a result is an update of the same row.
type ToolCallRecord struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	AgentID   string `json:"agent_id,omitempty"`
	// MessageID links back to the assistant message that requested the call,
	// or zero when it is not known.
	MessageID int64  `json:"message_id,omitempty"`
	Name      string `json:"name"`
	// Arguments is the raw JSON emitted by the model, stored verbatim.
	Arguments string `json:"arguments,omitempty"`
	// Result is the textual content returned to the model.
	Result      string        `json:"result,omitempty"`
	IsError     bool          `json:"is_error"`
	Duration    time.Duration `json:"duration"`
	CreatedAt   time.Time     `json:"created_at"`
	CompletedAt *time.Time    `json:"completed_at,omitempty"`
}

// ToolCallResult records the outcome of a previously appended tool call.
type ToolCallResult struct {
	ID       string
	Result   string
	IsError  bool
	Duration time.Duration
	// CompletedAt defaults to the current time when zero.
	CompletedAt time.Time
}

// ExecutionRecord is a persisted command execution.
//
// A non-zero ExitCode is data, not an error (§13): it is kept so the model can
// diagnose and repair, and so the stats layer can count command failures.
type ExecutionRecord struct {
	ID        int64  `json:"id"`
	SessionID string `json:"session_id"`
	AgentID   string `json:"agent_id,omitempty"`
	// ToolCallID links the execution to the tool call that requested it.
	ToolCallID      string        `json:"tool_call_id,omitempty"`
	Command         string        `json:"command"`
	WorkingDir      string        `json:"working_dir,omitempty"`
	ExitCode        int           `json:"exit_code"`
	Stdout          string        `json:"stdout,omitempty"`
	Stderr          string        `json:"stderr,omitempty"`
	Duration        time.Duration `json:"duration"`
	TimedOut        bool          `json:"timed_out"`
	Cancelled       bool          `json:"cancelled"`
	Signal          string        `json:"signal,omitempty"`
	StdoutTruncated bool          `json:"stdout_truncated,omitempty"`
	StderrTruncated bool          `json:"stderr_truncated,omitempty"`
	StartedAt       time.Time     `json:"started_at"`
	CreatedAt       time.Time     `json:"created_at"`
}

// AgentRecord is the persisted metadata of one agent run (§10).
type AgentRecord struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	ParentID  string `json:"parent_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Task      string `json:"task,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
	Status    string `json:"status"`
	// Error carries the failure reason for an agent that ended in error.
	Error      string     `json:"error,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// EventRecord is one searchable entry of the event history (§4.7).
//
// Payload is the JSON-encoded event payload, matching what the WebUI receives
// over the bus, so replaying history and live streaming look the same.
type EventRecord struct {
	ID        int64     `json:"id"`
	SessionID string    `json:"session_id,omitempty"`
	AgentID   string    `json:"agent_id,omitempty"`
	Type      string    `json:"type"`
	Payload   []byte    `json:"payload,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// EventQuery selects a slice of the event history.
type EventQuery struct {
	SessionID string
	AgentID   string
	// Types restricts results to the listed event types when non-empty.
	Types []string
	Since time.Time
	// Limit caps the number of rows; zero applies DefaultEventLimit.
	Limit int
	// Newest takes the Limit most recent events instead of the oldest, while
	// still returning them oldest-first.
	Newest bool
}

// DefaultEventLimit bounds an unbounded ListEvents call.
const DefaultEventLimit = 500

// UsageRecord is token accounting for a single model exchange (§28).
//
// These are the provider-reported numbers, which are authoritative; any
// pre-flight estimate from a token counter is not persisted here.
type UsageRecord struct {
	ID               int64  `json:"id"`
	SessionID        string `json:"session_id"`
	AgentID          string `json:"agent_id,omitempty"`
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	TotalTokens      int    `json:"total_tokens"`
	CachedTokens     int    `json:"cached_tokens,omitempty"`
	// CostUSD is populated only when pricing metadata is configured and known;
	// local providers normally record zero (§28).
	CostUSD   float64   `json:"cost_usd"`
	CreatedAt time.Time `json:"created_at"`
}

// UsageTotals aggregates UsageRecord rows for a session.
type UsageTotals struct {
	Exchanges        int64   `json:"exchanges"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CachedTokens     int64   `json:"cached_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

// Store is the persistence contract for Boop's structured state.
//
// Append* methods mutate their pointer argument in place with the identifiers
// and timestamps assigned by the backend, so the caller can immediately
// reference the stored row.
type Store interface {
	// Ping verifies the backend is reachable.
	Ping(ctx context.Context) error
	// Close releases the backend. It is safe to call more than once.
	Close() error

	CreateSession(ctx context.Context, rec SessionRecord) error
	UpdateSession(ctx context.Context, rec SessionRecord) error
	GetSession(ctx context.Context, id string) (SessionRecord, error)
	ListSessions(ctx context.Context, filter SessionFilter) ([]SessionRecord, error)
	// DeleteSession removes the session and every row that hangs off it.
	DeleteSession(ctx context.Context, id string) error

	AppendMessage(ctx context.Context, rec *MessageRecord) error
	ListMessages(ctx context.Context, q MessageQuery) ([]MessageRecord, error)
	SearchMessages(ctx context.Context, q SearchQuery) ([]MessageRecord, error)
	CountMessages(ctx context.Context, sessionID string) (int64, error)

	AppendToolCall(ctx context.Context, rec *ToolCallRecord) error
	CompleteToolCall(ctx context.Context, res ToolCallResult) error
	ListToolCalls(ctx context.Context, sessionID string) ([]ToolCallRecord, error)

	AppendExecution(ctx context.Context, rec *ExecutionRecord) error
	ListExecutions(ctx context.Context, sessionID string) ([]ExecutionRecord, error)

	// SaveAgent inserts or updates an agent by ID.
	SaveAgent(ctx context.Context, rec *AgentRecord) error
	GetAgent(ctx context.Context, id string) (AgentRecord, error)
	ListAgents(ctx context.Context, sessionID string) ([]AgentRecord, error)

	AppendEvent(ctx context.Context, rec *EventRecord) error
	ListEvents(ctx context.Context, q EventQuery) ([]EventRecord, error)

	AppendUsage(ctx context.Context, rec *UsageRecord) error
	SessionUsage(ctx context.Context, sessionID string) (UsageTotals, error)
}

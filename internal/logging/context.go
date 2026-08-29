package logging

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
)

// Correlation attribute keys.
//
// These names are fixed because they are the join keys between a log line and
// the event bus (§25): a support question of the form "what happened in this
// session / this agent / this request" has to be answerable with one grep.
const (
	KeySessionID = "session_id"
	KeyAgentID   = "agent_id"
	KeyRequestID = "request_id"
)

// ctxKey is the unexported context key type, so no other package can collide
// with or overwrite the stored logger.
type ctxKey struct{}

// loggerKey identifies the logger stored in a context.
var loggerKey = ctxKey{}

// ContextWithLogger returns a context carrying l.
//
// Passing the logger through context rather than through every signature is
// what makes correlation attributes practical: a session handler attaches
// session_id once, and everything it calls inherits it. A nil logger is
// ignored so the context keeps whatever it already had.
func ContextWithLogger(ctx context.Context, l *slog.Logger) context.Context {
	if l == nil {
		return ctx
	}
	return context.WithValue(ctx, loggerKey, l)
}

// FromContext returns the logger carried by ctx.
//
// When ctx carries none it falls back to slog.Default, which startup sets to
// the configured logger — so code reached without an explicit logger still
// logs to the right file instead of silently dropping records. A nil ctx is
// tolerated because it turns up in tests.
func FromContext(ctx context.Context) *slog.Logger {
	if ctx != nil {
		if l, ok := ctx.Value(loggerKey).(*slog.Logger); ok && l != nil {
			return l
		}
	}
	return slog.Default()
}

// LoggerFrom returns the logger carried by ctx and whether one was present,
// for callers that need to distinguish "no logger" from "the default logger".
func LoggerFrom(ctx context.Context) (*slog.Logger, bool) {
	if ctx == nil {
		return slog.Default(), false
	}
	l, ok := ctx.Value(loggerKey).(*slog.Logger)
	if !ok || l == nil {
		return slog.Default(), false
	}
	return l, true
}

// ContextWithAttrs binds extra attributes to the context's logger.
//
// Values pass through the same redaction as any other attribute, so binding a
// credential here is no more dangerous than logging one directly.
func ContextWithAttrs(ctx context.Context, args ...any) context.Context {
	if len(args) == 0 {
		return ctx
	}
	return ContextWithLogger(ctx, FromContext(ctx).With(args...))
}

// WithSessionID binds session_id to the context's logger.
func WithSessionID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return ContextWithAttrs(ctx, KeySessionID, id)
}

// WithAgentID binds agent_id, so the output of concurrently scheduled agents
// (§11 allows several at once) can be separated after the fact.
func WithAgentID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return ContextWithAttrs(ctx, KeyAgentID, id)
}

// WithRequestID binds request_id, which correlates a provider call or an HTTP
// request to the log lines it produced.
func WithRequestID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return ContextWithAttrs(ctx, KeyRequestID, id)
}

// NewRequestID mints an identifier for a single request or provider call.
func NewRequestID() string { return uuid.NewString() }

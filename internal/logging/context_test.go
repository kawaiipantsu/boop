package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/logging"
)

// decodeRecord parses the single JSON record written to buf.
func decodeRecord(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("output is not JSON (%v): %s", err, buf.String())
	}
	return rec
}

func TestContextRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	lg, err := logging.New(logging.Options{Format: logging.FormatJSON, Writer: &buf})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := logging.ContextWithLogger(context.Background(), lg.Logger)
	if got := logging.FromContext(ctx); got != lg.Logger {
		t.Error("FromContext did not return the stored logger")
	}
	if _, ok := logging.LoggerFrom(ctx); !ok {
		t.Error("LoggerFrom should report that a logger was present")
	}

	// A nil logger must not blank the one already in the context.
	if got := logging.FromContext(logging.ContextWithLogger(ctx, nil)); got != lg.Logger {
		t.Error("ContextWithLogger(nil) removed the existing logger")
	}
}

// unrelatedKey is a context key that is not logging's, used to prove
// FromContext ignores a value stored under someone else's key.
type unrelatedKey struct{}

func TestFromContextFallsBackToDefault(t *testing.T) {
	tests := []struct {
		name string
		ctx  context.Context
	}{
		{name: "background", ctx: context.Background()},
		{name: "nil", ctx: nil},
		{name: "wrong type stored", ctx: context.WithValue(context.Background(), unrelatedKey{}, "not a logger")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := logging.FromContext(tc.ctx)
			if got == nil {
				t.Fatal("FromContext returned nil; callers would panic")
			}
			if got != slog.Default() {
				t.Error("FromContext should fall back to slog.Default")
			}
			if _, ok := logging.LoggerFrom(tc.ctx); ok {
				t.Error("LoggerFrom should report that no logger was present")
			}
		})
	}
}

func TestCorrelationAttributes(t *testing.T) {
	tests := []struct {
		name    string
		attach  func(context.Context) context.Context
		key     string
		want    any
		wantAbs bool // the key must be absent
	}{
		{
			name:   "session id",
			attach: func(ctx context.Context) context.Context { return logging.WithSessionID(ctx, "sess-1") },
			key:    logging.KeySessionID,
			want:   "sess-1",
		},
		{
			name:   "agent id",
			attach: func(ctx context.Context) context.Context { return logging.WithAgentID(ctx, "agent-7") },
			key:    logging.KeyAgentID,
			want:   "agent-7",
		},
		{
			name:   "request id",
			attach: func(ctx context.Context) context.Context { return logging.WithRequestID(ctx, "req-9") },
			key:    logging.KeyRequestID,
			want:   "req-9",
		},
		{
			name:    "empty session id is not attached",
			attach:  func(ctx context.Context) context.Context { return logging.WithSessionID(ctx, "") },
			key:     logging.KeySessionID,
			wantAbs: true,
		},
		{
			name:    "empty agent id is not attached",
			attach:  func(ctx context.Context) context.Context { return logging.WithAgentID(ctx, "") },
			key:     logging.KeyAgentID,
			wantAbs: true,
		},
		{
			name:    "empty request id is not attached",
			attach:  func(ctx context.Context) context.Context { return logging.WithRequestID(ctx, "") },
			key:     logging.KeyRequestID,
			wantAbs: true,
		},
		{
			name: "arbitrary attributes",
			attach: func(ctx context.Context) context.Context {
				return logging.ContextWithAttrs(ctx, "provider", "ollama")
			},
			key:  "provider",
			want: "ollama",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			lg, err := logging.New(logging.Options{Format: logging.FormatJSON, Writer: &buf})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			ctx := tc.attach(logging.ContextWithLogger(context.Background(), lg.Logger))
			logging.FromContext(ctx).Info("event")

			rec := decodeRecord(t, &buf)
			got, present := rec[tc.key]
			if tc.wantAbs {
				if present {
					t.Errorf("%s = %v, want the key to be absent", tc.key, got)
				}
				return
			}
			if got != tc.want {
				t.Errorf("%s = %v, want %v", tc.key, got, tc.want)
			}
		})
	}
}

// TestCorrelationAttributesCompose is the real use: a session context is
// narrowed to an agent and then to a request, and every line carries all three.
func TestCorrelationAttributesCompose(t *testing.T) {
	var buf bytes.Buffer
	lg, err := logging.New(logging.Options{Format: logging.FormatJSON, Writer: &buf})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := logging.ContextWithLogger(context.Background(), lg.Logger)
	sessionCtx := logging.WithSessionID(ctx, "sess-1")
	agentCtx := logging.WithAgentID(sessionCtx, "agent-7")
	reqCtx := logging.WithRequestID(agentCtx, "req-9")

	logging.FromContext(reqCtx).Info("provider call")
	rec := decodeRecord(t, &buf)
	for key, want := range map[string]string{
		logging.KeySessionID: "sess-1",
		logging.KeyAgentID:   "agent-7",
		logging.KeyRequestID: "req-9",
	} {
		if rec[key] != want {
			t.Errorf("%s = %v, want %v", key, rec[key], want)
		}
	}

	// The parent context must not have been mutated.
	buf.Reset()
	logging.FromContext(sessionCtx).Info("session only")
	rec = decodeRecord(t, &buf)
	if _, ok := rec[logging.KeyAgentID]; ok {
		t.Error("narrowing the child context leaked attributes back into the parent")
	}
}

// TestContextAttributesAreRedacted: binding through the context is not a way
// around §45.
func TestContextAttributesAreRedacted(t *testing.T) {
	var buf bytes.Buffer
	lg, err := logging.New(logging.Options{Format: logging.FormatJSON, Writer: &buf})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := logging.ContextWithAttrs(
		logging.ContextWithLogger(context.Background(), lg.Logger),
		"api_key", "anything-at-all",
	)
	logging.FromContext(ctx).Info("call")
	if strings.Contains(buf.String(), "anything-at-all") {
		t.Errorf("context-bound secret was not redacted: %s", buf.String())
	}
}

func TestContextWithAttrsWithoutArgsIsANoOp(t *testing.T) {
	ctx := context.Background()
	if got := logging.ContextWithAttrs(ctx); got != ctx {
		t.Error("ContextWithAttrs with no arguments should return the same context")
	}
}

func TestNewRequestID(t *testing.T) {
	seen := make(map[string]struct{}, 100)
	for i := 0; i < 100; i++ {
		id := logging.NewRequestID()
		if id == "" {
			t.Fatal("NewRequestID returned an empty string")
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("NewRequestID returned a duplicate: %s", id)
		}
		seen[id] = struct{}{}
	}
}

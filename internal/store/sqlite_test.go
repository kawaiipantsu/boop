package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/boop-dev/boop/internal/store"
)

// newMemoryStore opens a private in-memory database for a single test.
func newMemoryStore(t *testing.T) *store.SQLiteStore {
	t.Helper()
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

// seedSession inserts a session and returns its ID.
func seedSession(t *testing.T, s store.Store, id string) string {
	t.Helper()
	rec := store.SessionRecord{ID: id, ProjectPath: "/tmp/project", Provider: "ollama", Model: "qwen3"}
	if err := s.CreateSession(context.Background(), rec); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return id
}

func TestOpenCreatesParentDirectories(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deeper", "boop.db")

	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database file not created: %v", err)
	}
	if got := s.Path(); got != path {
		t.Errorf("Path() = %q, want %q", got, path)
	}
	if err := s.Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	t.Parallel()
	if _, err := store.Open("  "); err == nil {
		t.Fatal("Open(\"\") = nil error, want failure")
	}
}

func TestMigrationsAreIdempotentAcrossOpens(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "boop.db")
	ctx := context.Background()

	var version int64
	for i := range 3 {
		s, err := store.Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		v, err := s.SchemaVersion(ctx)
		if err != nil {
			t.Fatalf("SchemaVersion: %v", err)
		}
		if v == 0 {
			t.Fatalf("open %d: schema version is 0, migrations did not run", i)
		}
		if i > 0 && v != version {
			t.Fatalf("open %d: schema version changed %d -> %d", i, version, v)
		}
		version = v

		var applied int
		if err := s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_version`).Scan(&applied); err != nil {
			t.Fatalf("count schema_version: %v", err)
		}
		if applied != int(version) {
			t.Fatalf("open %d: %d ledger rows for version %d; a migration was applied twice", i, applied, version)
		}
		// Data written on an earlier open must survive later migration runs.
		if i == 0 {
			seedSession(t, s, "survivor")
		} else if _, err := s.GetSession(ctx, "survivor"); err != nil {
			t.Fatalf("open %d: data lost across reopen: %v", i, err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	t.Parallel()
	s, err := store.Open(filepath.Join(t.TempDir(), "boop.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestForeignKeysAndWALAreEnabled(t *testing.T) {
	t.Parallel()
	s, err := store.Open(filepath.Join(t.TempDir(), "boop.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	var fk int
	if err := s.DB().QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1", fk)
	}
	var mode string
	if err := s.DB().QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
	var busy int
	if err := s.DB().QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busy); err != nil {
		t.Fatalf("PRAGMA busy_timeout: %v", err)
	}
	if busy <= 0 {
		t.Errorf("busy_timeout = %d, want > 0", busy)
	}
}

func TestSessionCRUD(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()

	created := time.Date(2026, 1, 2, 3, 4, 5, 600000000, time.UTC)
	rec := store.SessionRecord{
		ID: "s1", ProjectPath: "/srv/app", Provider: "lmstudio", Model: "qwen3-coder",
		Title: "initial", CreatedAt: created, UpdatedAt: created,
	}
	if err := s.CreateSession(ctx, rec); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := s.GetSession(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !got.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, created)
	}
	if got.Title != "initial" || got.Provider != "lmstudio" {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	rec.Title = "renamed"
	rec.UpdatedAt = created.Add(time.Hour)
	if err := s.UpdateSession(ctx, rec); err != nil {
		t.Fatalf("UpdateSession: %v", err)
	}
	got, err = s.GetSession(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSession after update: %v", err)
	}
	if got.Title != "renamed" {
		t.Errorf("Title = %q, want renamed", got.Title)
	}
	if !got.CreatedAt.Equal(created) {
		t.Errorf("UpdateSession rewrote CreatedAt to %v", got.CreatedAt)
	}

	if err := s.DeleteSession(ctx, "s1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := s.GetSession(ctx, "s1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetSession after delete: err = %v, want ErrNotFound", err)
	}
}

func TestSessionMissingRowsReportNotFound(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()

	tests := []struct {
		name string
		call func() error
	}{
		{"get", func() error { _, err := s.GetSession(ctx, "nope"); return err }},
		{"update", func() error { return s.UpdateSession(ctx, store.SessionRecord{ID: "nope"}) }},
		{"delete", func() error { return s.DeleteSession(ctx, "nope") }},
		{"append message", func() error {
			return s.AppendMessage(ctx, &store.MessageRecord{SessionID: "nope", Role: "user"})
		}},
		{"append tool call", func() error {
			return s.AppendToolCall(ctx, &store.ToolCallRecord{ID: "t", SessionID: "nope", Name: "run"})
		}},
		{"append execution", func() error {
			return s.AppendExecution(ctx, &store.ExecutionRecord{SessionID: "nope", Command: "ls"})
		}},
		{"append usage", func() error { return s.AppendUsage(ctx, &store.UsageRecord{SessionID: "nope"}) }},
		{"save agent", func() error {
			return s.SaveAgent(ctx, &store.AgentRecord{ID: "a", SessionID: "nope"})
		}},
		{"complete tool call", func() error { return s.CompleteToolCall(ctx, store.ToolCallResult{ID: "nope"}) }},
		{"get agent", func() error { _, err := s.GetAgent(ctx, "nope"); return err }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestListSessionsFilters(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	for i, spec := range []struct {
		id      string
		project string
	}{
		{"a", "/one"}, {"b", "/two"}, {"c", "/one"},
	} {
		at := base.Add(time.Duration(i) * time.Hour)
		if err := s.CreateSession(ctx, store.SessionRecord{
			ID: spec.id, ProjectPath: spec.project, CreatedAt: at, UpdatedAt: at,
		}); err != nil {
			t.Fatalf("CreateSession %s: %v", spec.id, err)
		}
	}

	tests := []struct {
		name   string
		filter store.SessionFilter
		want   []string
	}{
		{"all newest first", store.SessionFilter{}, []string{"c", "b", "a"}},
		{"oldest first", store.SessionFilter{Oldest: true}, []string{"a", "b", "c"}},
		{"by project", store.SessionFilter{ProjectPath: "/one"}, []string{"c", "a"}},
		{"limit", store.SessionFilter{Limit: 2}, []string{"c", "b"}},
		{"limit and offset", store.SessionFilter{Limit: 2, Offset: 1}, []string{"b", "a"}},
		{"offset only", store.SessionFilter{Offset: 2}, []string{"a"}},
		{"since", store.SessionFilter{Since: base.Add(time.Hour)}, []string{"c", "b"}},
		{"no match", store.SessionFilter{ProjectPath: "/missing"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.ListSessions(ctx, tc.filter)
			if err != nil {
				t.Fatalf("ListSessions: %v", err)
			}
			ids := make([]string, len(got))
			for i, rec := range got {
				ids[i] = rec.ID
			}
			if fmt.Sprint(ids) != fmt.Sprint(tc.want) {
				t.Errorf("ids = %v, want %v", ids, tc.want)
			}
		})
	}
}

func TestAppendMessageRoundTrip(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()
	seedSession(t, s, "s1")

	rec := &store.MessageRecord{
		SessionID:  "s1",
		AgentID:    "agent-1",
		Role:       "assistant",
		Content:    "running the tests",
		Name:       "boop",
		ToolCallID: "call-9",
		Parts:      []byte(`[{"kind":"text","text":"hi"}]`),
		ToolCalls:  []byte(`[{"id":"call-9","name":"run","arguments":"{}"}]`),
	}
	if err := s.AppendMessage(ctx, rec); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if rec.ID == 0 || rec.Seq != 1 {
		t.Fatalf("append did not stamp identifiers: id=%d seq=%d", rec.ID, rec.Seq)
	}
	if rec.CreatedAt.IsZero() {
		t.Error("append did not stamp CreatedAt")
	}

	got, err := s.ListMessages(ctx, store.MessageQuery{SessionID: "s1"})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	m := got[0]
	if m.Content != rec.Content || m.AgentID != rec.AgentID || m.Name != rec.Name || m.ToolCallID != rec.ToolCallID {
		t.Errorf("round-trip mismatch: %+v", m)
	}
	if string(m.Parts) != string(rec.Parts) || string(m.ToolCalls) != string(rec.ToolCalls) {
		t.Errorf("raw JSON columns not preserved: parts=%s tool_calls=%s", m.Parts, m.ToolCalls)
	}

	n, err := s.CountMessages(ctx, "s1")
	if err != nil || n != 1 {
		t.Errorf("CountMessages = %d, %v; want 1, nil", n, err)
	}
}

func TestAppendMessageRejectsIncompleteRecords(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()
	seedSession(t, s, "s1")

	tests := []struct {
		name string
		rec  *store.MessageRecord
	}{
		{"nil", nil},
		{"no session", &store.MessageRecord{Role: "user"}},
		{"no role", &store.MessageRecord{SessionID: "s1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.AppendMessage(ctx, tc.rec); err == nil {
				t.Error("err = nil, want failure")
			}
		})
	}
}

func TestListMessagesQueries(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()
	seedSession(t, s, "s1")

	for i, spec := range []struct{ role, agent, content string }{
		{"user", "", "first"},
		{"assistant", "", "second"},
		{"tool", "worker", "third"},
		{"assistant", "worker", "fourth"},
		{"user", "", "fifth"},
	} {
		rec := &store.MessageRecord{SessionID: "s1", Role: spec.role, AgentID: spec.agent, Content: spec.content}
		if err := s.AppendMessage(ctx, rec); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if rec.Seq != int64(i+1) {
			t.Fatalf("seq = %d, want %d", rec.Seq, i+1)
		}
	}

	tests := []struct {
		name  string
		query store.MessageQuery
		want  []string
	}{
		{"all", store.MessageQuery{SessionID: "s1"}, []string{"first", "second", "third", "fourth", "fifth"}},
		{"after seq", store.MessageQuery{SessionID: "s1", AfterSeq: 3}, []string{"fourth", "fifth"}},
		{"by agent", store.MessageQuery{SessionID: "s1", AgentID: "worker"}, []string{"third", "fourth"}},
		{"by role", store.MessageQuery{SessionID: "s1", Roles: []string{"user"}}, []string{"first", "fifth"}},
		{"multi role", store.MessageQuery{SessionID: "s1", Roles: []string{"user", "tool"}}, []string{"first", "third", "fifth"}},
		{"oldest limit", store.MessageQuery{SessionID: "s1", Limit: 2}, []string{"first", "second"}},
		{"newest limit still oldest-first", store.MessageQuery{SessionID: "s1", Limit: 2, Newest: true}, []string{"fourth", "fifth"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.ListMessages(ctx, tc.query)
			if err != nil {
				t.Fatalf("ListMessages: %v", err)
			}
			contents := make([]string, len(got))
			for i, m := range got {
				contents[i] = m.Content
			}
			if fmt.Sprint(contents) != fmt.Sprint(tc.want) {
				t.Errorf("contents = %v, want %v", contents, tc.want)
			}
		})
	}

	if _, err := s.ListMessages(ctx, store.MessageQuery{}); err == nil {
		t.Error("ListMessages without a session ID: err = nil, want failure")
	}
}

func TestSearchMessages(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()
	seedSession(t, s, "s1")
	seedSession(t, s, "s2")

	base := time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)
	seed := []struct {
		session, role, content string
		at                     time.Time
	}{
		{"s1", "user", "please refactor the Parser", base},
		{"s1", "assistant", "the parser now handles 100% of inputs", base.Add(time.Minute)},
		{"s2", "user", "unrelated parser question", base.Add(2 * time.Minute)},
		{"s1", "user", "snake_case naming", base.Add(3 * time.Minute)},
	}
	for _, spec := range seed {
		if err := s.AppendMessage(ctx, &store.MessageRecord{
			SessionID: spec.session, Role: spec.role, Content: spec.content, CreatedAt: spec.at,
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	tests := []struct {
		name  string
		query store.SearchQuery
		want  int
	}{
		{"case insensitive across sessions", store.SearchQuery{Text: "parser"}, 3},
		{"scoped to session", store.SearchQuery{Text: "parser", SessionID: "s1"}, 2},
		{"role filter", store.SearchQuery{Text: "parser", Roles: []string{"assistant"}}, 1},
		{"percent is literal", store.SearchQuery{Text: "100%"}, 1},
		{"underscore is literal", store.SearchQuery{Text: "snake_case"}, 1},
		{"underscore is not a wildcard", store.SearchQuery{Text: "snake_ase"}, 0},
		{"since", store.SearchQuery{Text: "parser", Since: base.Add(2 * time.Minute)}, 1},
		{"until", store.SearchQuery{Text: "parser", Until: base}, 1},
		{"limit", store.SearchQuery{Text: "parser", Limit: 1}, 1},
		{"empty text matches all", store.SearchQuery{SessionID: "s1"}, 3},
		{"no match", store.SearchQuery{Text: "kubernetes"}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.SearchMessages(ctx, tc.query)
			if err != nil {
				t.Fatalf("SearchMessages: %v", err)
			}
			if len(got) != tc.want {
				t.Errorf("len = %d, want %d (%v)", len(got), tc.want, got)
			}
		})
	}
}

func TestToolCallLifecycle(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()
	seedSession(t, s, "s1")

	msg := &store.MessageRecord{SessionID: "s1", Role: "assistant", Content: "calling run"}
	if err := s.AppendMessage(ctx, msg); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	call := &store.ToolCallRecord{
		ID: "call-1", SessionID: "s1", MessageID: msg.ID, Name: "run",
		Arguments: `{"command":"go test ./..."}`,
	}
	if err := s.AppendToolCall(ctx, call); err != nil {
		t.Fatalf("AppendToolCall: %v", err)
	}

	got, err := s.ListToolCalls(ctx, "s1")
	if err != nil {
		t.Fatalf("ListToolCalls: %v", err)
	}
	if len(got) != 1 || got[0].MessageID != msg.ID || got[0].Arguments != call.Arguments {
		t.Fatalf("unexpected tool calls: %+v", got)
	}
	if got[0].CompletedAt != nil {
		t.Errorf("CompletedAt = %v on a pending call, want nil", got[0].CompletedAt)
	}

	completed := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if err := s.CompleteToolCall(ctx, store.ToolCallResult{
		ID: "call-1", Result: "FAIL", IsError: true, Duration: 1500 * time.Millisecond, CompletedAt: completed,
	}); err != nil {
		t.Fatalf("CompleteToolCall: %v", err)
	}
	got, err = s.ListToolCalls(ctx, "s1")
	if err != nil {
		t.Fatalf("ListToolCalls: %v", err)
	}
	if !got[0].IsError || got[0].Result != "FAIL" || got[0].Duration != 1500*time.Millisecond {
		t.Errorf("result not recorded: %+v", got[0])
	}
	if got[0].CompletedAt == nil || !got[0].CompletedAt.Equal(completed) {
		t.Errorf("CompletedAt = %v, want %v", got[0].CompletedAt, completed)
	}
}

func TestExecutionRoundTrip(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()
	seedSession(t, s, "s1")

	started := time.Date(2026, 7, 7, 8, 9, 10, 0, time.UTC)
	rec := &store.ExecutionRecord{
		SessionID: "s1", ToolCallID: "call-1", Command: "go build ./...", WorkingDir: "/srv/app",
		ExitCode: 2, Stdout: "out", Stderr: "boom", Duration: 3 * time.Second,
		TimedOut: true, Cancelled: false, Signal: "SIGKILL",
		StdoutTruncated: true, StderrTruncated: false, StartedAt: started,
	}
	if err := s.AppendExecution(ctx, rec); err != nil {
		t.Fatalf("AppendExecution: %v", err)
	}
	if rec.ID == 0 {
		t.Fatal("AppendExecution did not stamp an ID")
	}

	got, err := s.ListExecutions(ctx, "s1")
	if err != nil {
		t.Fatalf("ListExecutions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	e := got[0]
	if e.ExitCode != 2 || e.Stderr != "boom" || !e.TimedOut || e.Cancelled || e.Signal != "SIGKILL" {
		t.Errorf("round-trip mismatch: %+v", e)
	}
	if e.Duration != 3*time.Second {
		t.Errorf("Duration = %v, want 3s", e.Duration)
	}
	if !e.StdoutTruncated || e.StderrTruncated {
		t.Errorf("truncation flags mismatch: %+v", e)
	}
	if !e.StartedAt.Equal(started) {
		t.Errorf("StartedAt = %v, want %v", e.StartedAt, started)
	}
}

func TestSaveAgentUpserts(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()
	seedSession(t, s, "s1")

	started := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
	rec := &store.AgentRecord{
		ID: "a1", SessionID: "s1", Name: "tester", Task: "run the suite",
		Provider: "ollama", Model: "qwen3", Status: "working", StartedAt: &started,
	}
	if err := s.SaveAgent(ctx, rec); err != nil {
		t.Fatalf("SaveAgent: %v", err)
	}

	finished := started.Add(2 * time.Minute)
	rec.Status = "complete"
	rec.FinishedAt = &finished
	if err := s.SaveAgent(ctx, rec); err != nil {
		t.Fatalf("SaveAgent update: %v", err)
	}

	agents, err := s.ListAgents(ctx, "s1")
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("upsert created %d rows, want 1", len(agents))
	}
	got := agents[0]
	if got.Status != "complete" {
		t.Errorf("Status = %q, want complete", got.Status)
	}
	if got.FinishedAt == nil || !got.FinishedAt.Equal(finished) {
		t.Errorf("FinishedAt = %v, want %v", got.FinishedAt, finished)
	}
	if got.StartedAt == nil || !got.StartedAt.Equal(started) {
		t.Errorf("StartedAt = %v, want %v", got.StartedAt, started)
	}

	one, err := s.GetAgent(ctx, "a1")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if one.Task != "run the suite" {
		t.Errorf("Task = %q", one.Task)
	}
}

func TestEventsAreRecordableWithoutASession(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()

	rec := &store.EventRecord{Type: "error", Payload: []byte(`{"message":"disk full"}`)}
	if err := s.AppendEvent(ctx, rec); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if rec.ID == 0 {
		t.Fatal("AppendEvent did not stamp an ID")
	}
	got, err := s.ListEvents(ctx, store.EventQuery{})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(got) != 1 || string(got[0].Payload) != `{"message":"disk full"}` {
		t.Fatalf("unexpected events: %+v", got)
	}
	if err := s.AppendEvent(ctx, &store.EventRecord{}); err == nil {
		t.Error("AppendEvent without a type: err = nil, want failure")
	}
}

func TestListEventsQueries(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()
	seedSession(t, s, "s1")

	base := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	for i, spec := range []struct{ sess, agent, typ string }{
		{"s1", "", "session.started"},
		{"s1", "a1", "tool.requested"},
		{"", "", "error"},
		{"s1", "a1", "tool.completed"},
	} {
		if err := s.AppendEvent(ctx, &store.EventRecord{
			SessionID: spec.sess, AgentID: spec.agent, Type: spec.typ,
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("append event %d: %v", i, err)
		}
	}

	tests := []struct {
		name  string
		query store.EventQuery
		want  []string
	}{
		{"all", store.EventQuery{}, []string{"session.started", "tool.requested", "error", "tool.completed"}},
		{"by session", store.EventQuery{SessionID: "s1"}, []string{"session.started", "tool.requested", "tool.completed"}},
		{"by agent", store.EventQuery{AgentID: "a1"}, []string{"tool.requested", "tool.completed"}},
		{"by type", store.EventQuery{Types: []string{"error", "tool.completed"}}, []string{"error", "tool.completed"}},
		{"since", store.EventQuery{Since: base.Add(2 * time.Minute)}, []string{"error", "tool.completed"}},
		{"newest still oldest-first", store.EventQuery{Newest: true, Limit: 2}, []string{"error", "tool.completed"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.ListEvents(ctx, tc.query)
			if err != nil {
				t.Fatalf("ListEvents: %v", err)
			}
			types := make([]string, len(got))
			for i, e := range got {
				types[i] = e.Type
			}
			if fmt.Sprint(types) != fmt.Sprint(tc.want) {
				t.Errorf("types = %v, want %v", types, tc.want)
			}
		})
	}
}

func TestUsageAggregation(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()
	seedSession(t, s, "s1")

	if err := s.AppendUsage(ctx, &store.UsageRecord{
		SessionID: "s1", Provider: "ollama", Model: "qwen3", PromptTokens: 100, CompletionTokens: 50,
	}); err != nil {
		t.Fatalf("AppendUsage: %v", err)
	}
	if err := s.AppendUsage(ctx, &store.UsageRecord{
		SessionID: "s1", Provider: "openai", Model: "gpt", PromptTokens: 10, CompletionTokens: 5,
		CachedTokens: 4, TotalTokens: 15, CostUSD: 0.25,
	}); err != nil {
		t.Fatalf("AppendUsage: %v", err)
	}

	totals, err := s.SessionUsage(ctx, "s1")
	if err != nil {
		t.Fatalf("SessionUsage: %v", err)
	}
	want := store.UsageTotals{
		Exchanges: 2, PromptTokens: 110, CompletionTokens: 55, TotalTokens: 165, CachedTokens: 4, CostUSD: 0.25,
	}
	if totals != want {
		t.Errorf("totals = %+v, want %+v", totals, want)
	}

	empty, err := s.SessionUsage(ctx, "unknown")
	if err != nil {
		t.Fatalf("SessionUsage(unknown): %v", err)
	}
	if empty != (store.UsageTotals{}) {
		t.Errorf("totals for unknown session = %+v, want zero", empty)
	}
}

func TestDeleteSessionRemovesEverything(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()
	seedSession(t, s, "s1")
	seedSession(t, s, "keep")

	if err := s.AppendMessage(ctx, &store.MessageRecord{SessionID: "s1", Role: "user", Content: "hi"}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if err := s.AppendToolCall(ctx, &store.ToolCallRecord{ID: "c1", SessionID: "s1", Name: "run"}); err != nil {
		t.Fatalf("AppendToolCall: %v", err)
	}
	if err := s.AppendExecution(ctx, &store.ExecutionRecord{SessionID: "s1", Command: "ls"}); err != nil {
		t.Fatalf("AppendExecution: %v", err)
	}
	if err := s.SaveAgent(ctx, &store.AgentRecord{ID: "a1", SessionID: "s1", Status: "idle"}); err != nil {
		t.Fatalf("SaveAgent: %v", err)
	}
	if err := s.AppendUsage(ctx, &store.UsageRecord{SessionID: "s1", PromptTokens: 1}); err != nil {
		t.Fatalf("AppendUsage: %v", err)
	}
	if err := s.AppendEvent(ctx, &store.EventRecord{SessionID: "s1", Type: "tool.completed"}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if err := s.AppendEvent(ctx, &store.EventRecord{SessionID: "keep", Type: "tool.completed"}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	if err := s.DeleteSession(ctx, "s1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	for _, table := range []string{"messages", "tool_calls", "executions", "agents", "usage", "events"} {
		var n int
		if err := s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE session_id = ?`, "s1").Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s still holds %d rows for the deleted session", table, n)
		}
	}
	var kept int
	if err := s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE session_id = ?`, "keep").Scan(&kept); err != nil {
		t.Fatalf("count kept events: %v", err)
	}
	if kept != 1 {
		t.Errorf("events for other sessions were deleted: %d remain, want 1", kept)
	}
}

func TestConcurrentWritesFromManyGoroutines(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "boop.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	seedSession(t, s, "s1")

	const (
		writers   = 8
		perWriter = 15
	)
	var wg sync.WaitGroup
	errCh := make(chan error, writers*perWriter)
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range perWriter {
				if err := s.AppendMessage(ctx, &store.MessageRecord{
					SessionID: "s1", Role: "user", Content: fmt.Sprintf("w%d-%d", w, i),
				}); err != nil {
					errCh <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent append: %v", err)
	}

	got, err := s.ListMessages(ctx, store.MessageQuery{SessionID: "s1"})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(got) != writers*perWriter {
		t.Fatalf("len = %d, want %d", len(got), writers*perWriter)
	}
	// Sequence numbers must be dense and unique: they are the transcript's
	// ordering key and a resumption cursor.
	for i, m := range got {
		if m.Seq != int64(i+1) {
			t.Fatalf("message %d has seq %d, want %d", i, m.Seq, i+1)
		}
	}
}

func TestConcurrentOpenersShareOneDatabase(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "boop.db")

	first, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open first: %v", err)
	}
	defer first.Close()
	seedSession(t, first, "s1")

	second, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open second: %v", err)
	}
	defer second.Close()

	ctx := context.Background()
	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	for _, s := range []*store.SQLiteStore{first, second} {
		wg.Add(1)
		go func(s *store.SQLiteStore) {
			defer wg.Done()
			for i := range 10 {
				if err := s.AppendMessage(ctx, &store.MessageRecord{
					SessionID: "s1", Role: "user", Content: fmt.Sprint(i),
				}); err != nil {
					errCh <- err
					return
				}
			}
		}(s)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("cross-handle append: %v", err)
	}

	n, err := first.CountMessages(ctx, "s1")
	if err != nil {
		t.Fatalf("CountMessages: %v", err)
	}
	if n != 20 {
		t.Errorf("CountMessages = %d, want 20", n)
	}
}

func TestContextCancellationIsHonoured(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	seedSession(t, s, "s1")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.AppendMessage(ctx, &store.MessageRecord{SessionID: "s1", Role: "user", Content: "x"}); err == nil {
		t.Error("AppendMessage with a cancelled context: err = nil, want failure")
	}
	if _, err := s.ListSessions(ctx, store.SessionFilter{}); err == nil {
		t.Error("ListSessions with a cancelled context: err = nil, want failure")
	}
}

func TestTimestampsSortLexicographically(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()
	seedSession(t, s, "s1")

	// A whole second and a fractional second must order correctly as text;
	// RFC3339Nano's trimmed zeros would get this wrong.
	base := time.Date(2026, 1, 1, 0, 0, 5, 0, time.UTC)
	for i, at := range []time.Time{base, base.Add(500 * time.Millisecond), base.Add(time.Second)} {
		if err := s.AppendMessage(ctx, &store.MessageRecord{
			SessionID: "s1", Role: "user", Content: fmt.Sprint(i), CreatedAt: at,
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	got, err := s.SearchMessages(ctx, store.SearchQuery{SessionID: "s1"})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	// SearchMessages orders newest first.
	want := []string{"2", "1", "0"}
	for i, m := range got {
		if m.Content != want[i] {
			t.Fatalf("position %d = %q, want %q (ordering is %v)", i, m.Content, want[i], got)
		}
	}
}

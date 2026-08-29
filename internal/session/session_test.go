package session_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/boop-dev/boop/internal/execution"
	"github.com/boop-dev/boop/internal/provider"
	"github.com/boop-dev/boop/internal/session"
	"github.com/boop-dev/boop/internal/store"
)

// newManager returns a Manager over a private in-memory database with a
// deterministic clock and ID sequence.
func newManager(t *testing.T) (*session.Manager, *store.SQLiteStore) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})

	var mu sync.Mutex
	tick := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	n := 0
	m := session.NewManager(st,
		session.WithClock(func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			tick = tick.Add(time.Second)
			return tick
		}),
		session.WithIDFunc(func() string {
			mu.Lock()
			defer mu.Unlock()
			n++
			return fmt.Sprintf("session-%03d", n)
		}),
	)
	return m, st
}

func TestCreateAssignsIdentityAndTimestamps(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t)
	ctx := context.Background()

	s, err := m.Create(ctx, session.CreateOptions{
		ProjectPath: "/srv/app", Provider: "ollama", Model: "qwen3", Title: "first",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.ID != "session-001" {
		t.Errorf("ID = %q, want session-001", s.ID)
	}
	if s.CreatedAt.IsZero() || !s.CreatedAt.Equal(s.UpdatedAt) {
		t.Errorf("timestamps = %v / %v, want equal and non-zero", s.CreatedAt, s.UpdatedAt)
	}

	loaded, err := m.Load(ctx, s.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if *loaded != *s {
		t.Errorf("loaded = %+v, want %+v", loaded, s)
	}
}

func TestCreateUsesUUIDsByDefault(t *testing.T) {
	t.Parallel()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	m := session.NewManager(st)
	s, err := m.Create(context.Background(), session.CreateOptions{ProjectPath: "/p"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// A UUID v4 in canonical form.
	if len(s.ID) != 36 {
		t.Errorf("ID = %q, want a 36-character UUID", s.ID)
	}
}

func TestCreateHonoursExplicitID(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t)
	s, err := m.Create(context.Background(), session.CreateOptions{ID: "fixed"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.ID != "fixed" {
		t.Errorf("ID = %q, want fixed", s.ID)
	}
}

func TestLoadMissingSessionIsNotFound(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t)
	if _, err := m.Load(context.Background(), "nope"); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestSaveUpdatesHeaderAndStampsUpdatedAt(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t)
	ctx := context.Background()
	s, err := m.Create(ctx, session.CreateOptions{Title: "before"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	created := s.CreatedAt

	if err := m.SetTitle(ctx, s, "after"); err != nil {
		t.Fatalf("SetTitle: %v", err)
	}
	if err := m.SetModel(ctx, s, "lmstudio", "devstral"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}

	loaded, err := m.Load(ctx, s.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Title != "after" || loaded.Provider != "lmstudio" || loaded.Model != "devstral" {
		t.Errorf("loaded = %+v", loaded)
	}
	if !loaded.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt drifted to %v, want %v", loaded.CreatedAt, created)
	}
	if !loaded.UpdatedAt.After(created) {
		t.Errorf("UpdatedAt = %v, want after %v", loaded.UpdatedAt, created)
	}
}

func TestSaveRejectsInvalidSession(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t)
	tests := []struct {
		name string
		s    *session.Session
	}{
		{"nil", nil},
		{"empty ID", &session.Session{}},
		{"blank ID", &session.Session{ID: "   "}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := m.Save(context.Background(), tc.s); err == nil {
				t.Error("err = nil, want failure")
			}
		})
	}
}

func TestResumeContinuesAppending(t *testing.T) {
	t.Parallel()
	m, st := newManager(t)
	ctx := context.Background()

	s, err := m.Create(ctx, session.CreateOptions{ProjectPath: "/srv/app"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := m.AppendMessages(ctx, s.ID,
		provider.Message{Role: provider.RoleUser, Content: "hello"},
		provider.Message{Role: provider.RoleAssistant, Content: "hi"},
	); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}

	// A fresh manager over the same store models a new process.
	resumed := session.NewManager(st)
	got, err := resumed.Resume(ctx, s.ID, "/srv/app")
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got.ID != s.ID {
		t.Fatalf("resumed ID = %q, want %q", got.ID, s.ID)
	}

	entry, err := resumed.AppendMessage(ctx, got.ID, provider.Message{Role: provider.RoleUser, Content: "again"})
	if err != nil {
		t.Fatalf("AppendMessage after resume: %v", err)
	}
	if entry.Seq != 3 {
		t.Errorf("Seq = %d, want 3; resumption must continue the sequence", entry.Seq)
	}

	msgs, err := resumed.History().Messages(ctx, got.ID, session.TranscriptOptions{})
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != 3 || msgs[2].Content != "again" {
		t.Errorf("transcript = %+v", msgs)
	}
}

func TestResumeRejectsAnotherProject(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t)
	ctx := context.Background()
	s, err := m.Create(ctx, session.CreateOptions{ProjectPath: "/srv/app"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	tests := []struct {
		name    string
		path    string
		wantErr error
	}{
		{"matching", "/srv/app", nil},
		{"unspecified", "", nil},
		{"mismatched", "/srv/other", session.ErrProjectMismatch},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := m.Resume(ctx, s.ID, tc.path)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Resume: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestLatestPrefersMostRecentlyUpdated(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t)
	ctx := context.Background()

	older, err := m.Create(ctx, session.CreateOptions{ProjectPath: "/a"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	newer, err := m.Create(ctx, session.CreateOptions{ProjectPath: "/a"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := m.Create(ctx, session.CreateOptions{ProjectPath: "/b"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := m.Latest(ctx, "/a")
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if got.ID != newer.ID {
		t.Errorf("Latest = %q, want %q", got.ID, newer.ID)
	}

	// Touching the older session makes it the latest again.
	if _, err := m.AppendMessage(ctx, older.ID, provider.Message{Role: provider.RoleUser, Content: "ping"}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	got, err = m.Latest(ctx, "/a")
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if got.ID != older.ID {
		t.Errorf("Latest = %q, want %q after appending", got.ID, older.ID)
	}

	if _, err := m.Latest(ctx, "/missing"); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("Latest(/missing) err = %v, want ErrNotFound", err)
	}
}

func TestListFiltersByProject(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t)
	ctx := context.Background()
	for _, path := range []string{"/a", "/b", "/a"} {
		if _, err := m.Create(ctx, session.CreateOptions{ProjectPath: path}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	got, err := m.List(ctx, session.ListOptions{ProjectPath: "/a"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
}

func TestDeleteRemovesSession(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t)
	ctx := context.Background()
	s, err := m.Create(ctx, session.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := m.AppendMessage(ctx, s.ID, provider.Message{Role: provider.RoleUser, Content: "x"}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if err := m.Delete(ctx, s.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := m.Load(ctx, s.ID); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("Load after delete: err = %v, want ErrNotFound", err)
	}
}

func TestMessageRoundTripPreservesMultimodalAndToolCalls(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t)
	ctx := context.Background()
	s, err := m.Create(ctx, session.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	tests := []struct {
		name string
		msg  provider.Message
	}{
		{
			"plain text",
			provider.Message{Role: provider.RoleUser, Content: "run the tests"},
		},
		{
			"multimodal parts",
			provider.Message{Role: provider.RoleUser, Parts: []provider.ContentPart{
				{Kind: provider.PartText, Text: "look at this"},
				{Kind: provider.PartImage, MIMEType: "image/png", Data: []byte{0x89, 0x50, 0x4e, 0x47}, Filename: "shot.png"},
			}},
		},
		{
			"assistant with tool calls",
			provider.Message{Role: provider.RoleAssistant, Content: "running", ToolCalls: []provider.ToolCall{
				{ID: "call-1", Name: "run", Arguments: `{"command":"go test ./..."}`},
			}},
		},
		{
			"tool result",
			provider.Message{Role: provider.RoleTool, ToolCallID: "call-1", Name: "run", Content: "ok"},
		},
	}
	for _, tc := range tests {
		if _, err := m.AppendMessage(ctx, s.ID, tc.msg); err != nil {
			t.Fatalf("AppendMessage %s: %v", tc.name, err)
		}
	}

	got, err := m.History().Messages(ctx, s.ID, session.TranscriptOptions{})
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(got) != len(tests) {
		t.Fatalf("len = %d, want %d", len(got), len(tests))
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if fmt.Sprintf("%+v", got[i]) != fmt.Sprintf("%+v", tc.msg) {
				t.Errorf("round trip:\n got %+v\nwant %+v", got[i], tc.msg)
			}
		})
	}
}

func TestAppendMessageRejectsRolelessMessage(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t)
	ctx := context.Background()
	s, err := m.Create(ctx, session.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := m.AppendMessage(ctx, s.ID, provider.Message{Content: "no role"}); err == nil {
		t.Error("err = nil, want failure")
	}
}

func TestToolCallLifecycle(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t)
	ctx := context.Background()
	s, err := m.Create(ctx, session.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	entry, err := m.AppendMessage(ctx, s.ID, provider.Message{
		Role: provider.RoleAssistant,
		ToolCalls: []provider.ToolCall{
			{ID: "call-1", Name: "run", Arguments: `{"command":"make test"}`},
		},
	})
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if err := m.AppendToolCall(ctx, s.ID, session.ToolInvocation{
		Call:      provider.ToolCall{ID: "call-1", Name: "run", Arguments: `{"command":"make test"}`},
		MessageID: entry.ID,
	}); err != nil {
		t.Fatalf("AppendToolCall: %v", err)
	}
	if err := m.CompleteToolCall(ctx, session.ToolOutcome{
		CallID: "call-1", Content: "2 tests failed", IsError: true, Duration: 900 * time.Millisecond,
	}); err != nil {
		t.Fatalf("CompleteToolCall: %v", err)
	}

	calls, err := m.ToolCalls(ctx, s.ID)
	if err != nil {
		t.Fatalf("ToolCalls: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("len = %d, want 1", len(calls))
	}
	if !calls[0].IsError || calls[0].Result != "2 tests failed" || calls[0].MessageID != entry.ID {
		t.Errorf("call = %+v", calls[0])
	}

	if err := m.AppendToolCall(ctx, s.ID, session.ToolInvocation{}); err == nil {
		t.Error("AppendToolCall without an ID: err = nil, want failure")
	}
	if err := m.CompleteToolCall(ctx, session.ToolOutcome{}); err == nil {
		t.Error("CompleteToolCall without an ID: err = nil, want failure")
	}
}

func TestRecordToolCallInOneStep(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t)
	ctx := context.Background()
	s, err := m.Create(ctx, session.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.RecordToolCall(ctx, s.ID,
		session.ToolInvocation{Call: provider.ToolCall{ID: "c1", Name: "read"}},
		session.ToolOutcome{Content: "file body"},
	); err != nil {
		t.Fatalf("RecordToolCall: %v", err)
	}
	calls, err := m.ToolCalls(ctx, s.ID)
	if err != nil {
		t.Fatalf("ToolCalls: %v", err)
	}
	if len(calls) != 1 || calls[0].Result != "file body" || calls[0].CompletedAt == nil {
		t.Errorf("calls = %+v", calls)
	}
}

func TestAppendExecutionKeepsFailures(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t)
	ctx := context.Background()
	s, err := m.Create(ctx, session.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	started := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	result := execution.RunResult{
		Command: "go build ./...", WorkingDir: "/srv/app", ExitCode: 1,
		Stdout: "", Stderr: "undefined: Foo", Duration: 2 * time.Second,
		StdoutTruncated: false, StderrTruncated: true, StartedAt: started,
	}
	id, err := m.AppendExecution(ctx, s.ID, session.ExecutionEntry{ToolCallID: "c1", Result: result})
	if err != nil {
		t.Fatalf("AppendExecution: %v", err)
	}
	if id == 0 {
		t.Fatal("AppendExecution returned id 0")
	}

	got, err := m.Executions(ctx, s.ID)
	if err != nil {
		t.Fatalf("Executions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	e := got[0]
	if e.ExitCode != 1 || e.Stderr != "undefined: Foo" || e.ToolCallID != "c1" {
		t.Errorf("execution = %+v", e)
	}
	if !e.StartedAt.Equal(started) || e.Duration != 2*time.Second {
		t.Errorf("timing = %v / %v", e.StartedAt, e.Duration)
	}
	if !e.StderrTruncated || e.StdoutTruncated {
		t.Errorf("truncation flags = %+v", e)
	}
}

func TestRecordUsageAggregates(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t)
	ctx := context.Background()
	s, err := m.Create(ctx, session.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	for _, u := range []session.UsageEntry{
		{Provider: "ollama", Model: "qwen3", Usage: provider.Usage{PromptTokens: 200, CompletionTokens: 40, TotalTokens: 240}},
		{Provider: "ollama", Model: "qwen3", Usage: provider.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120, CachedTokens: 60}},
	} {
		if err := m.RecordUsage(ctx, s.ID, u); err != nil {
			t.Fatalf("RecordUsage: %v", err)
		}
	}

	totals, err := m.Usage(ctx, s.ID)
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	want := store.UsageTotals{Exchanges: 2, PromptTokens: 300, CompletionTokens: 60, TotalTokens: 360, CachedTokens: 60}
	if totals != want {
		t.Errorf("totals = %+v, want %+v", totals, want)
	}
}

func TestAgentActivityIsRecorded(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t)
	ctx := context.Background()
	s, err := m.Create(ctx, session.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	rec := &session.AgentRecord{ID: "a1", SessionID: s.ID, Name: "tester", Task: "run tests", Status: "working"}
	if err := m.SaveAgent(ctx, rec); err != nil {
		t.Fatalf("SaveAgent: %v", err)
	}
	rec.Status = "complete"
	if err := m.SaveAgent(ctx, rec); err != nil {
		t.Fatalf("SaveAgent update: %v", err)
	}

	agents, err := m.Agents(ctx, s.ID)
	if err != nil {
		t.Fatalf("Agents: %v", err)
	}
	if len(agents) != 1 || agents[0].Status != "complete" {
		t.Errorf("agents = %+v", agents)
	}

	// Agent turns are tagged but share the transcript.
	if _, err := m.AppendAgentMessage(ctx, s.ID, "a1", provider.Message{Role: provider.RoleAssistant, Content: "done"}); err != nil {
		t.Fatalf("AppendAgentMessage: %v", err)
	}
	if _, err := m.AppendMessage(ctx, s.ID, provider.Message{Role: provider.RoleUser, Content: "thanks"}); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	agentTurns, err := m.History().Transcript(ctx, s.ID, session.TranscriptOptions{AgentID: "a1"})
	if err != nil {
		t.Fatalf("Transcript: %v", err)
	}
	if len(agentTurns) != 1 || agentTurns[0].Message.Content != "done" {
		t.Errorf("agent turns = %+v", agentTurns)
	}
}

func TestRecordEvent(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t)
	ctx := context.Background()
	s, err := m.Create(ctx, session.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := m.RecordEvent(ctx, session.EventEntry{
		SessionID: s.ID, Type: "tool.completed", Payload: map[string]any{"tool": "run", "exit": 0},
	}); err != nil {
		t.Fatalf("RecordEvent: %v", err)
	}
	events, err := m.Events(ctx, session.EventQuery{SessionID: s.ID})
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 1 || events[0].Type != "tool.completed" {
		t.Fatalf("events = %+v", events)
	}
	if string(events[0].Payload) == "" {
		t.Error("payload was not encoded")
	}

	if err := m.RecordEvent(ctx, session.EventEntry{SessionID: s.ID}); err == nil {
		t.Error("RecordEvent without a type: err = nil, want failure")
	}
	if err := m.RecordEvent(ctx, session.EventEntry{
		SessionID: s.ID, Type: "error", Payload: make(chan int),
	}); err == nil {
		t.Error("RecordEvent with an unencodable payload: err = nil, want failure")
	}
}

func TestConcurrentAppendsFromGoroutines(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t)
	ctx := context.Background()
	s, err := m.Create(ctx, session.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	const (
		workers = 6
		perTurn = 10
	)
	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range perTurn {
				if _, err := m.AppendMessage(ctx, s.ID, provider.Message{
					Role: provider.RoleUser, Content: fmt.Sprintf("w%d-%d", w, i),
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

	entries, err := m.History().Transcript(ctx, s.ID, session.TranscriptOptions{})
	if err != nil {
		t.Fatalf("Transcript: %v", err)
	}
	if len(entries) != workers*perTurn {
		t.Fatalf("len = %d, want %d", len(entries), workers*perTurn)
	}
	seen := make(map[int64]bool, len(entries))
	for _, e := range entries {
		if seen[e.Seq] {
			t.Fatalf("duplicate sequence number %d", e.Seq)
		}
		seen[e.Seq] = true
	}
}

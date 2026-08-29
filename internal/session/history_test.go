package session_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/boop-dev/boop/internal/provider"
	"github.com/boop-dev/boop/internal/session"
)

// seedTranscript creates a session and appends the given turns.
func seedTranscript(t *testing.T, m *session.Manager, msgs ...provider.Message) string {
	t.Helper()
	ctx := context.Background()
	s, err := m.Create(ctx, session.CreateOptions{ProjectPath: "/srv/app"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := m.AppendMessages(ctx, s.ID, msgs...); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	return s.ID
}

func TestTranscriptOptions(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t)
	id := seedTranscript(t, m,
		provider.Message{Role: provider.RoleUser, Content: "one"},
		provider.Message{Role: provider.RoleAssistant, Content: "two"},
		provider.Message{Role: provider.RoleUser, Content: "three"},
		provider.Message{Role: provider.RoleAssistant, Content: "four"},
		provider.Message{Role: provider.RoleUser, Content: "five"},
	)
	h := m.History()
	ctx := context.Background()

	tests := []struct {
		name string
		opts session.TranscriptOptions
		want []string
	}{
		{"all", session.TranscriptOptions{}, []string{"one", "two", "three", "four", "five"}},
		{"after seq", session.TranscriptOptions{AfterSeq: 3}, []string{"four", "five"}},
		{"role filter", session.TranscriptOptions{Roles: []provider.Role{provider.RoleUser}}, []string{"one", "three", "five"}},
		{"oldest limited", session.TranscriptOptions{Limit: 2}, []string{"one", "two"}},
		{"newest limited stays chronological", session.TranscriptOptions{Limit: 2, Newest: true}, []string{"four", "five"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := h.Messages(ctx, id, tc.opts)
			if err != nil {
				t.Fatalf("Messages: %v", err)
			}
			contents := make([]string, len(got))
			for i, msg := range got {
				contents[i] = msg.Content
			}
			if fmt.Sprint(contents) != fmt.Sprint(tc.want) {
				t.Errorf("contents = %v, want %v", contents, tc.want)
			}
		})
	}
}

func TestRecentLastAndCount(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t)
	h := m.History()
	ctx := context.Background()

	empty, err := m.Create(ctx, session.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, ok, err := h.Last(ctx, empty.ID); err != nil || ok {
		t.Errorf("Last on an empty session = ok %v, err %v; want false, nil", ok, err)
	}
	if n, err := h.Count(ctx, empty.ID); err != nil || n != 0 {
		t.Errorf("Count = %d, %v; want 0, nil", n, err)
	}

	id := seedTranscript(t, m,
		provider.Message{Role: provider.RoleUser, Content: "a"},
		provider.Message{Role: provider.RoleAssistant, Content: "b"},
		provider.Message{Role: provider.RoleUser, Content: "c"},
	)
	recent, err := h.Recent(ctx, id, 2)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(recent) != 2 || recent[0].Message.Content != "b" || recent[1].Message.Content != "c" {
		t.Errorf("Recent = %+v", recent)
	}
	if got, err := h.Recent(ctx, id, 0); err != nil || got != nil {
		t.Errorf("Recent(0) = %v, %v; want nil, nil", got, err)
	}

	last, ok, err := h.Last(ctx, id)
	if err != nil || !ok {
		t.Fatalf("Last: ok %v, err %v", ok, err)
	}
	if last.Message.Content != "c" || last.Seq != 3 {
		t.Errorf("Last = %+v", last)
	}
	if n, err := h.Count(ctx, id); err != nil || n != 3 {
		t.Errorf("Count = %d, %v; want 3, nil", n, err)
	}
}

func TestSearchAcrossSessions(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t)
	ctx := context.Background()
	h := m.History()

	first := seedTranscript(t, m,
		provider.Message{Role: provider.RoleUser, Content: "the migration runner keeps failing"},
		provider.Message{Role: provider.RoleAssistant, Content: "the migration ledger was double-inserting"},
	)
	second := seedTranscript(t, m,
		provider.Message{Role: provider.RoleUser, Content: "unrelated question about MIGRATION order"},
	)

	tests := []struct {
		name string
		opts session.SearchOptions
		want int
	}{
		{"across all sessions", session.SearchOptions{Text: "migration"}, 3},
		{"case insensitive", session.SearchOptions{Text: "MIGRATION"}, 3},
		{"scoped", session.SearchOptions{Text: "migration", SessionID: first}, 2},
		{"role scoped", session.SearchOptions{Text: "migration", Roles: []provider.Role{provider.RoleAssistant}}, 1},
		{"other session", session.SearchOptions{Text: "unrelated", SessionID: second}, 1},
		{"no hits", session.SearchOptions{Text: "kubernetes"}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hits, err := h.Search(ctx, tc.opts)
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(hits) != tc.want {
				t.Fatalf("len = %d, want %d (%+v)", len(hits), tc.want, hits)
			}
			for _, hit := range hits {
				if tc.opts.Text == "" {
					continue
				}
				if !strings.Contains(strings.ToLower(hit.Snippet), strings.ToLower(tc.opts.Text)) {
					t.Errorf("snippet %q does not contain %q", hit.Snippet, tc.opts.Text)
				}
				if hit.Offset < 0 {
					t.Errorf("offset = %d, want the match position", hit.Offset)
				}
				if hit.SessionID == "" || hit.Seq == 0 {
					t.Errorf("hit lacks storage metadata: %+v", hit.Entry)
				}
			}
		})
	}
}

func TestSearchSnippetElision(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t)
	ctx := context.Background()

	long := strings.Repeat("x", 200) + "NEEDLE" + strings.Repeat("y", 200)
	id := seedTranscript(t, m, provider.Message{Role: provider.RoleUser, Content: long})

	hits, err := m.History().Search(ctx, session.SearchOptions{Text: "NEEDLE", SessionID: id, SnippetRadius: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("len = %d, want 1", len(hits))
	}
	got := hits[0].Snippet
	want := "…" + strings.Repeat("x", 10) + "NEEDLE" + strings.Repeat("y", 10) + "…"
	if got != want {
		t.Errorf("snippet = %q, want %q", got, want)
	}
	if hits[0].Offset != 200 {
		t.Errorf("offset = %d, want 200", hits[0].Offset)
	}
}

func TestSearchSnippetHandlesMultibyteText(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t)
	ctx := context.Background()

	content := "køb en bålplads og sæt NEEDLE i midten af haven"
	id := seedTranscript(t, m, provider.Message{Role: provider.RoleUser, Content: content})

	hits, err := m.History().Search(ctx, session.SearchOptions{Text: "NEEDLE", SessionID: id, SnippetRadius: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("len = %d, want 1", len(hits))
	}
	if !strings.Contains(hits[0].Snippet, "NEEDLE") {
		t.Errorf("snippet = %q", hits[0].Snippet)
	}
	// The offset is a rune index, not a byte index: the text before the match
	// contains two-byte runes, so the two differ.
	byteIdx := strings.Index(content, "NEEDLE")
	want := utf8.RuneCountInString(content[:byteIdx])
	if want == byteIdx {
		t.Fatal("test fixture has no multi-byte runes before the match")
	}
	if hits[0].Offset != want {
		t.Errorf("offset = %d, want %d (rune index, byte index is %d)", hits[0].Offset, want, byteIdx)
	}
}

func TestSearchWithEmptyTextListsMessages(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t)
	ctx := context.Background()
	id := seedTranscript(t, m,
		provider.Message{Role: provider.RoleUser, Content: "one"},
		provider.Message{Role: provider.RoleAssistant, Content: "two"},
	)
	hits, err := m.History().Search(ctx, session.SearchOptions{SessionID: id})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("len = %d, want 2", len(hits))
	}
	for _, hit := range hits {
		if hit.Offset != -1 {
			t.Errorf("offset = %d, want -1 for an empty query", hit.Offset)
		}
	}
}

func TestSearchTimeWindow(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t)
	ctx := context.Background()
	id := seedTranscript(t, m,
		provider.Message{Role: provider.RoleUser, Content: "alpha"},
		provider.Message{Role: provider.RoleUser, Content: "alpha again"},
		provider.Message{Role: provider.RoleUser, Content: "alpha once more"},
	)
	entries, err := m.History().Transcript(ctx, id, session.TranscriptOptions{})
	if err != nil {
		t.Fatalf("Transcript: %v", err)
	}
	middle := entries[1].CreatedAt

	tests := []struct {
		name string
		opts session.SearchOptions
		want int
	}{
		{"since middle", session.SearchOptions{Text: "alpha", SessionID: id, Since: middle}, 2},
		{"until middle", session.SearchOptions{Text: "alpha", SessionID: id, Until: middle}, 2},
		{"window", session.SearchOptions{Text: "alpha", SessionID: id, Since: middle, Until: middle}, 1},
		{"future", session.SearchOptions{Text: "alpha", SessionID: id, Since: middle.Add(time.Hour)}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hits, err := m.History().Search(ctx, tc.opts)
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(hits) != tc.want {
				t.Errorf("len = %d, want %d", len(hits), tc.want)
			}
		})
	}
}

func TestNewHistoryWorksWithoutAManager(t *testing.T) {
	t.Parallel()
	m, st := newManager(t)
	id := seedTranscript(t, m, provider.Message{Role: provider.RoleUser, Content: "standalone"})

	h := session.NewHistory(st)
	msgs, err := h.Messages(context.Background(), id, session.TranscriptOptions{})
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Content != "standalone" {
		t.Errorf("msgs = %+v", msgs)
	}
}

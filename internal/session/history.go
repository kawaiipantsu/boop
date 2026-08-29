package session

import (
	"context"
	"strings"
	"time"

	"github.com/boop-dev/boop/internal/provider"
	"github.com/boop-dev/boop/internal/store"
)

// History is read access to persisted transcripts.
//
// It is separate from Manager because reading is the far more common operation
// and has no need for the write path: the TUI transcript pane, `/history`
// search and the context manager all only read.
type History struct {
	store store.Store
}

// NewHistory returns a History backed by st.
func NewHistory(st store.Store) *History { return &History{store: st} }

// History returns a reader over the same store this Manager writes to.
func (m *Manager) History() *History { return &History{store: m.store} }

// TranscriptOptions narrows a transcript read.
type TranscriptOptions struct {
	// AgentID restricts the read to one agent's turns when non-empty.
	AgentID string
	// Roles restricts the read to the given roles when non-empty.
	Roles []provider.Role
	// AfterSeq returns only turns after this sequence number, which is how a
	// resumed session picks up incrementally.
	AfterSeq int64
	// Limit caps the number of turns; zero means all.
	Limit int
	// Newest takes the Limit most recent turns rather than the oldest. Results
	// are returned oldest-first either way, ready to send to a provider.
	Newest bool
}

// Transcript returns a session's turns with their storage metadata.
func (h *History) Transcript(ctx context.Context, sessionID string, opts TranscriptOptions) ([]Entry, error) {
	recs, err := h.store.ListMessages(ctx, store.MessageQuery{
		SessionID: sessionID,
		AgentID:   opts.AgentID,
		Roles:     roleStrings(opts.Roles),
		AfterSeq:  opts.AfterSeq,
		Limit:     opts.Limit,
		Newest:    opts.Newest,
	})
	if err != nil {
		return nil, err
	}
	return toEntries(recs)
}

// Messages returns a session's turns as provider messages, oldest first.
//
// This is what gets handed to the context manager; it is never handed straight
// to a provider, because sending an entire session is exactly what §47 forbids.
func (h *History) Messages(ctx context.Context, sessionID string, opts TranscriptOptions) ([]provider.Message, error) {
	entries, err := h.Transcript(ctx, sessionID, opts)
	if err != nil {
		return nil, err
	}
	out := make([]provider.Message, len(entries))
	for i, e := range entries {
		out[i] = e.Message
	}
	return out, nil
}

// Recent returns the n most recent turns, oldest first.
func (h *History) Recent(ctx context.Context, sessionID string, n int) ([]Entry, error) {
	if n <= 0 {
		return nil, nil
	}
	return h.Transcript(ctx, sessionID, TranscriptOptions{Limit: n, Newest: true})
}

// Last returns the most recent turn, reporting ok=false for an empty session.
func (h *History) Last(ctx context.Context, sessionID string) (Entry, bool, error) {
	entries, err := h.Recent(ctx, sessionID, 1)
	if err != nil || len(entries) == 0 {
		return Entry{}, false, err
	}
	return entries[0], true, nil
}

// Count reports how many turns a session holds.
func (h *History) Count(ctx context.Context, sessionID string) (int64, error) {
	return h.store.CountMessages(ctx, sessionID)
}

// SearchOptions describes a transcript search.
type SearchOptions struct {
	// Text is matched as a case-insensitive substring. An empty Text matches
	// every message, which is how the other filters are used on their own.
	Text string
	// SessionID restricts the search to one session; empty searches all of
	// them, which is what makes past work findable (§2.7).
	SessionID string
	Roles     []provider.Role
	Since     time.Time
	Until     time.Time
	// Limit caps the number of hits; zero applies the store default.
	Limit int
	// SnippetRadius is how many characters of context to keep either side of a
	// match. Zero applies DefaultSnippetRadius.
	SnippetRadius int
}

// DefaultSnippetRadius is the context kept around a search match.
const DefaultSnippetRadius = 80

// SearchHit is one transcript search result.
type SearchHit struct {
	Entry
	// Snippet is the matched text with surrounding context, elided at both
	// ends when the message is longer than the snippet window.
	Snippet string `json:"snippet"`
	// Offset is the rune index of the match within the message content, or -1
	// when the query was empty.
	Offset int `json:"offset"`
}

// Search finds turns whose content contains the query text, newest first.
func (h *History) Search(ctx context.Context, opts SearchOptions) ([]SearchHit, error) {
	recs, err := h.store.SearchMessages(ctx, store.SearchQuery{
		Text:      opts.Text,
		SessionID: opts.SessionID,
		Roles:     roleStrings(opts.Roles),
		Since:     opts.Since,
		Until:     opts.Until,
		Limit:     opts.Limit,
	})
	if err != nil {
		return nil, err
	}
	radius := opts.SnippetRadius
	if radius <= 0 {
		radius = DefaultSnippetRadius
	}
	hits := make([]SearchHit, 0, len(recs))
	for _, rec := range recs {
		entry, err := fromMessageRecord(rec)
		if err != nil {
			return nil, err
		}
		offset, snippet := snippetAround(entry.Message.Content, opts.Text, radius)
		hits = append(hits, SearchHit{Entry: entry, Snippet: snippet, Offset: offset})
	}
	return hits, nil
}

// snippetAround locates query in content and renders a window around it.
//
// Indices are in runes, not bytes, so a match next to multi-byte text does not
// produce a broken snippet.
func snippetAround(content, query string, radius int) (int, string) {
	runes := []rune(content)
	if query == "" {
		if len(runes) <= 2*radius {
			return -1, content
		}
		return -1, string(runes[:2*radius]) + "…"
	}
	byteIdx := strings.Index(strings.ToLower(content), strings.ToLower(query))
	if byteIdx < 0 {
		// The store matched but the Go-side comparison did not; fall back to
		// the head of the message rather than claiming a position.
		if len(runes) <= 2*radius {
			return -1, content
		}
		return -1, string(runes[:2*radius]) + "…"
	}
	offset := len([]rune(content[:byteIdx]))
	end := offset + len([]rune(query))

	start := max(offset-radius, 0)
	stop := min(end+radius, len(runes))

	var b strings.Builder
	if start > 0 {
		b.WriteString("…")
	}
	b.WriteString(string(runes[start:stop]))
	if stop < len(runes) {
		b.WriteString("…")
	}
	return offset, b.String()
}

// toEntries decodes persisted messages into transcript entries.
func toEntries(recs []store.MessageRecord) ([]Entry, error) {
	out := make([]Entry, 0, len(recs))
	for _, rec := range recs {
		entry, err := fromMessageRecord(rec)
		if err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, nil
}

// roleStrings converts provider roles for the store's string-based query.
func roleStrings(roles []provider.Role) []string {
	if len(roles) == 0 {
		return nil
	}
	out := make([]string, len(roles))
	for i, r := range roles {
		out[i] = string(r)
	}
	return out
}

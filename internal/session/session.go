// Package session owns conversation state: its header, its transcript, and the
// context that is actually sent to a model.
//
// The package deliberately depends only on internal/provider (the neutral
// message types), internal/execution (the structured command result) and
// internal/store (persistence). It must never import internal/app or a UI
// package: the event bus and the frontends depend on sessions, not the other
// way round.
package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kawaiipantsu/boop/internal/store"
)

// ErrNotFound reports a session, agent or tool call that does not exist. It is
// re-exported from the store so callers need not import the persistence layer.
var ErrNotFound = store.ErrNotFound

// ErrProjectMismatch reports an attempt to resume a session that belongs to a
// different project root.
//
// This is a safety check, not bookkeeping: a session carries project memory and
// file references, and replaying it against another checkout would let the
// model reason about one project while acting on another.
var ErrProjectMismatch = errors.New("session: project path does not match the session")

// Session is the header of one conversation (§46).
//
// It is a plain value: the transcript lives in the store and is reached through
// Manager and History, so a Session can be copied, serialised and handed to a
// UI without dragging a database handle along.
type Session struct {
	// ID is a UUID assigned at creation.
	ID string `json:"id"`
	// ProjectPath is the absolute project root the session belongs to.
	ProjectPath string `json:"project_path"`
	// Provider and Model record the backend most recently used. The full
	// provider/model history is recoverable from recorded usage.
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// Title is a short human label, shown in `/session list`.
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Clone returns a copy of s.
func (s *Session) Clone() *Session {
	if s == nil {
		return nil
	}
	out := *s
	return &out
}

// Validate reports whether the session is well-formed enough to persist.
func (s *Session) Validate() error {
	if s == nil {
		return errors.New("session: nil session")
	}
	if strings.TrimSpace(s.ID) == "" {
		return errors.New("session: ID is required")
	}
	return nil
}

// Manager is the store-backed lifecycle and append API for sessions.
//
// It is safe for concurrent use: it holds no mutable state of its own and every
// method delegates to the store, which serialises writes.
type Manager struct {
	store store.Store
	now   func() time.Time
	newID func() string
}

// Option customises a Manager.
type Option func(*Manager)

// WithClock replaces the time source, which makes timestamps deterministic in
// tests.
func WithClock(now func() time.Time) Option {
	return func(m *Manager) {
		if now != nil {
			m.now = now
		}
	}
}

// WithIDFunc replaces the session ID generator. The default is a UUID v4.
func WithIDFunc(newID func() string) Option {
	return func(m *Manager) {
		if newID != nil {
			m.newID = newID
		}
	}
}

// NewManager returns a Manager backed by st.
func NewManager(st store.Store, opts ...Option) *Manager {
	m := &Manager{
		store: st,
		now:   time.Now,
		newID: func() string { return uuid.NewString() },
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Store returns the underlying store, for callers that need persistence
// operations this package does not wrap.
func (m *Manager) Store() store.Store { return m.store }

// CreateOptions describes a new session.
type CreateOptions struct {
	// ID overrides the generated UUID. Leave empty in normal use.
	ID          string
	ProjectPath string
	Provider    string
	Model       string
	Title       string
}

// Create starts and persists a new session.
func (m *Manager) Create(ctx context.Context, opts CreateOptions) (*Session, error) {
	id := strings.TrimSpace(opts.ID)
	if id == "" {
		id = m.newID()
	}
	now := m.now().UTC()
	s := &Session{
		ID:          id,
		ProjectPath: opts.ProjectPath,
		Provider:    opts.Provider,
		Model:       opts.Model,
		Title:       opts.Title,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := m.store.CreateSession(ctx, toSessionRecord(s)); err != nil {
		return nil, err
	}
	return s, nil
}

// Load reads a session header by ID.
func (m *Manager) Load(ctx context.Context, id string) (*Session, error) {
	rec, err := m.store.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	return fromSessionRecord(rec), nil
}

// Resume loads an existing session so work can continue on it.
//
// When projectPath is non-empty it must match the session's recorded root; see
// ErrProjectMismatch for why that is enforced rather than warned about.
func (m *Manager) Resume(ctx context.Context, id, projectPath string) (*Session, error) {
	s, err := m.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	if projectPath != "" && s.ProjectPath != "" && projectPath != s.ProjectPath {
		return nil, fmt.Errorf("session %s belongs to %q, not %q: %w", id, s.ProjectPath, projectPath, ErrProjectMismatch)
	}
	return s, nil
}

// Latest returns the most recently updated session, optionally restricted to
// one project root. It backs "continue where I left off".
func (m *Manager) Latest(ctx context.Context, projectPath string) (*Session, error) {
	recs, err := m.store.ListSessions(ctx, store.SessionFilter{ProjectPath: projectPath, Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, fmt.Errorf("session: no sessions for project %q: %w", projectPath, ErrNotFound)
	}
	return fromSessionRecord(recs[0]), nil
}

// ListOptions narrows Manager.List. It is the store's session filter, aliased
// so callers do not have to import the persistence layer for a filter.
type ListOptions = store.SessionFilter

// List returns session headers, most recently updated first by default.
func (m *Manager) List(ctx context.Context, opts ListOptions) ([]*Session, error) {
	recs, err := m.store.ListSessions(ctx, opts)
	if err != nil {
		return nil, err
	}
	out := make([]*Session, len(recs))
	for i, rec := range recs {
		out[i] = fromSessionRecord(rec)
	}
	return out, nil
}

// Save persists changes to a session header and stamps UpdatedAt in place.
func (m *Manager) Save(ctx context.Context, s *Session) error {
	if err := s.Validate(); err != nil {
		return err
	}
	s.UpdatedAt = m.now().UTC()
	if err := m.store.UpdateSession(ctx, toSessionRecord(s)); err != nil {
		return err
	}
	return nil
}

// Delete removes a session and everything recorded under it.
func (m *Manager) Delete(ctx context.Context, id string) error {
	return m.store.DeleteSession(ctx, id)
}

// SetModel records a provider/model switch on an open session.
//
// Switching model mid-session is normal (§9), so it is a first-class operation
// rather than a caller-assembled Save.
func (m *Manager) SetModel(ctx context.Context, s *Session, providerName, model string) error {
	s.Provider, s.Model = providerName, model
	return m.Save(ctx, s)
}

// SetTitle renames a session.
func (m *Manager) SetTitle(ctx context.Context, s *Session, title string) error {
	s.Title = title
	return m.Save(ctx, s)
}

// toSessionRecord converts to the persistence representation.
func toSessionRecord(s *Session) store.SessionRecord {
	return store.SessionRecord{
		ID:          s.ID,
		ProjectPath: s.ProjectPath,
		Provider:    s.Provider,
		Model:       s.Model,
		Title:       s.Title,
		CreatedAt:   s.CreatedAt,
		UpdatedAt:   s.UpdatedAt,
	}
}

// fromSessionRecord converts from the persistence representation.
func fromSessionRecord(rec store.SessionRecord) *Session {
	return &Session{
		ID:          rec.ID,
		ProjectPath: rec.ProjectPath,
		Provider:    rec.Provider,
		Model:       rec.Model,
		Title:       rec.Title,
		CreatedAt:   rec.CreatedAt,
		UpdatedAt:   rec.UpdatedAt,
	}
}

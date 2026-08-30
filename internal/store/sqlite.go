package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	// modernc.org/sqlite is a pure-Go SQLite. It is deliberately not the cgo
	// driver: Boop must cross-compile to six targets without a C toolchain
	// (§37), and a cgo dependency would make that impossible.
	_ "modernc.org/sqlite"
)

// driverName is the name modernc.org/sqlite registers with database/sql.
const driverName = "sqlite"

// timeLayout is the on-disk timestamp format: fixed-width UTC.
//
// Fixed width matters. RFC3339Nano trims trailing zeros, which makes
// "…:05Z" and "…:05.5Z" sort in the wrong order as text; padding every value
// to nine fractional digits keeps lexicographic order equal to chronological
// order, so ORDER BY created_at is correct without a numeric column.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

const (
	// busyTimeoutMS is how long a connection waits for a write lock before
	// giving up. Several agents may write concurrently (§11), and SQLite
	// permits a single writer, so contention is expected and must not be an
	// error.
	busyTimeoutMS = 5000
	// maxOpenConns bounds the connection pool for file-backed databases.
	maxOpenConns = 8
)

// SQLiteStore is the SQLite-backed Store implementation.
type SQLiteStore struct {
	db   *sql.DB
	path string
	// memory records that this database lives only in RAM, which changes the
	// pool configuration and disables WAL.
	memory    bool
	closeOnce sync.Once
	closeErr  error
}

var _ Store = (*SQLiteStore)(nil)

// Open opens or creates the database at path and brings its schema up to date.
//
// Parent directories are created as needed. The special path ":memory:" (and
// any DSN naming an in-memory database) yields a private database that lives
// for the lifetime of the returned store, which is what tests use.
func Open(path string) (*SQLiteStore, error) {
	return OpenContext(context.Background(), path)
}

// OpenContext is Open with a caller-supplied context governing the initial
// connection and the migration run.
func OpenContext(ctx context.Context, path string) (*SQLiteStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("store: database path is empty")
	}
	memory := isMemoryPath(path)
	dsn, err := buildDSN(path, memory)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %q: %w", path, err)
	}
	configurePool(db, memory)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: connect %q: %w", path, err)
	}
	if err := applyMigrations(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &SQLiteStore{db: db, path: path, memory: memory}, nil
}

// configurePool sizes the connection pool.
//
// An in-memory database is bound to its connection, so the pool is pinned to a
// single, never-expiring connection; letting database/sql retire it would
// silently discard the whole database.
func configurePool(db *sql.DB, memory bool) {
	if memory {
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
	} else {
		db.SetMaxOpenConns(maxOpenConns)
		db.SetMaxIdleConns(maxOpenConns)
	}
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)
}

// isMemoryPath reports whether path names an in-memory database.
func isMemoryPath(path string) bool {
	return path == ":memory:" ||
		strings.HasPrefix(path, "file::memory:") ||
		strings.Contains(path, "mode=memory")
}

// buildDSN turns a filesystem path into a driver DSN carrying the connection
// pragmas Boop relies on.
//
// The pragmas are placed in the DSN rather than executed after Open because
// the driver applies them to every connection the pool creates; running them
// once against *a* pooled connection would leave the others unconfigured.
func buildDSN(path string, memory bool) (string, error) {
	params := url.Values{}
	params.Set("_busy_timeout", fmt.Sprint(busyTimeoutMS))
	// Foreign keys are off by default in SQLite; Boop's cascades depend on them.
	params.Set("_foreign_keys", "on")
	// Every transaction takes the write lock up front. Deferred transactions
	// that upgrade from read to write can deadlock two concurrent writers into
	// SQLITE_BUSY that no busy timeout can resolve.
	params.Set("_txlock", "immediate")

	if memory {
		// Journal modes do not apply to memory databases; asking for WAL there
		// is a silent no-op, so it is simply omitted.
		return ":memory:?" + params.Encode(), nil
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("store: resolve %q: %w", path, err)
	}
	if dir := filepath.Dir(abs); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("store: create database directory %q: %w", dir, err)
		}
	}
	// WAL lets readers proceed during a write, which is what keeps the TUI
	// responsive while agents append. NORMAL synchronous is the standard
	// companion: durable across process crashes, fast enough for a chat log.
	params.Set("_journal_mode", "WAL")
	params.Set("_synchronous", "NORMAL")

	slashed := filepath.ToSlash(abs)
	if !strings.HasPrefix(slashed, "/") {
		// Windows paths such as C:/x/y.db become file:///C:/x/y.db.
		slashed = "/" + slashed
	}
	u := url.URL{Scheme: "file", Path: slashed, RawQuery: params.Encode()}
	return u.String(), nil
}

// Path returns the path the store was opened with.
func (s *SQLiteStore) Path() string { return s.path }

// DB exposes the underlying handle for packages that need a raw query, such as
// the statistics layer. Callers must not close it.
func (s *SQLiteStore) DB() *sql.DB { return s.db }

// Ping verifies the database is reachable.
func (s *SQLiteStore) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("store: ping: %w", err)
	}
	return nil
}

// Close flushes and closes the database. It is safe to call more than once,
// which matters during graceful shutdown (§58) where several paths may try.
func (s *SQLiteStore) Close() error {
	s.closeOnce.Do(func() {
		if !s.memory {
			// Fold the WAL back into the main database so the file left behind
			// is self-contained.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
			cancel()
		}
		s.closeErr = s.db.Close()
	})
	return s.closeErr
}

// ---------------------------------------------------------------- sessions --

// CreateSession inserts a new session header.
func (s *SQLiteStore) CreateSession(ctx context.Context, rec SessionRecord) error {
	if rec.ID == "" {
		return errors.New("store: session ID is required")
	}
	now := time.Now()
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = now
	}
	if rec.UpdatedAt.IsZero() {
		rec.UpdatedAt = rec.CreatedAt
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, project_path, provider, model, title, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		rec.ID, rec.ProjectPath, rec.Provider, rec.Model, rec.Title,
		formatTime(rec.CreatedAt), formatTime(rec.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("store: create session %s: %w", rec.ID, err)
	}
	return nil
}

// UpdateSession overwrites the mutable fields of an existing session header.
// CreatedAt is never rewritten.
func (s *SQLiteStore) UpdateSession(ctx context.Context, rec SessionRecord) error {
	if rec.ID == "" {
		return errors.New("store: session ID is required")
	}
	if rec.UpdatedAt.IsZero() {
		rec.UpdatedAt = time.Now()
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET project_path = ?, provider = ?, model = ?, title = ?, updated_at = ?
		 WHERE id = ?`,
		rec.ProjectPath, rec.Provider, rec.Model, rec.Title, formatTime(rec.UpdatedAt), rec.ID,
	)
	if err != nil {
		return fmt.Errorf("store: update session %s: %w", rec.ID, err)
	}
	return requireRow(res, fmt.Sprintf("session %s", rec.ID))
}

// GetSession loads one session header.
func (s *SQLiteStore) GetSession(ctx context.Context, id string) (SessionRecord, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, project_path, provider, model, title, created_at, updated_at
		 FROM sessions WHERE id = ?`, id)
	rec, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionRecord{}, fmt.Errorf("store: session %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return SessionRecord{}, fmt.Errorf("store: get session %s: %w", id, err)
	}
	return rec, nil
}

// ListSessions returns session headers matching filter, newest first unless
// SessionFilter.Oldest is set.
func (s *SQLiteStore) ListSessions(ctx context.Context, filter SessionFilter) ([]SessionRecord, error) {
	var (
		where []string
		args  []any
	)
	if filter.ProjectPath != "" {
		where = append(where, "project_path = ?")
		args = append(args, filter.ProjectPath)
	}
	if !filter.Since.IsZero() {
		where = append(where, "updated_at >= ?")
		args = append(args, formatTime(filter.Since))
	}
	order := "DESC"
	if filter.Oldest {
		order = "ASC"
	}
	q := `SELECT id, project_path, provider, model, title, created_at, updated_at FROM sessions` +
		whereClause(where) + ` ORDER BY updated_at ` + order + `, id ` + order
	q, args = withLimitOffset(q, args, filter.Limit, filter.Offset)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SessionRecord
	for rows.Next() {
		rec, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan session: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list sessions: %w", err)
	}
	return out, nil
}

// DeleteSession removes a session and everything recorded under it.
//
// Child rows are deleted explicitly rather than relying solely on ON DELETE
// CASCADE: the cascade is a per-connection pragma, and deletion of user data
// should not depend on connection state. The cascade remains as a backstop.
func (s *SQLiteStore) DeleteSession(ctx context.Context, id string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, table := range []string{"usage", "executions", "tool_calls", "messages", "agents", "events"} {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE session_id = ?`, id); err != nil {
				return fmt.Errorf("store: delete %s for session %s: %w", table, id, err)
			}
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
		if err != nil {
			return fmt.Errorf("store: delete session %s: %w", id, err)
		}
		return requireRow(res, fmt.Sprintf("session %s", id))
	})
}

// ---------------------------------------------------------------- messages --

// AppendMessage appends a message to a session transcript, assigning its ID
// and per-session sequence number and bumping the session's updated_at.
//
// The sequence number is allocated inside the same write transaction as the
// insert, so concurrent appenders cannot collide; the UNIQUE(session_id, seq)
// index is the final guard.
func (s *SQLiteStore) AppendMessage(ctx context.Context, rec *MessageRecord) error {
	if rec == nil {
		return errors.New("store: nil message record")
	}
	if rec.SessionID == "" {
		return errors.New("store: message session ID is required")
	}
	if rec.Role == "" {
		return errors.New("store: message role is required")
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now()
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := touchSession(ctx, tx, rec.SessionID, rec.CreatedAt); err != nil {
			return err
		}
		var next int64
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(seq), 0) + 1 FROM messages WHERE session_id = ?`, rec.SessionID,
		).Scan(&next); err != nil {
			return fmt.Errorf("store: allocate message sequence: %w", err)
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO messages
			   (session_id, agent_id, seq, role, content, name, tool_call_id, parts, tool_calls, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			rec.SessionID, rec.AgentID, next, rec.Role, rec.Content, rec.Name, rec.ToolCallID,
			nullBytes(rec.Parts), nullBytes(rec.ToolCalls), formatTime(rec.CreatedAt),
		)
		if err != nil {
			return fmt.Errorf("store: append message: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: append message: %w", err)
		}
		rec.ID, rec.Seq = id, next
		return nil
	})
}

// ListMessages returns transcript messages, always ordered oldest first.
func (s *SQLiteStore) ListMessages(ctx context.Context, q MessageQuery) ([]MessageRecord, error) {
	if q.SessionID == "" {
		return nil, errors.New("store: message query requires a session ID")
	}
	where := []string{"session_id = ?"}
	args := []any{q.SessionID}
	if q.AgentID != "" {
		where = append(where, "agent_id = ?")
		args = append(args, q.AgentID)
	}
	if q.AfterSeq > 0 {
		where = append(where, "seq > ?")
		args = append(args, q.AfterSeq)
	}
	if clause, roleArgs := inClause("role", q.Roles); clause != "" {
		where = append(where, clause)
		args = append(args, roleArgs...)
	}
	order := "ASC"
	if q.Newest && q.Limit > 0 {
		order = "DESC"
	}
	sqlText := `SELECT id, session_id, agent_id, seq, role, content, name, tool_call_id, parts, tool_calls, created_at
		 FROM messages` + whereClause(where) + ` ORDER BY seq ` + order
	sqlText, args = withLimitOffset(sqlText, args, q.Limit, 0)

	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list messages: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out, err := scanMessages(rows)
	if err != nil {
		return nil, err
	}
	if order == "DESC" {
		reverse(out)
	}
	return out, nil
}

// SearchMessages finds messages whose content contains q.Text.
//
// Matching is a case-insensitive substring search (SQLite's ASCII LIKE), not a
// full-text index: transcripts are per-session and small, and an FTS table
// would double the write cost of every turn for a feature used interactively.
func (s *SQLiteStore) SearchMessages(ctx context.Context, q SearchQuery) ([]MessageRecord, error) {
	var (
		where []string
		args  []any
	)
	if q.SessionID != "" {
		where = append(where, "session_id = ?")
		args = append(args, q.SessionID)
	}
	if q.Text != "" {
		where = append(where, `content LIKE ? ESCAPE '\'`)
		args = append(args, likePattern(q.Text))
	}
	if clause, roleArgs := inClause("role", q.Roles); clause != "" {
		where = append(where, clause)
		args = append(args, roleArgs...)
	}
	if !q.Since.IsZero() {
		where = append(where, "created_at >= ?")
		args = append(args, formatTime(q.Since))
	}
	if !q.Until.IsZero() {
		where = append(where, "created_at <= ?")
		args = append(args, formatTime(q.Until))
	}
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	sqlText := `SELECT id, session_id, agent_id, seq, role, content, name, tool_call_id, parts, tool_calls, created_at
		 FROM messages` + whereClause(where) + ` ORDER BY created_at DESC, id DESC`
	sqlText, args = withLimitOffset(sqlText, args, limit, 0)

	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("store: search messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanMessages(rows)
}

// CountMessages reports how many messages a session holds.
func (s *SQLiteStore) CountMessages(ctx context.Context, sessionID string) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE session_id = ?`, sessionID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count messages: %w", err)
	}
	return n, nil
}

// -------------------------------------------------------------- tool calls --

// AppendToolCall records a requested tool invocation.
func (s *SQLiteStore) AppendToolCall(ctx context.Context, rec *ToolCallRecord) error {
	if rec == nil {
		return errors.New("store: nil tool call record")
	}
	if rec.ID == "" {
		return errors.New("store: tool call ID is required")
	}
	if rec.SessionID == "" {
		return errors.New("store: tool call session ID is required")
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now()
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := touchSession(ctx, tx, rec.SessionID, rec.CreatedAt); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO tool_calls
			   (id, session_id, agent_id, message_id, name, arguments, result, is_error, duration_ns, created_at, completed_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			rec.ID, rec.SessionID, rec.AgentID, nullInt64(rec.MessageID), rec.Name, rec.Arguments,
			rec.Result, rec.IsError, int64(rec.Duration), formatTime(rec.CreatedAt), nullTime(rec.CompletedAt),
		)
		if err != nil {
			return fmt.Errorf("store: append tool call %s: %w", rec.ID, err)
		}
		return nil
	})
}

// CompleteToolCall attaches the outcome to an already-recorded tool call.
//
// A failed tool is still a completed tool: IsError is data returned to the
// model, not a reason to drop the row.
func (s *SQLiteStore) CompleteToolCall(ctx context.Context, res ToolCallResult) error {
	if res.ID == "" {
		return errors.New("store: tool call ID is required")
	}
	if res.CompletedAt.IsZero() {
		res.CompletedAt = time.Now()
	}
	out, err := s.db.ExecContext(ctx,
		`UPDATE tool_calls SET result = ?, is_error = ?, duration_ns = ?, completed_at = ? WHERE id = ?`,
		res.Result, res.IsError, int64(res.Duration), formatTime(res.CompletedAt), res.ID,
	)
	if err != nil {
		return fmt.Errorf("store: complete tool call %s: %w", res.ID, err)
	}
	return requireRow(out, fmt.Sprintf("tool call %s", res.ID))
}

// ListToolCalls returns a session's tool calls, oldest first.
func (s *SQLiteStore) ListToolCalls(ctx context.Context, sessionID string) ([]ToolCallRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, agent_id, message_id, name, arguments, result, is_error, duration_ns, created_at, completed_at
		 FROM tool_calls WHERE session_id = ? ORDER BY created_at, rowid`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: list tool calls: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ToolCallRecord
	for rows.Next() {
		var (
			rec         ToolCallRecord
			messageID   sql.NullInt64
			durationNS  int64
			createdAt   string
			completedAt sql.NullString
		)
		if err := rows.Scan(&rec.ID, &rec.SessionID, &rec.AgentID, &messageID, &rec.Name, &rec.Arguments,
			&rec.Result, &rec.IsError, &durationNS, &createdAt, &completedAt); err != nil {
			return nil, fmt.Errorf("store: scan tool call: %w", err)
		}
		rec.MessageID = messageID.Int64
		rec.Duration = time.Duration(durationNS)
		if rec.CreatedAt, err = parseTime(createdAt); err != nil {
			return nil, err
		}
		if rec.CompletedAt, err = parseTimePtr(completedAt); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list tool calls: %w", err)
	}
	return out, nil
}

// -------------------------------------------------------------- executions --

// AppendExecution records the structured result of a command run.
func (s *SQLiteStore) AppendExecution(ctx context.Context, rec *ExecutionRecord) error {
	if rec == nil {
		return errors.New("store: nil execution record")
	}
	if rec.SessionID == "" {
		return errors.New("store: execution session ID is required")
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now()
	}
	if rec.StartedAt.IsZero() {
		rec.StartedAt = rec.CreatedAt
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := touchSession(ctx, tx, rec.SessionID, rec.CreatedAt); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO executions
			   (session_id, agent_id, tool_call_id, command, working_dir, exit_code, stdout, stderr,
			    duration_ns, timed_out, cancelled, signal, stdout_truncated, stderr_truncated, started_at, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			rec.SessionID, rec.AgentID, rec.ToolCallID, rec.Command, rec.WorkingDir, rec.ExitCode,
			rec.Stdout, rec.Stderr, int64(rec.Duration), rec.TimedOut, rec.Cancelled, rec.Signal,
			rec.StdoutTruncated, rec.StderrTruncated, formatTime(rec.StartedAt), formatTime(rec.CreatedAt),
		)
		if err != nil {
			return fmt.Errorf("store: append execution: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: append execution: %w", err)
		}
		rec.ID = id
		return nil
	})
}

// ListExecutions returns a session's command executions, oldest first.
func (s *SQLiteStore) ListExecutions(ctx context.Context, sessionID string) ([]ExecutionRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, agent_id, tool_call_id, command, working_dir, exit_code, stdout, stderr,
		        duration_ns, timed_out, cancelled, signal, stdout_truncated, stderr_truncated, started_at, created_at
		 FROM executions WHERE session_id = ? ORDER BY id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: list executions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ExecutionRecord
	for rows.Next() {
		var (
			rec        ExecutionRecord
			durationNS int64
			startedAt  string
			createdAt  string
		)
		if err := rows.Scan(&rec.ID, &rec.SessionID, &rec.AgentID, &rec.ToolCallID, &rec.Command,
			&rec.WorkingDir, &rec.ExitCode, &rec.Stdout, &rec.Stderr, &durationNS, &rec.TimedOut,
			&rec.Cancelled, &rec.Signal, &rec.StdoutTruncated, &rec.StderrTruncated, &startedAt, &createdAt); err != nil {
			return nil, fmt.Errorf("store: scan execution: %w", err)
		}
		rec.Duration = time.Duration(durationNS)
		if rec.StartedAt, err = parseTime(startedAt); err != nil {
			return nil, err
		}
		if rec.CreatedAt, err = parseTime(createdAt); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list executions: %w", err)
	}
	return out, nil
}

// ------------------------------------------------------------------ agents --

// SaveAgent inserts or updates agent metadata, keyed by agent ID.
func (s *SQLiteStore) SaveAgent(ctx context.Context, rec *AgentRecord) error {
	if rec == nil {
		return errors.New("store: nil agent record")
	}
	if rec.ID == "" {
		return errors.New("store: agent ID is required")
	}
	if rec.SessionID == "" {
		return errors.New("store: agent session ID is required")
	}
	now := time.Now()
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = now
	}
	rec.UpdatedAt = now
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := touchSession(ctx, tx, rec.SessionID, rec.UpdatedAt); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO agents
			   (id, session_id, parent_id, name, task, provider, model, status, error, started_at, finished_at, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(id) DO UPDATE SET
			   parent_id = excluded.parent_id,
			   name = excluded.name,
			   task = excluded.task,
			   provider = excluded.provider,
			   model = excluded.model,
			   status = excluded.status,
			   error = excluded.error,
			   started_at = excluded.started_at,
			   finished_at = excluded.finished_at,
			   updated_at = excluded.updated_at`,
			rec.ID, rec.SessionID, rec.ParentID, rec.Name, rec.Task, rec.Provider, rec.Model,
			rec.Status, rec.Error, nullTime(rec.StartedAt), nullTime(rec.FinishedAt),
			formatTime(rec.CreatedAt), formatTime(rec.UpdatedAt),
		)
		if err != nil {
			return fmt.Errorf("store: save agent %s: %w", rec.ID, err)
		}
		return nil
	})
}

// GetAgent loads one agent record.
func (s *SQLiteStore) GetAgent(ctx context.Context, id string) (AgentRecord, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, session_id, parent_id, name, task, provider, model, status, error,
		        started_at, finished_at, created_at, updated_at
		 FROM agents WHERE id = ?`, id)
	rec, err := scanAgent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentRecord{}, fmt.Errorf("store: agent %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return AgentRecord{}, fmt.Errorf("store: get agent %s: %w", id, err)
	}
	return rec, nil
}

// ListAgents returns a session's agents, oldest first.
func (s *SQLiteStore) ListAgents(ctx context.Context, sessionID string) ([]AgentRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, parent_id, name, task, provider, model, status, error,
		        started_at, finished_at, created_at, updated_at
		 FROM agents WHERE session_id = ? ORDER BY created_at, id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: list agents: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []AgentRecord
	for rows.Next() {
		rec, err := scanAgent(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan agent: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list agents: %w", err)
	}
	return out, nil
}

// ------------------------------------------------------------------ events --

// AppendEvent records one event for the searchable history.
//
// Events are deliberately not tied to a session by a foreign key: process-level
// events belong to no session and must still be recordable.
func (s *SQLiteStore) AppendEvent(ctx context.Context, rec *EventRecord) error {
	if rec == nil {
		return errors.New("store: nil event record")
	}
	if rec.Type == "" {
		return errors.New("store: event type is required")
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO events (session_id, agent_id, type, payload, created_at) VALUES (?, ?, ?, ?, ?)`,
		rec.SessionID, rec.AgentID, rec.Type, nullBytes(rec.Payload), formatTime(rec.CreatedAt),
	)
	if err != nil {
		return fmt.Errorf("store: append event %s: %w", rec.Type, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("store: append event %s: %w", rec.Type, err)
	}
	rec.ID = id
	return nil
}

// ListEvents returns recorded events, always ordered oldest first.
func (s *SQLiteStore) ListEvents(ctx context.Context, q EventQuery) ([]EventRecord, error) {
	var (
		where []string
		args  []any
	)
	if q.SessionID != "" {
		where = append(where, "session_id = ?")
		args = append(args, q.SessionID)
	}
	if q.AgentID != "" {
		where = append(where, "agent_id = ?")
		args = append(args, q.AgentID)
	}
	if clause, typeArgs := inClause("type", q.Types); clause != "" {
		where = append(where, clause)
		args = append(args, typeArgs...)
	}
	if !q.Since.IsZero() {
		where = append(where, "created_at >= ?")
		args = append(args, formatTime(q.Since))
	}
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultEventLimit
	}
	order := "ASC"
	if q.Newest {
		order = "DESC"
	}
	sqlText := `SELECT id, session_id, agent_id, type, payload, created_at FROM events` +
		whereClause(where) + ` ORDER BY id ` + order
	sqlText, args = withLimitOffset(sqlText, args, limit, 0)

	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []EventRecord
	for rows.Next() {
		var (
			rec       EventRecord
			payload   []byte
			createdAt string
		)
		if err := rows.Scan(&rec.ID, &rec.SessionID, &rec.AgentID, &rec.Type, &payload, &createdAt); err != nil {
			return nil, fmt.Errorf("store: scan event: %w", err)
		}
		rec.Payload = payload
		if rec.CreatedAt, err = parseTime(createdAt); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list events: %w", err)
	}
	if order == "DESC" {
		reverse(out)
	}
	return out, nil
}

// ------------------------------------------------------------------- usage --

// AppendUsage records provider-reported token accounting for one exchange.
func (s *SQLiteStore) AppendUsage(ctx context.Context, rec *UsageRecord) error {
	if rec == nil {
		return errors.New("store: nil usage record")
	}
	if rec.SessionID == "" {
		return errors.New("store: usage session ID is required")
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now()
	}
	if rec.TotalTokens == 0 {
		rec.TotalTokens = rec.PromptTokens + rec.CompletionTokens
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := touchSession(ctx, tx, rec.SessionID, rec.CreatedAt); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO usage
			   (session_id, agent_id, provider, model, prompt_tokens, completion_tokens, total_tokens, cached_tokens, cost_usd, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			rec.SessionID, rec.AgentID, rec.Provider, rec.Model, rec.PromptTokens, rec.CompletionTokens,
			rec.TotalTokens, rec.CachedTokens, rec.CostUSD, formatTime(rec.CreatedAt),
		)
		if err != nil {
			return fmt.Errorf("store: append usage: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: append usage: %w", err)
		}
		rec.ID = id
		return nil
	})
}

// SessionUsage aggregates a session's token accounting.
func (s *SQLiteStore) SessionUsage(ctx context.Context, sessionID string) (UsageTotals, error) {
	var t UsageTotals
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*),
		        COALESCE(SUM(prompt_tokens), 0),
		        COALESCE(SUM(completion_tokens), 0),
		        COALESCE(SUM(total_tokens), 0),
		        COALESCE(SUM(cached_tokens), 0),
		        COALESCE(SUM(cost_usd), 0)
		 FROM usage WHERE session_id = ?`, sessionID,
	).Scan(&t.Exchanges, &t.PromptTokens, &t.CompletionTokens, &t.TotalTokens, &t.CachedTokens, &t.CostUSD)
	if err != nil {
		return UsageTotals{}, fmt.Errorf("store: session usage: %w", err)
	}
	return t, nil
}

// ----------------------------------------------------------------- helpers --

// scanner abstracts *sql.Row and *sql.Rows so a row can be scanned by either.
type scanner interface {
	Scan(dest ...any) error
}

func scanSession(sc scanner) (SessionRecord, error) {
	var (
		rec       SessionRecord
		createdAt string
		updatedAt string
	)
	if err := sc.Scan(&rec.ID, &rec.ProjectPath, &rec.Provider, &rec.Model, &rec.Title, &createdAt, &updatedAt); err != nil {
		return SessionRecord{}, err
	}
	var err error
	if rec.CreatedAt, err = parseTime(createdAt); err != nil {
		return SessionRecord{}, err
	}
	if rec.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return SessionRecord{}, err
	}
	return rec, nil
}

func scanAgent(sc scanner) (AgentRecord, error) {
	var (
		rec        AgentRecord
		startedAt  sql.NullString
		finishedAt sql.NullString
		createdAt  string
		updatedAt  string
	)
	if err := sc.Scan(&rec.ID, &rec.SessionID, &rec.ParentID, &rec.Name, &rec.Task, &rec.Provider,
		&rec.Model, &rec.Status, &rec.Error, &startedAt, &finishedAt, &createdAt, &updatedAt); err != nil {
		return AgentRecord{}, err
	}
	var err error
	if rec.StartedAt, err = parseTimePtr(startedAt); err != nil {
		return AgentRecord{}, err
	}
	if rec.FinishedAt, err = parseTimePtr(finishedAt); err != nil {
		return AgentRecord{}, err
	}
	if rec.CreatedAt, err = parseTime(createdAt); err != nil {
		return AgentRecord{}, err
	}
	if rec.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return AgentRecord{}, err
	}
	return rec, nil
}

func scanMessages(rows *sql.Rows) ([]MessageRecord, error) {
	var out []MessageRecord
	for rows.Next() {
		var (
			rec       MessageRecord
			parts     []byte
			toolCalls []byte
			createdAt string
		)
		if err := rows.Scan(&rec.ID, &rec.SessionID, &rec.AgentID, &rec.Seq, &rec.Role, &rec.Content,
			&rec.Name, &rec.ToolCallID, &parts, &toolCalls, &createdAt); err != nil {
			return nil, fmt.Errorf("store: scan message: %w", err)
		}
		rec.Parts, rec.ToolCalls = parts, toolCalls
		var err error
		if rec.CreatedAt, err = parseTime(createdAt); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate messages: %w", err)
	}
	return out, nil
}

// inTx runs fn inside a transaction, rolling back on error or panic.
func (s *SQLiteStore) inTx(ctx context.Context, fn func(tx *sql.Tx) error) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin transaction: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("store: commit transaction: %w", err)
	}
	return nil
}

// touchSession bumps a session's updated_at and, because the update matches no
// row for an unknown session, doubles as the existence check for every append.
func touchSession(ctx context.Context, tx *sql.Tx, sessionID string, at time.Time) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE sessions SET updated_at = ? WHERE id = ?`, formatTime(at), sessionID)
	if err != nil {
		return fmt.Errorf("store: touch session %s: %w", sessionID, err)
	}
	return requireRow(res, fmt.Sprintf("session %s", sessionID))
}

// requireRow turns "matched nothing" into ErrNotFound.
func requireRow(res sql.Result, what string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: %s: %w", what, err)
	}
	if n == 0 {
		return fmt.Errorf("store: %s: %w", what, ErrNotFound)
	}
	return nil
}

// whereClause renders a conjunction, or the empty string when there are no
// conditions.
func whereClause(conds []string) string {
	if len(conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(conds, " AND ")
}

// inClause renders `col IN (?, ?, …)` for a non-empty value set.
func inClause(column string, values []string) (string, []any) {
	if len(values) == 0 {
		return "", nil
	}
	args := make([]any, len(values))
	for i, v := range values {
		args[i] = v
	}
	return column + " IN (" + strings.TrimSuffix(strings.Repeat("?, ", len(values)), ", ") + ")", args
}

// withLimitOffset appends LIMIT/OFFSET as bound parameters.
func withLimitOffset(query string, args []any, limit, offset int) (string, []any) {
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
		if offset > 0 {
			query += " OFFSET ?"
			args = append(args, offset)
		}
	} else if offset > 0 {
		// SQLite requires a LIMIT before OFFSET; -1 means unbounded.
		query += " LIMIT -1 OFFSET ?"
		args = append(args, offset)
	}
	return query, args
}

// likePattern wraps s in wildcards and escapes LIKE metacharacters so a search
// for "100%" does not silently become a prefix match.
func likePattern(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(s) + "%"
}

// reverse flips a slice in place.
func reverse[T any](s []T) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}

// nullBytes stores an empty blob as SQL NULL to keep the file compact.
func nullBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// nullInt64 stores a zero identifier as SQL NULL.
func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// nullTime stores an unset timestamp as SQL NULL.
func nullTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return formatTime(*t)
}

// formatTime renders a timestamp in the fixed-width UTC on-disk layout.
func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

// parseTime reads a stored timestamp. RFC3339 variants are accepted so a
// database hand-edited with the sqlite3 CLI still loads.
func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(timeLayout, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("store: invalid timestamp %q", s)
}

// parseTimePtr reads a nullable stored timestamp.
func parseTimePtr(s sql.NullString) (*time.Time, error) {
	if !s.Valid || s.String == "" {
		return nil, nil
	}
	t, err := parseTime(s.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/boop-dev/boop/internal/store"
)

func TestSchemaHasEveryRequiredTable(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()

	// PROJECT.md §4.7 and §46 enumerate what must be persisted.
	tables := []string{"sessions", "messages", "tool_calls", "executions", "agents", "events", "usage", "schema_version"}
	for _, name := range tables {
		t.Run(name, func(t *testing.T) {
			var found string
			err := s.DB().QueryRowContext(ctx,
				`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&found)
			if err != nil {
				t.Fatalf("table %q missing: %v", name, err)
			}
		})
	}
}

func TestSchemaIndexesCoverSessionAndTime(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()

	rows, err := s.DB().QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'index' AND name LIKE 'idx_%'`)
	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan index: %v", err)
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate indexes: %v", err)
	}

	want := []string{
		"idx_messages_session_seq", "idx_messages_created_at",
		"idx_tool_calls_session_id", "idx_tool_calls_created_at",
		"idx_executions_session_id", "idx_executions_created_at",
		"idx_events_session_id", "idx_events_created_at",
		"idx_usage_session_id", "idx_usage_created_at",
		"idx_agents_session_id", "idx_agents_created_at",
		"idx_sessions_created_at",
	}
	for _, name := range want {
		if !have[name] {
			t.Errorf("index %q missing", name)
		}
	}
}

func TestSchemaVersionMatchesLedger(t *testing.T) {
	t.Parallel()
	s := newMemoryStore(t)
	ctx := context.Background()

	version, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	rows, err := s.DB().QueryContext(ctx, `SELECT version, name FROM schema_version ORDER BY version`)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	defer rows.Close()

	var last int64
	var count int64
	for rows.Next() {
		var (
			v    int64
			name string
		)
		if err := rows.Scan(&v, &name); err != nil {
			t.Fatalf("scan ledger: %v", err)
		}
		if v <= last {
			t.Fatalf("ledger is not strictly increasing: %d after %d", v, last)
		}
		if name == "" {
			t.Errorf("ledger row %d has no migration name", v)
		}
		last, count = v, count+1
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate ledger: %v", err)
	}
	if count == 0 {
		t.Fatal("no migrations recorded")
	}
	if version != last {
		t.Errorf("SchemaVersion = %d, want %d", version, last)
	}
}

func TestSchemaVersionOnFreshFileDatabase(t *testing.T) {
	t.Parallel()
	s, err := store.Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	v, err := s.SchemaVersion(context.Background())
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v < 2 {
		t.Errorf("SchemaVersion = %d, want at least 2 (init + indexes)", v)
	}
}

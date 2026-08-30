package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// migrationFS holds the schema, embedded so a released binary carries its own
// migrations and never depends on files next to it.
//
//go:embed migrations/*.sql
var migrationFS embed.FS

// migration is one embedded schema step.
type migration struct {
	version int64
	name    string
	sql     string
}

// schemaVersionDDL creates the ledger of applied migrations. It is applied
// before any migration and is itself idempotent.
const schemaVersionDDL = `
CREATE TABLE IF NOT EXISTS schema_version (
    version    INTEGER PRIMARY KEY,
    name       TEXT NOT NULL,
    applied_at TEXT NOT NULL
);`

// loadMigrations reads the embedded migrations, ordered by the numeric prefix
// of their filenames.
//
// The prefix, not the lexical filename, is authoritative: it keeps ordering
// correct once the numbering passes 9 and stops a rename from silently
// reordering an already-applied schema.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("store: read embedded migrations: %w", err)
	}
	out := make([]migration, 0, len(entries))
	seen := make(map[int64]string, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		version, err := migrationVersion(entry.Name())
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("store: duplicate migration version %d in %q and %q", version, prev, entry.Name())
		}
		seen[version] = entry.Name()

		body, err := migrationFS.ReadFile(path.Join("migrations", entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("store: read migration %q: %w", entry.Name(), err)
		}
		out = append(out, migration{version: version, name: entry.Name(), sql: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// migrationVersion extracts the leading numeric prefix of a migration filename.
func migrationVersion(name string) (int64, error) {
	idx := strings.IndexByte(name, '_')
	if idx <= 0 {
		return 0, fmt.Errorf("store: migration %q must be named <version>_<description>.sql", name)
	}
	version, err := strconv.ParseInt(name[:idx], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("store: migration %q has a non-numeric version prefix: %w", name, err)
	}
	if version <= 0 {
		return 0, fmt.Errorf("store: migration %q must have a positive version", name)
	}
	return version, nil
}

// applyMigrations brings db up to the latest embedded schema.
//
// It runs on every Open and is therefore idempotent: already-recorded versions
// are skipped, and each pending migration runs inside its own transaction so a
// failure leaves the ledger and the schema consistent with each other.
func applyMigrations(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, schemaVersionDDL); err != nil {
		return fmt.Errorf("store: create schema_version: %w", err)
	}
	applied, err := appliedVersions(ctx, db)
	if err != nil {
		return err
	}
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	for _, m := range migrations {
		if _, done := applied[m.version]; done {
			continue
		}
		if err := applyMigration(ctx, db, m); err != nil {
			return err
		}
	}
	return nil
}

// applyMigration executes one migration and records it atomically.
func applyMigration(ctx context.Context, db *sql.DB, m migration) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin migration %s: %w", m.name, err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("store: apply migration %s: %w", m.name, err)
	}
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO schema_version (version, name, applied_at) VALUES (?, ?, ?)`,
		m.version, m.name, formatTime(time.Now()),
	); err != nil {
		return fmt.Errorf("store: record migration %s: %w", m.name, err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("store: commit migration %s: %w", m.name, err)
	}
	return nil
}

// appliedVersions reads the migration ledger.
func appliedVersions(ctx context.Context, db *sql.DB) (map[int64]struct{}, error) {
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_version`)
	if err != nil {
		return nil, fmt.Errorf("store: read schema_version: %w", err)
	}
	defer func() { _ = rows.Close() }()

	applied := make(map[int64]struct{})
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("store: scan schema_version: %w", err)
		}
		applied[v] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate schema_version: %w", err)
	}
	return applied, nil
}

// SchemaVersion reports the highest migration version applied to the database,
// or zero when none has been applied.
func (s *SQLiteStore) SchemaVersion(ctx context.Context) (int64, error) {
	var version sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		return 0, fmt.Errorf("store: schema version: %w", err)
	}
	return version.Int64, nil
}

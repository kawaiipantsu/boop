-- 0001_init: core session state (PROJECT.md §4.7, §46).
--
-- Timestamps are stored as fixed-width UTC text ("2006-01-02T15:04:05.000000000Z")
-- so that lexicographic ordering matches chronological ordering and the file
-- stays readable with the sqlite3 CLI.

CREATE TABLE IF NOT EXISTS sessions (
    id           TEXT PRIMARY KEY,
    project_path TEXT NOT NULL DEFAULT '',
    provider     TEXT NOT NULL DEFAULT '',
    model        TEXT NOT NULL DEFAULT '',
    title        TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS agents (
    id          TEXT PRIMARY KEY,
    session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    parent_id   TEXT NOT NULL DEFAULT '',
    name        TEXT NOT NULL DEFAULT '',
    task        TEXT NOT NULL DEFAULT '',
    provider    TEXT NOT NULL DEFAULT '',
    model       TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT '',
    error       TEXT NOT NULL DEFAULT '',
    started_at  TEXT,
    finished_at TEXT,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS messages (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id   TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    agent_id     TEXT NOT NULL DEFAULT '',
    seq          INTEGER NOT NULL,
    role         TEXT NOT NULL,
    content      TEXT NOT NULL DEFAULT '',
    name         TEXT NOT NULL DEFAULT '',
    tool_call_id TEXT NOT NULL DEFAULT '',
    -- parts and tool_calls hold raw JSON; the store deliberately does not
    -- interpret provider payloads.
    parts        BLOB,
    tool_calls   BLOB,
    created_at   TEXT NOT NULL,
    UNIQUE (session_id, seq)
);

CREATE TABLE IF NOT EXISTS tool_calls (
    id           TEXT PRIMARY KEY,
    session_id   TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    agent_id     TEXT NOT NULL DEFAULT '',
    message_id   INTEGER,
    name         TEXT NOT NULL,
    arguments    TEXT NOT NULL DEFAULT '',
    result       TEXT NOT NULL DEFAULT '',
    is_error     INTEGER NOT NULL DEFAULT 0,
    duration_ns  INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT NOT NULL,
    completed_at TEXT
);

CREATE TABLE IF NOT EXISTS executions (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id       TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    agent_id         TEXT NOT NULL DEFAULT '',
    tool_call_id     TEXT NOT NULL DEFAULT '',
    command          TEXT NOT NULL,
    working_dir      TEXT NOT NULL DEFAULT '',
    exit_code        INTEGER NOT NULL DEFAULT 0,
    stdout           TEXT NOT NULL DEFAULT '',
    stderr           TEXT NOT NULL DEFAULT '',
    duration_ns      INTEGER NOT NULL DEFAULT 0,
    timed_out        INTEGER NOT NULL DEFAULT 0,
    cancelled        INTEGER NOT NULL DEFAULT 0,
    signal           TEXT NOT NULL DEFAULT '',
    stdout_truncated INTEGER NOT NULL DEFAULT 0,
    stderr_truncated INTEGER NOT NULL DEFAULT 0,
    started_at       TEXT NOT NULL,
    created_at       TEXT NOT NULL
);

-- events carries no foreign key: the bus emits process-level events that
-- belong to no session, and those must still be recordable. DeleteSession
-- removes the matching rows explicitly.
CREATE TABLE IF NOT EXISTS events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL DEFAULT '',
    agent_id   TEXT NOT NULL DEFAULT '',
    type       TEXT NOT NULL,
    payload    BLOB,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS usage (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id        TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    agent_id          TEXT NOT NULL DEFAULT '',
    provider          TEXT NOT NULL DEFAULT '',
    model             TEXT NOT NULL DEFAULT '',
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens      INTEGER NOT NULL DEFAULT 0,
    cached_tokens     INTEGER NOT NULL DEFAULT 0,
    cost_usd          REAL NOT NULL DEFAULT 0,
    created_at        TEXT NOT NULL
);

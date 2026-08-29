-- 0002_indexes: every hot path filters by session_id and orders by time.

CREATE INDEX IF NOT EXISTS idx_sessions_project_path ON sessions (project_path);
CREATE INDEX IF NOT EXISTS idx_sessions_updated_at   ON sessions (updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_sessions_created_at   ON sessions (created_at DESC);

CREATE INDEX IF NOT EXISTS idx_agents_session_id ON agents (session_id);
CREATE INDEX IF NOT EXISTS idx_agents_created_at ON agents (created_at);
CREATE INDEX IF NOT EXISTS idx_agents_parent_id  ON agents (parent_id);

CREATE INDEX IF NOT EXISTS idx_messages_session_seq ON messages (session_id, seq);
CREATE INDEX IF NOT EXISTS idx_messages_created_at  ON messages (created_at);
CREATE INDEX IF NOT EXISTS idx_messages_agent_id    ON messages (agent_id);
CREATE INDEX IF NOT EXISTS idx_messages_role        ON messages (role);

CREATE INDEX IF NOT EXISTS idx_tool_calls_session_id ON tool_calls (session_id);
CREATE INDEX IF NOT EXISTS idx_tool_calls_created_at ON tool_calls (created_at);
CREATE INDEX IF NOT EXISTS idx_tool_calls_name       ON tool_calls (name);

CREATE INDEX IF NOT EXISTS idx_executions_session_id   ON executions (session_id);
CREATE INDEX IF NOT EXISTS idx_executions_created_at   ON executions (created_at);
CREATE INDEX IF NOT EXISTS idx_executions_tool_call_id ON executions (tool_call_id);

CREATE INDEX IF NOT EXISTS idx_events_session_id ON events (session_id);
CREATE INDEX IF NOT EXISTS idx_events_created_at ON events (created_at);
CREATE INDEX IF NOT EXISTS idx_events_type       ON events (type);

CREATE INDEX IF NOT EXISTS idx_usage_session_id ON usage (session_id);
CREATE INDEX IF NOT EXISTS idx_usage_created_at ON usage (created_at);

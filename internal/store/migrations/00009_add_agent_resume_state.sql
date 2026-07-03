-- +goose Up
ALTER TABLE codex_sessions
ADD COLUMN requested_model TEXT;

ALTER TABLE codex_sessions
ADD COLUMN agent_backend_id TEXT;

ALTER TABLE codex_sessions
ADD COLUMN agent_backend_kind TEXT;

ALTER TABLE codex_sessions
ADD COLUMN agent_role TEXT;

ALTER TABLE codex_sessions
ADD COLUMN provider_thread_id TEXT;

ALTER TABLE codex_sessions
ADD COLUMN provider_session_id TEXT;

ALTER TABLE codex_sessions
ADD COLUMN resumed_from_session_id INTEGER;

CREATE INDEX codex_sessions_resume_lookup_idx
ON codex_sessions(identifier, issue_id, issue_url, agent_backend_id, agent_backend_kind, agent_role, requested_model, completed_at DESC, id DESC);

-- +goose Down
DROP INDEX IF EXISTS codex_sessions_resume_lookup_idx;

ALTER TABLE codex_sessions
DROP COLUMN resumed_from_session_id;

ALTER TABLE codex_sessions
DROP COLUMN provider_session_id;

ALTER TABLE codex_sessions
DROP COLUMN provider_thread_id;

ALTER TABLE codex_sessions
DROP COLUMN agent_backend_kind;

ALTER TABLE codex_sessions
DROP COLUMN agent_role;

ALTER TABLE codex_sessions
DROP COLUMN agent_backend_id;

ALTER TABLE codex_sessions
DROP COLUMN requested_model;

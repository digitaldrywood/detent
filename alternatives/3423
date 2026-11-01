-- +goose Up
ALTER TABLE codex_sessions
ADD COLUMN work_attempt_id INTEGER;

ALTER TABLE codex_sessions
ADD COLUMN agent_route TEXT;

ALTER TABLE codex_sessions
ADD COLUMN provider TEXT;

ALTER TABLE codex_sessions
ADD COLUMN provider_provenance TEXT;

ALTER TABLE codex_sessions
ADD COLUMN requested_model_provenance TEXT;

ALTER TABLE codex_sessions
ADD COLUMN model_provenance TEXT;

ALTER TABLE codex_sessions
ADD COLUMN reasoning_effort TEXT;

ALTER TABLE codex_sessions
ADD COLUMN reasoning_effort_provenance TEXT;

ALTER TABLE codex_sessions
ADD COLUMN service_tier TEXT;

ALTER TABLE codex_sessions
ADD COLUMN service_tier_provenance TEXT;

ALTER TABLE codex_sessions
ADD COLUMN identity_observed_at TEXT;

CREATE INDEX codex_sessions_work_attempt_idx
ON codex_sessions(work_attempt_id, id);

ALTER TABLE work_attempts
ADD COLUMN detent_session_id INTEGER;

ALTER TABLE work_attempts
ADD COLUMN provider_session_id TEXT;

ALTER TABLE work_attempts
ADD COLUMN runtime_identity_json TEXT NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE work_attempts
DROP COLUMN runtime_identity_json;

ALTER TABLE work_attempts
DROP COLUMN provider_session_id;

ALTER TABLE work_attempts
DROP COLUMN detent_session_id;

DROP INDEX IF EXISTS codex_sessions_work_attempt_idx;

ALTER TABLE codex_sessions
DROP COLUMN identity_observed_at;

ALTER TABLE codex_sessions
DROP COLUMN service_tier_provenance;

ALTER TABLE codex_sessions
DROP COLUMN service_tier;

ALTER TABLE codex_sessions
DROP COLUMN reasoning_effort_provenance;

ALTER TABLE codex_sessions
DROP COLUMN reasoning_effort;

ALTER TABLE codex_sessions
DROP COLUMN model_provenance;

ALTER TABLE codex_sessions
DROP COLUMN requested_model_provenance;

ALTER TABLE codex_sessions
DROP COLUMN provider_provenance;

ALTER TABLE codex_sessions
DROP COLUMN provider;

ALTER TABLE codex_sessions
DROP COLUMN agent_route;

ALTER TABLE codex_sessions
DROP COLUMN work_attempt_id;

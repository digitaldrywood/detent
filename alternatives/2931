-- +goose Up
ALTER TABLE codex_sessions
ADD COLUMN orphan_recovery_outcome TEXT;

CREATE INDEX codex_sessions_orphan_lookup_idx
ON codex_sessions(work_attempt_id, final_state, completed_at, started_at DESC, id DESC);

-- +goose Down
DROP INDEX IF EXISTS codex_sessions_orphan_lookup_idx;

ALTER TABLE codex_sessions
DROP COLUMN orphan_recovery_outcome;

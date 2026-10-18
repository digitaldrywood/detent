-- +goose Up
ALTER TABLE codex_sessions ADD COLUMN worker_pid INTEGER;
ALTER TABLE codex_sessions ADD COLUMN worker_pgid INTEGER;
ALTER TABLE codex_sessions ADD COLUMN worker_started_at TEXT;
ALTER TABLE codex_sessions ADD COLUMN worker_reaped_at TEXT;
ALTER TABLE codex_sessions ADD COLUMN worker_reap_outcome TEXT;

CREATE INDEX codex_sessions_active_worker_idx
ON codex_sessions(completed_at, worker_reaped_at, worker_pid);

-- +goose Down
DROP INDEX IF EXISTS codex_sessions_active_worker_idx;

ALTER TABLE codex_sessions DROP COLUMN worker_reap_outcome;
ALTER TABLE codex_sessions DROP COLUMN worker_reaped_at;
ALTER TABLE codex_sessions DROP COLUMN worker_started_at;
ALTER TABLE codex_sessions DROP COLUMN worker_pgid;
ALTER TABLE codex_sessions DROP COLUMN worker_pid;

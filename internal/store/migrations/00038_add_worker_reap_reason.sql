-- +goose Up
ALTER TABLE codex_sessions ADD COLUMN worker_reap_reason TEXT;
CREATE INDEX idx_codex_sessions_unreaped_worker_processes
ON codex_sessions(worker_reaped_at, worker_pid)
WHERE worker_reaped_at IS NULL AND worker_pid > 0;

-- +goose Down
DROP INDEX idx_codex_sessions_unreaped_worker_processes;
ALTER TABLE codex_sessions DROP COLUMN worker_reap_reason;

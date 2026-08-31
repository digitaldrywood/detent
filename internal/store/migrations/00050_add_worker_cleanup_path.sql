-- +goose Up
ALTER TABLE codex_sessions ADD COLUMN worker_cleanup_root TEXT;
ALTER TABLE codex_sessions ADD COLUMN worker_cleanup_path TEXT;

-- +goose Down
ALTER TABLE codex_sessions DROP COLUMN worker_cleanup_path;
ALTER TABLE codex_sessions DROP COLUMN worker_cleanup_root;

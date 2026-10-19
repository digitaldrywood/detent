-- +goose Up
ALTER TABLE codex_sessions ADD COLUMN turn_start_refused INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE codex_sessions DROP COLUMN turn_start_refused;

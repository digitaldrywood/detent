-- +goose Up
ALTER TABLE codex_sessions
ADD COLUMN orphan_recovery_fallback_reason TEXT;

-- +goose Down
ALTER TABLE codex_sessions
DROP COLUMN orphan_recovery_fallback_reason;

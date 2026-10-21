-- +goose Up
ALTER TABLE staleness_warning_states ADD COLUMN last_seen_at TEXT;

UPDATE staleness_warning_states
SET last_seen_at = COALESCE(acknowledged_at, reminded_at)
WHERE last_seen_at IS NULL;

-- +goose Down
ALTER TABLE staleness_warning_states DROP COLUMN last_seen_at;

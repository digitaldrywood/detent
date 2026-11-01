-- +goose Up
ALTER TABLE usage_events ADD COLUMN cpu_seconds REAL;
ALTER TABLE usage_events ADD COLUMN avg_memory_bytes REAL;
ALTER TABLE usage_events ADD COLUMN wall_seconds REAL;
ALTER TABLE usage_events ADD COLUMN compute_usd REAL;

-- +goose Down
ALTER TABLE usage_events DROP COLUMN compute_usd;
ALTER TABLE usage_events DROP COLUMN wall_seconds;
ALTER TABLE usage_events DROP COLUMN avg_memory_bytes;
ALTER TABLE usage_events DROP COLUMN cpu_seconds;

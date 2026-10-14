-- +goose Up
ALTER TABLE usage_events
ADD COLUMN cost_usd REAL NOT NULL DEFAULT 0.0;

-- +goose Down
ALTER TABLE usage_events
DROP COLUMN cost_usd;

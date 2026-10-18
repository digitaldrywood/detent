-- +goose Up
ALTER TABLE usage_events
ADD COLUMN projected_cost_usd REAL;

ALTER TABLE usage_events
ADD COLUMN projection_overshoot_usd REAL NOT NULL DEFAULT 0.0;

-- +goose Down
ALTER TABLE usage_events
DROP COLUMN projection_overshoot_usd;

ALTER TABLE usage_events
DROP COLUMN projected_cost_usd;

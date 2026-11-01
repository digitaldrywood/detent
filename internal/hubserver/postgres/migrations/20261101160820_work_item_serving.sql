-- +goose Up
ALTER TABLE issues ADD COLUMN last_activity_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP;

-- +goose Down
ALTER TABLE issues DROP COLUMN last_activity_at;

-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE issues ADD COLUMN terminal_entered_at timestamptz;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE issues DROP COLUMN terminal_entered_at;

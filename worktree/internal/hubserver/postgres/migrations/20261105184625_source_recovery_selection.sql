-- +goose Up
SET LOCAL lock_timeout = '2s';
ALTER TABLE issues ADD COLUMN recovery_runner_id text NOT NULL DEFAULT '', ADD COLUMN recovery_version_id text NOT NULL DEFAULT '';

-- +goose Down
SET LOCAL lock_timeout = '2s';
ALTER TABLE issues DROP COLUMN recovery_runner_id, DROP COLUMN recovery_version_id;

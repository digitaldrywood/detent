-- +goose Up
ALTER TABLE issues ADD COLUMN recovery_runner_id TEXT NOT NULL DEFAULT '';
ALTER TABLE issues ADD COLUMN recovery_version_id TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE issues DROP COLUMN recovery_version_id;
ALTER TABLE issues DROP COLUMN recovery_runner_id;

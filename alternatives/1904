-- +goose Up
ALTER TABLE hosted_plans ADD COLUMN runtime_id TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE hosted_plans DROP COLUMN runtime_id;

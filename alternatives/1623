-- +goose Up
ALTER TABLE runner_identities ADD COLUMN removed_at TEXT;

-- +goose Down
ALTER TABLE runner_identities DROP COLUMN removed_at;

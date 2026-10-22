-- +goose Up
ALTER TABLE runner_identities ADD COLUMN home_dry_since TEXT;

-- +goose Down
ALTER TABLE runner_identities DROP COLUMN home_dry_since;

-- +goose Up
ALTER TABLE runner_identities ADD COLUMN local_checks_json TEXT NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE runner_identities DROP COLUMN local_checks_json;

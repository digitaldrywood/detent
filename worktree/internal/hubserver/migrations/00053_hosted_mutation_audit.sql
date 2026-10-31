-- +goose Up
ALTER TABLE hosted_audit ADD COLUMN mutation_json TEXT NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE hosted_audit DROP COLUMN mutation_json;

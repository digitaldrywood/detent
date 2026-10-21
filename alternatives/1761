-- +goose Up
ALTER TABLE runner_identities ADD COLUMN capacity_configuration_json TEXT NOT NULL DEFAULT 'null' CHECK (json_valid(capacity_configuration_json));

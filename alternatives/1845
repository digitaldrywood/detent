-- +goose Up
ALTER TABLE runner_identities ADD COLUMN update_observation_json TEXT NOT NULL DEFAULT 'null' CHECK (json_valid(update_observation_json));

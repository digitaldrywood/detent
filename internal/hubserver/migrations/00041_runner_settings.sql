-- +goose Up
ALTER TABLE runner_identities ADD COLUMN routing_settings_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(routing_settings_json));

-- +goose Down
ALTER TABLE runner_identities DROP COLUMN routing_settings_json;

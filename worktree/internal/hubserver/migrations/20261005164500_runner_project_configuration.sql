-- +goose Up
ALTER TABLE runner_identities ADD COLUMN project_configuration_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(project_configuration_json));

-- +goose Down
ALTER TABLE runner_identities DROP COLUMN project_configuration_json;

-- +goose Up
ALTER TABLE runner_identities ADD COLUMN problems_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(problems_json));
ALTER TABLE runner_identities ADD COLUMN reported_protocol_major INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runner_identities ADD COLUMN settings_rejected INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE runner_identities DROP COLUMN settings_rejected;
ALTER TABLE runner_identities DROP COLUMN reported_protocol_major;
ALTER TABLE runner_identities DROP COLUMN problems_json;

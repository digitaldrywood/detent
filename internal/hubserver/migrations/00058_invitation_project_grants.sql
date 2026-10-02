-- +goose Up
ALTER TABLE hosted_invitations ADD COLUMN grants_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(grants_json) AND json_type(grants_json) = 'array');

-- +goose Down
ALTER TABLE hosted_invitations DROP COLUMN grants_json;

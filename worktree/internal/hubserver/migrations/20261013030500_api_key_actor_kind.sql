-- +goose Up
ALTER TABLE api_tokens ADD COLUMN key_kind TEXT NOT NULL DEFAULT 'service' CHECK (key_kind IN ('personal', 'service'));
UPDATE api_tokens SET key_kind = 'personal'
WHERE hosted_user_id IS NOT NULL AND hosted_membership_id IS NOT NULL
AND operator_key_scope IS NOT NULL
AND scope IN ('operator', 'admin') AND native_only = 1
AND NOT EXISTS (SELECT 1 FROM runner_identities r WHERE r.token_id = api_tokens.id);

-- +goose Down
ALTER TABLE api_tokens DROP COLUMN key_kind;

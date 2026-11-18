-- +goose Up
ALTER TABLE platform.organizations
    ADD COLUMN hosted_configuration_key text CHECK (hosted_configuration_key = '' OR hosted_configuration_key ~ '^platform/entry/sha256/[0-9a-f]{64}$'),
    ALTER COLUMN workspace_policy_key DROP DEFAULT;

-- +goose Down
ALTER TABLE platform.organizations
    DROP COLUMN hosted_configuration_key,
    ALTER COLUMN workspace_policy_key SET DEFAULT '';

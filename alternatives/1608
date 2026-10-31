-- +goose Up
ALTER TABLE api_tokens ADD COLUMN hosted_user_id TEXT REFERENCES hosted_members(user_id);
ALTER TABLE api_tokens ADD COLUMN hosted_organization_id TEXT REFERENCES organizations(id);
ALTER TABLE api_tokens ADD COLUMN hosted_membership_id TEXT;
ALTER TABLE api_tokens ADD COLUMN operator_key_scope TEXT CHECK (operator_key_scope IN ('read', 'write', 'admin'));

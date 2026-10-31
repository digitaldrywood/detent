-- +goose Up
CREATE TABLE organization_key_policy_next (
    organization_id TEXT PRIMARY KEY,
    personal_keys TEXT NOT NULL DEFAULT 'allowed' CHECK (personal_keys IN ('allowed','approval','blocked'))
);
INSERT INTO organization_key_policy_next SELECT organization_id, CASE WHEN allow_external_keys=1 THEN 'allowed' ELSE 'blocked' END FROM organization_key_policy;
DROP TABLE organization_key_policy;
ALTER TABLE organization_key_policy_next RENAME TO organization_key_policy;
ALTER TABLE access_key_organizations ADD COLUMN approved_at TEXT;

-- +goose Down
ALTER TABLE organization_key_policy RENAME TO organization_key_policy_next;
CREATE TABLE organization_key_policy (
    organization_id TEXT PRIMARY KEY,
    allow_external_keys INTEGER NOT NULL DEFAULT 1 CHECK (allow_external_keys IN (0,1))
);
INSERT INTO organization_key_policy SELECT organization_id, CASE WHEN personal_keys='allowed' THEN 1 ELSE 0 END FROM organization_key_policy_next;
DROP TABLE organization_key_policy_next;
ALTER TABLE access_key_organizations DROP COLUMN approved_at;

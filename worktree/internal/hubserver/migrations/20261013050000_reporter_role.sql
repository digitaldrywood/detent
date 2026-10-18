-- +goose Up
PRAGMA legacy_alter_table = ON;
CREATE TABLE hosted_members_reporter (
  user_id TEXT PRIMARY KEY NOT NULL,
  email TEXT NOT NULL,
  membership_id TEXT NOT NULL UNIQUE,
  role TEXT NOT NULL CHECK (role IN ('owner', 'admin', 'member', 'reporter', 'viewer')),
  active INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0, 1)),
  principal_id TEXT NOT NULL UNIQUE REFERENCES api_tokens(id),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
INSERT INTO hosted_members_reporter SELECT * FROM hosted_members;
DROP TABLE hosted_members;
ALTER TABLE hosted_members_reporter RENAME TO hosted_members;
CREATE TABLE hosted_invitations_reporter (
  id TEXT PRIMARY KEY NOT NULL,
  email TEXT NOT NULL,
  organization_id TEXT NOT NULL REFERENCES organizations(id),
  role TEXT NOT NULL CHECK (role IN ('owner', 'admin', 'member', 'reporter', 'viewer')),
  accepted_user_id TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL DEFAULT '',
  grants_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(grants_json) AND json_type(grants_json) = 'array')
);
INSERT INTO hosted_invitations_reporter SELECT * FROM hosted_invitations;
DROP TABLE hosted_invitations;
ALTER TABLE hosted_invitations_reporter RENAME TO hosted_invitations;
PRAGMA legacy_alter_table = OFF;

-- +goose Down
SELECT 1;

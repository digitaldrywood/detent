-- +goose Up
CREATE TABLE access_keys (
    id TEXT PRIMARY KEY,
    token_hash TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('personal','service')),
    owner_subject TEXT NOT NULL,
    owner_email TEXT NOT NULL,
    permission TEXT NOT NULL CHECK (permission IN ('read','write','admin')),
    access_context TEXT NOT NULL CHECK (access_context IN ('global','selected','project')),
    organizations_json TEXT NOT NULL,
    service_organization_id TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    expires_at TEXT,
    revoked_at TEXT
);
CREATE INDEX access_keys_owner ON access_keys(owner_subject, created_at);
CREATE TABLE organization_key_policy (
    organization_id TEXT PRIMARY KEY,
    allow_external_keys INTEGER NOT NULL DEFAULT 1 CHECK (allow_external_keys IN (0,1))
);
CREATE TABLE access_key_organizations (
    key_id TEXT NOT NULL REFERENCES access_keys(id) ON DELETE CASCADE,
    organization_id TEXT NOT NULL,
    blocked INTEGER NOT NULL DEFAULT 0 CHECK (blocked IN (0,1)),
    blocked_projects_json TEXT NOT NULL DEFAULT '[]',
    reached_at TEXT,
    last_used_at TEXT,
    PRIMARY KEY (key_id, organization_id)
);
CREATE TABLE access_key_notifications (
    id INTEGER PRIMARY KEY,
    owner_subject TEXT NOT NULL,
    key_id TEXT NOT NULL REFERENCES access_keys(id) ON DELETE CASCADE,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);

-- +goose Down
DROP TABLE access_key_notifications;
DROP TABLE access_key_organizations;
DROP TABLE organization_key_policy;
DROP TABLE access_keys;

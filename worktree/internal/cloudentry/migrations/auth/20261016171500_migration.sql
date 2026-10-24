-- +goose Up
CREATE TABLE oauth_clients (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    auth_method TEXT NOT NULL CHECK (auth_method IN ('none','client_secret_post','client_secret_basic')),
    secret_hash TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE oauth_grants (
    id TEXT PRIMARY KEY,
    client_id TEXT NOT NULL REFERENCES oauth_clients(id),
    owner_subject TEXT NOT NULL,
    owner_email TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    permission TEXT NOT NULL CHECK (permission IN ('read','write','admin')),
    resource TEXT NOT NULL,
    created_at TEXT NOT NULL,
    last_used_at TEXT,
    revoked_at TEXT,
    access_hash TEXT UNIQUE,
    access_expires_at TEXT
);
CREATE INDEX oauth_grants_owner ON oauth_grants(owner_subject, created_at, id);
CREATE TABLE oauth_requests (
    id TEXT PRIMARY KEY,
    client_id TEXT NOT NULL REFERENCES oauth_clients(id),
    redirect_uri TEXT NOT NULL,
    challenge TEXT NOT NULL,
    resource TEXT NOT NULL,
    code_hash TEXT NOT NULL UNIQUE,
    code_expires_at TEXT NOT NULL,
    grant_id TEXT NOT NULL REFERENCES oauth_grants(id),
    consumed_at TEXT
);
CREATE TABLE oauth_refresh_tokens (
    token_hash TEXT PRIMARY KEY,
    grant_id TEXT NOT NULL UNIQUE REFERENCES oauth_grants(id),
    expires_at TEXT NOT NULL
);

-- +goose Down
DROP TABLE oauth_refresh_tokens;
DROP TABLE oauth_requests;
DROP TABLE oauth_grants;
DROP TABLE oauth_clients;

-- +goose Up
CREATE TABLE sessions (
  token_hash TEXT PRIMARY KEY,
  subject TEXT NOT NULL,
  email TEXT NOT NULL,
  identity_json TEXT NOT NULL CHECK (json_valid(identity_json)),
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  revoked_at TEXT
);

CREATE TABLE authorizations (
  binding TEXT PRIMARY KEY,
  session_hash TEXT NOT NULL REFERENCES sessions(token_hash),
  organization_id TEXT NOT NULL,
  identity_json TEXT NOT NULL CHECK (json_valid(identity_json)),
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  revoked_at TEXT
);
CREATE UNIQUE INDEX authorizations_active ON authorizations(session_hash, organization_id) WHERE revoked_at IS NULL;

CREATE TABLE transactions (
  token_hash TEXT PRIMARY KEY,
  transaction_id TEXT NOT NULL UNIQUE,
  state TEXT NOT NULL,
  verifier TEXT NOT NULL,
  organization_id TEXT NOT NULL DEFAULT '',
  return_path TEXT NOT NULL DEFAULT '',
  invitation_token TEXT NOT NULL DEFAULT '',
  invitation_organization TEXT NOT NULL DEFAULT '',
  expires_at TEXT NOT NULL,
  consumed_at TEXT
);

CREATE TABLE audit (
  id INTEGER PRIMARY KEY,
  subject TEXT NOT NULL,
  organization_id TEXT NOT NULL,
  event TEXT NOT NULL,
  recorded_at TEXT NOT NULL
);

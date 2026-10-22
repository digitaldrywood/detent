-- +goose Up
CREATE TABLE api_tokens (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  token_hash TEXT NOT NULL UNIQUE CHECK (length(token_hash) = 64),
  token_fingerprint TEXT NOT NULL,
  scope TEXT NOT NULL CHECK (scope IN ('worker', 'operator', 'admin')),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  last_used_at TEXT,
  rotated_at TEXT,
  revoked_at TEXT
);

CREATE INDEX api_tokens_status_idx
ON api_tokens(revoked_at, scope, id);

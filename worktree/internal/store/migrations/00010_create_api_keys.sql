-- +goose Up
CREATE TABLE api_keys (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  prefix_last4 TEXT NOT NULL,
  key_hash TEXT NOT NULL UNIQUE,
  scopes TEXT NOT NULL,
  project_ids TEXT NOT NULL DEFAULT '[]',
  created_at TEXT NOT NULL,
  expires_at TEXT,
  last_used_at TEXT,
  revoked_at TEXT
);

CREATE INDEX api_keys_created_at_idx
ON api_keys(created_at DESC, id DESC);

CREATE INDEX api_keys_status_idx
ON api_keys(revoked_at, expires_at);

CREATE TABLE api_usage_logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  api_key_id TEXT NOT NULL,
  method TEXT NOT NULL,
  path TEXT NOT NULL,
  status_code INTEGER NOT NULL,
  latency_ms INTEGER NOT NULL,
  ip TEXT NOT NULL,
  user_agent TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE INDEX api_usage_logs_key_created_idx
ON api_usage_logs(api_key_id, created_at DESC);

CREATE INDEX api_usage_logs_created_idx
ON api_usage_logs(created_at DESC);

-- +goose Down
DROP INDEX IF EXISTS api_usage_logs_created_idx;
DROP INDEX IF EXISTS api_usage_logs_key_created_idx;
DROP TABLE IF EXISTS api_usage_logs;

DROP INDEX IF EXISTS api_keys_status_idx;
DROP INDEX IF EXISTS api_keys_created_at_idx;
DROP TABLE IF EXISTS api_keys;

-- +goose Up
CREATE TABLE github_webhook_inbox (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  delivery_id TEXT NOT NULL UNIQUE,
  repository_id INTEGER REFERENCES repositories(id) ON DELETE SET NULL,
  event_type TEXT NOT NULL,
  action TEXT,
  headers_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(headers_json)),
  payload_json TEXT NOT NULL CHECK (json_valid(payload_json)),
  payload_sha256 TEXT NOT NULL,
  payload_bytes INTEGER NOT NULL CHECK (payload_bytes >= 0),
  status TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  next_retry_at TEXT,
  processing_started_at TEXT,
  processed_at TEXT,
  last_error TEXT,
  received_at TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE INDEX github_webhook_inbox_processing_idx
ON github_webhook_inbox(status, next_retry_at, received_at, id);

CREATE TABLE github_outbox (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  idempotency_key TEXT NOT NULL UNIQUE,
  repository_id INTEGER NOT NULL REFERENCES repositories(id),
  issue_id INTEGER REFERENCES issues(id),
  mutation_kind TEXT NOT NULL,
  target_node_id TEXT,
  desired_json TEXT NOT NULL CHECK (json_valid(desired_json)),
  status TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  next_retry_at TEXT,
  last_attempt_at TEXT,
  completed_at TEXT,
  terminal_error TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE INDEX github_outbox_retry_idx
ON github_outbox(status, next_retry_at, id);

CREATE TABLE sync_checkpoints (
  repository_id INTEGER NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  checkpoint_name TEXT NOT NULL,
  cursor TEXT,
  state_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(state_json)),
  last_started_at TEXT,
  last_successful_at TEXT,
  last_error TEXT,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (repository_id, checkpoint_name)
);

CREATE INDEX sync_checkpoints_success_idx
ON sync_checkpoints(last_successful_at, repository_id, checkpoint_name);

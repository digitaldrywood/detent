-- +goose Up
ALTER TABLE repositories ADD COLUMN source_version TEXT NOT NULL DEFAULT '';
ALTER TABLE repositories ADD COLUMN source_updated_at TEXT;
ALTER TABLE repositories ADD COLUMN synchronized_at TEXT;

DROP INDEX github_webhook_inbox_processing_idx;

ALTER TABLE github_webhook_inbox RENAME TO github_webhook_inbox_legacy;

CREATE TABLE github_webhook_inbox (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  delivery_id TEXT NOT NULL UNIQUE,
  repository_id INTEGER REFERENCES repositories(id) ON DELETE SET NULL,
  event_type TEXT NOT NULL,
  action TEXT,
  headers_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(headers_json)),
  payload_sha256 TEXT NOT NULL,
  payload_bytes INTEGER NOT NULL CHECK (payload_bytes >= 0),
  status TEXT NOT NULL CHECK (status IN ('pending', 'processing', 'processed', 'ignored', 'failed')),
  outcome TEXT,
  attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  next_retry_at TEXT,
  processing_started_at TEXT,
  processed_at TEXT,
  last_error TEXT,
  signature_verified_at TEXT,
  received_at TEXT NOT NULL,
  last_received_at TEXT NOT NULL,
  redelivery_count INTEGER NOT NULL DEFAULT 0 CHECK (redelivery_count >= 0),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

INSERT INTO github_webhook_inbox (
  id,
  delivery_id,
  repository_id,
  event_type,
  action,
  headers_json,
  payload_sha256,
  payload_bytes,
  status,
  attempts,
  next_retry_at,
  processing_started_at,
  processed_at,
  last_error,
  received_at,
  last_received_at,
  created_at,
  updated_at
)
SELECT
  id,
  delivery_id,
  repository_id,
  event_type,
  action,
  headers_json,
  payload_sha256,
  payload_bytes,
  status,
  attempts,
  next_retry_at,
  processing_started_at,
  processed_at,
  last_error,
  received_at,
  received_at,
  created_at,
  updated_at
FROM github_webhook_inbox_legacy;

CREATE INDEX github_webhook_inbox_processing_idx
ON github_webhook_inbox(status, next_retry_at, received_at, id);

CREATE TABLE github_webhook_payloads (
  inbox_id INTEGER PRIMARY KEY REFERENCES github_webhook_inbox(id) ON DELETE CASCADE,
  body BLOB NOT NULL,
  expires_at TEXT NOT NULL,
  created_at TEXT NOT NULL
);

INSERT INTO github_webhook_payloads (inbox_id, body, expires_at, created_at)
SELECT id, CAST(payload_json AS BLOB), created_at, created_at
FROM github_webhook_inbox_legacy;

CREATE INDEX github_webhook_payloads_expiry_idx
ON github_webhook_payloads(expires_at, inbox_id);

DROP TABLE github_webhook_inbox_legacy;

CREATE TABLE github_hydration_requests (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  repository_id INTEGER REFERENCES repositories(id) ON DELETE SET NULL,
  repository_full_name TEXT NOT NULL,
  object_kind TEXT NOT NULL CHECK (object_kind IN ('issue', 'pull_request', 'pull_request_checks', 'pull_request_reviews', 'commit_checks')),
  object_key TEXT NOT NULL,
  github_node_id TEXT,
  github_number INTEGER CHECK (github_number IS NULL OR github_number > 0),
  head_sha TEXT,
  reason TEXT NOT NULL,
  requested_source_updated_at TEXT,
  requested_source_version TEXT NOT NULL,
  first_delivery_id TEXT REFERENCES github_webhook_inbox(delivery_id) ON DELETE SET NULL,
  last_delivery_id TEXT REFERENCES github_webhook_inbox(delivery_id) ON DELETE SET NULL,
  status TEXT NOT NULL CHECK (status IN ('pending', 'processing', 'completed', 'failed')),
  request_count INTEGER NOT NULL DEFAULT 1 CHECK (request_count > 0),
  attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  next_retry_at TEXT,
  processing_started_at TEXT,
  completed_at TEXT,
  last_error TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  CHECK (github_node_id IS NOT NULL OR github_number IS NOT NULL OR head_sha IS NOT NULL)
);

CREATE UNIQUE INDEX github_hydration_requests_pending_target_idx
ON github_hydration_requests(repository_full_name, object_kind, object_key)
WHERE status = 'pending';

CREATE INDEX github_hydration_requests_processing_idx
ON github_hydration_requests(status, next_retry_at, id);

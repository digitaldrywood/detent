-- +goose Up
CREATE TABLE organization_secrets (
  organization_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  kind TEXT NOT NULL,
  ciphertext BLOB NOT NULL,
  nonce BLOB NOT NULL,
  wrapped_data_key BLOB NOT NULL,
  master_key_version INTEGER NOT NULL CHECK (master_key_version > 0),
  updated_at TEXT NOT NULL,
  PRIMARY KEY (organization_id, kind)
);
CREATE TABLE organization_secret_audit (
  id INTEGER PRIMARY KEY,
  organization_id TEXT NOT NULL,
  actor TEXT NOT NULL,
  kind TEXT NOT NULL,
  key_version INTEGER NOT NULL,
  event TEXT NOT NULL CHECK (event IN ('set', 'replace', 'remove', 'use', 'rotate')),
  recorded_at TEXT NOT NULL
);
CREATE TABLE slack_integrations (
  organization_id TEXT PRIMARY KEY REFERENCES organizations(id) ON DELETE CASCADE,
  channel_name TEXT NOT NULL DEFAULT '',
  last_success_at TEXT,
  last_failure_at TEXT,
  last_status_code INTEGER NOT NULL DEFAULT 0,
  last_delivery_failed INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE health_slack_deliveries (
  finding_id TEXT NOT NULL REFERENCES health_findings(id) ON DELETE CASCADE,
  event TEXT NOT NULL CHECK (event IN ('opened', 'resolved')),
  queued_at TEXT NOT NULL,
  next_attempt_at TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 2),
  completed_at TEXT,
  PRIMARY KEY (finding_id, event)
);
CREATE INDEX health_slack_pending ON health_slack_deliveries(next_attempt_at) WHERE completed_at IS NULL AND attempts < 2;
ALTER TABLE health_findings ADD COLUMN slack_unavailable_at TEXT;
ALTER TABLE health_findings ADD COLUMN slack_status_code INTEGER;

-- +goose Down
ALTER TABLE health_findings DROP COLUMN slack_status_code;
ALTER TABLE health_findings DROP COLUMN slack_unavailable_at;
DROP TABLE health_slack_deliveries;
DROP TABLE slack_integrations;
DROP TABLE organization_secret_audit;
DROP TABLE organization_secrets;

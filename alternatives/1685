-- +goose Up
CREATE TABLE native_attempts (
  id TEXT PRIMARY KEY,
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  work_item_id TEXT NOT NULL,
  lease_id TEXT NOT NULL UNIQUE REFERENCES leases(lease_id),
  fencing_token INTEGER NOT NULL UNIQUE CHECK (fencing_token > 0),
  run_id TEXT NOT NULL,
  sequence INTEGER NOT NULL CHECK (sequence > 0),
  status TEXT NOT NULL CHECK (status IN ('running', 'succeeded', 'failed', 'cancelled', 'interrupted')),
  data_json TEXT NOT NULL CHECK (json_valid(data_json)),
  checkpoint_json TEXT CHECK (checkpoint_json IS NULL OR json_valid(checkpoint_json)),
  artifact_ids_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(artifact_ids_json)),
  started_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY (organization_id, project_id, work_item_id) REFERENCES issues(organization_id, project_id, native_id)
);
CREATE INDEX native_attempts_page_idx ON native_attempts(organization_id, project_id, work_item_id, fencing_token);
CREATE TABLE native_attempt_events (
  attempt_id TEXT NOT NULL REFERENCES native_attempts(id),
  sequence INTEGER NOT NULL CHECK (sequence > 0),
  request_hash TEXT NOT NULL,
  PRIMARY KEY (attempt_id, sequence)
);

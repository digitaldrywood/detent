-- +goose Up
CREATE TABLE release_audit_checkpoints (
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  id TEXT NOT NULL,
  repository TEXT NOT NULL,
  commit_sha TEXT NOT NULL,
  release_series TEXT NOT NULL,
  run_id TEXT NOT NULL,
  run_attempt TEXT NOT NULL,
  job_id TEXT NOT NULL,
  run_url TEXT NOT NULL,
  sequence INTEGER NOT NULL,
  status TEXT NOT NULL,
  bundle_commit TEXT NOT NULL,
  bundle_digest TEXT NOT NULL,
  allowance_invocations INTEGER NOT NULL,
  allowance_input INTEGER NOT NULL,
  allowance_output INTEGER NOT NULL,
  allowance_cached INTEGER NOT NULL,
  invocations INTEGER NOT NULL,
  input_tokens INTEGER NOT NULL,
  output_tokens INTEGER NOT NULL,
  cached_input_tokens INTEGER NOT NULL,
  digest TEXT NOT NULL,
  bytes INTEGER NOT NULL,
  recorded_at TEXT NOT NULL,
  PRIMARY KEY (organization_id, project_id, id),
  UNIQUE (organization_id, project_id, run_id, run_attempt, job_id, sequence),
  FOREIGN KEY (organization_id, project_id) REFERENCES projects(organization_id, id)
);

CREATE TABLE release_audit_parts (
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  checkpoint_id TEXT NOT NULL,
  part_index INTEGER NOT NULL,
  attachment_id TEXT NOT NULL,
  byte_offset INTEGER NOT NULL,
  bytes INTEGER NOT NULL,
  digest TEXT NOT NULL,
  PRIMARY KEY (organization_id, project_id, checkpoint_id, part_index),
  FOREIGN KEY (organization_id, project_id, checkpoint_id) REFERENCES release_audit_checkpoints(organization_id, project_id, id),
  FOREIGN KEY (attachment_id) REFERENCES attachments(id)
);

CREATE INDEX release_audit_parts_attachment ON release_audit_parts(attachment_id);

-- +goose Down
DROP TABLE release_audit_parts;
DROP TABLE release_audit_checkpoints;

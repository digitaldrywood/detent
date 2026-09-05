-- +goose Up
ALTER TABLE api_tokens ADD COLUMN expires_at TEXT;

CREATE TABLE runner_enrollments (
  id TEXT PRIMARY KEY,
  organization_id TEXT NOT NULL REFERENCES organizations(id),
  runner_id TEXT NOT NULL,
  machine_id TEXT NOT NULL,
  token_hash TEXT NOT NULL UNIQUE CHECK (length(token_hash) = 64),
  operations_json TEXT NOT NULL CHECK (json_valid(operations_json)),
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  created_by TEXT NOT NULL REFERENCES api_tokens(id),
  redeemed_at TEXT,
  revoked_at TEXT
);

CREATE TABLE runner_enrollment_projects (
  enrollment_id TEXT NOT NULL REFERENCES runner_enrollments(id),
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  PRIMARY KEY (enrollment_id, project_id),
  FOREIGN KEY (organization_id, project_id) REFERENCES projects(organization_id, id)
);

CREATE TABLE runner_identities (
  id TEXT PRIMARY KEY,
  organization_id TEXT NOT NULL REFERENCES organizations(id),
  machine_id TEXT NOT NULL UNIQUE REFERENCES machines(id),
  token_id TEXT NOT NULL UNIQUE REFERENCES api_tokens(id),
  enrollment_id TEXT NOT NULL UNIQUE REFERENCES runner_enrollments(id),
  operations_json TEXT NOT NULL CHECK (json_valid(operations_json)),
  created_at TEXT NOT NULL
);

CREATE TABLE runner_identity_events (
  id INTEGER PRIMARY KEY,
  runner_id TEXT NOT NULL REFERENCES runner_identities(id),
  actor_id TEXT NOT NULL REFERENCES api_tokens(id),
  kind TEXT NOT NULL CHECK (kind IN ('enrolled', 'renewed', 'rotated', 'revoked')),
  occurred_at TEXT NOT NULL
);

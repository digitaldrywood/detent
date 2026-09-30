-- +goose Up
CREATE TABLE project_secrets (
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  organization_slug TEXT NOT NULL,
  ciphertext BLOB NOT NULL,
  nonce BLOB NOT NULL,
  wrapped_data_key BLOB NOT NULL,
  master_key_version INTEGER NOT NULL CHECK (master_key_version > 0),
  updated_at TEXT NOT NULL,
  PRIMARY KEY (organization_id, project_id, kind),
  FOREIGN KEY (organization_id, project_id) REFERENCES projects(organization_id, id) ON DELETE CASCADE
);
CREATE TABLE project_secret_audit (
  id INTEGER PRIMARY KEY,
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  actor TEXT NOT NULL,
  kind TEXT NOT NULL,
  key_version INTEGER NOT NULL,
  event TEXT NOT NULL CHECK (event IN ('set', 'replace', 'remove', 'use', 'rotate')),
  recorded_at TEXT NOT NULL
);

-- +goose Down
DROP TABLE project_secret_audit;
DROP TABLE project_secrets;

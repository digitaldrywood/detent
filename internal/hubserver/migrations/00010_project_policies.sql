-- +goose Up
CREATE TABLE policy_revisions (
  scope TEXT NOT NULL,
  policy_id TEXT NOT NULL,
  metadata_json TEXT NOT NULL CHECK (json_valid(metadata_json)),
  approved_by TEXT NOT NULL,
  approved_at TEXT NOT NULL,
  PRIMARY KEY (scope, policy_id)
);
CREATE TABLE project_policies (
  scope TEXT PRIMARY KEY,
  policy_id TEXT NOT NULL,
  FOREIGN KEY (scope, policy_id) REFERENCES policy_revisions(scope, policy_id)
);
CREATE TABLE lease_policies (
  lease_id TEXT PRIMARY KEY REFERENCES leases(lease_id),
  scope TEXT NOT NULL,
  policy_id TEXT NOT NULL,
  FOREIGN KEY (scope, policy_id) REFERENCES policy_revisions(scope, policy_id)
);

-- +goose Down
DROP TABLE lease_policies;
DROP TABLE project_policies;
DROP TABLE policy_revisions;

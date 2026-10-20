-- +goose Up
CREATE TABLE landing_barriers (
  organization_id TEXT NOT NULL REFERENCES organizations(id),
  repository TEXT NOT NULL,
  project_id TEXT NOT NULL REFERENCES projects(id),
  record_json TEXT NOT NULL CHECK (json_valid(record_json)),
  PRIMARY KEY (organization_id, repository)
);
CREATE TABLE landing_barrier_receipts (
  sequence INTEGER PRIMARY KEY AUTOINCREMENT,
  organization_id TEXT NOT NULL REFERENCES organizations(id),
  project_id TEXT NOT NULL REFERENCES projects(id),
  repository TEXT NOT NULL,
  version_id TEXT NOT NULL UNIQUE REFERENCES change_versions(id),
  record_json TEXT NOT NULL CHECK (json_valid(record_json))
);
CREATE INDEX landing_barrier_repository ON landing_barrier_receipts(organization_id, repository, sequence);

-- +goose Down
DROP TABLE landing_barrier_receipts;
DROP TABLE landing_barriers;

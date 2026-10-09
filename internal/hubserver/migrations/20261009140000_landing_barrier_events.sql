-- +goose Up
CREATE TABLE landing_barrier_events (
  sequence INTEGER PRIMARY KEY AUTOINCREMENT,
  organization_id TEXT NOT NULL REFERENCES organizations(id),
  project_id TEXT NOT NULL REFERENCES projects(id),
  repository TEXT NOT NULL,
  record_json TEXT NOT NULL CHECK (json_valid(record_json))
);
CREATE INDEX landing_barrier_event_history ON landing_barrier_events(organization_id, project_id, repository, sequence);

-- +goose Down
DROP TABLE landing_barrier_events;

-- +goose Up
CREATE TABLE scheduled_validation_reports (
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  repository TEXT NOT NULL,
  run_id TEXT NOT NULL,
  run_attempt TEXT NOT NULL,
  record_json TEXT NOT NULL CHECK (json_valid(record_json)),
  recorded_at TEXT NOT NULL,
  PRIMARY KEY (organization_id, project_id, repository, run_id, run_attempt),
  FOREIGN KEY (organization_id, project_id) REFERENCES projects(organization_id, id)
);

CREATE INDEX scheduled_validation_reports_time ON scheduled_validation_reports(organization_id, project_id, recorded_at);

-- +goose Down
DROP TABLE scheduled_validation_reports;

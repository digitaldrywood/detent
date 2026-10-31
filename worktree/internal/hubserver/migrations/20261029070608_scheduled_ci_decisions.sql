-- +goose Up
CREATE TABLE scheduled_ci_decisions (
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  repository TEXT NOT NULL,
  head_sha TEXT NOT NULL,
  run_id TEXT NOT NULL,
  run_attempt TEXT NOT NULL,
  job_id TEXT NOT NULL,
  outcome TEXT NOT NULL CHECK (outcome IN ('source', 'infrastructure', 'unknown')),
  target TEXT NOT NULL,
  reason TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (organization_id, project_id, repository, run_id, run_attempt, job_id),
  FOREIGN KEY (project_id) REFERENCES projects(id)
);

-- +goose Down
DROP TABLE scheduled_ci_decisions;

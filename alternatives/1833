-- +goose Up
CREATE TABLE runner_project_capacity_limits (
  organization_id TEXT NOT NULL REFERENCES organizations(id),
  runner_id TEXT NOT NULL REFERENCES runner_identities(id) ON DELETE CASCADE,
  project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  capacity_limit INTEGER NOT NULL CHECK (typeof(capacity_limit) = 'integer' AND capacity_limit BETWEEN 0 AND 10000),
  PRIMARY KEY (organization_id, runner_id, project_id)
);

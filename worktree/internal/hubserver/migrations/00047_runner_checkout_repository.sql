-- +goose Up
ALTER TABLE projects ADD COLUMN checkout_repository TEXT NOT NULL DEFAULT '';

CREATE TABLE runner_checkout_repositories (
  runner_id TEXT NOT NULL REFERENCES runner_identities(id) ON DELETE CASCADE,
  project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  repository TEXT NOT NULL,
  reported_at TEXT NOT NULL,
  PRIMARY KEY (runner_id, project_id)
);

-- +goose Down
DROP TABLE runner_checkout_repositories;
ALTER TABLE projects DROP COLUMN checkout_repository;

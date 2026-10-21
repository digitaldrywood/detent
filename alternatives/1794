-- +goose Up
CREATE TABLE runner_project_skills (
  runner_id TEXT NOT NULL REFERENCES runner_identities(id) ON DELETE CASCADE,
  project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  skills_json TEXT NOT NULL CHECK (json_valid(skills_json)),
  reported_at TEXT NOT NULL,
  PRIMARY KEY (runner_id, project_id)
);

-- +goose Down
DROP TABLE runner_project_skills;

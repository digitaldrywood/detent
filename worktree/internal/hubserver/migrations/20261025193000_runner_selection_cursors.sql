-- +goose Up
CREATE TABLE runner_selection_cursors (
  runner_id TEXT NOT NULL REFERENCES runner_identities(id) ON DELETE CASCADE,
  project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  issue_id INTEGER NOT NULL,
  priority INTEGER NOT NULL,
  aging INTEGER NOT NULL,
  landing INTEGER NOT NULL,
  created TEXT NOT NULL,
  identifier TEXT NOT NULL,
  revision INTEGER NOT NULL,
  PRIMARY KEY (runner_id, project_id)
);

-- +goose Down
DROP TABLE runner_selection_cursors;

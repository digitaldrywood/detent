-- +goose Up
CREATE TABLE routine_runs (
  id INTEGER PRIMARY KEY,
  project_id TEXT NOT NULL,
  routine_name TEXT NOT NULL,
  scheduled_for TEXT NOT NULL,
  started_at TEXT NOT NULL,
  completed_at TEXT NOT NULL,
  filed_count INTEGER NOT NULL DEFAULT 0,
  deduplicated_count INTEGER NOT NULL DEFAULT 0,
  issues_json TEXT NOT NULL DEFAULT '[]',
  error TEXT
);

CREATE INDEX routine_runs_project_name_completed_idx
ON routine_runs(project_id, routine_name, completed_at DESC, id DESC);

-- +goose Down
DROP TABLE IF EXISTS routine_runs;

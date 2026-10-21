-- +goose Up
CREATE TABLE scheduled_runs (
  id INTEGER PRIMARY KEY,
  project_id TEXT NOT NULL,
  schedule_id TEXT NOT NULL,
  scheduled_for TEXT NOT NULL,
  started_at TEXT NOT NULL,
  completed_at TEXT NOT NULL,
  error TEXT
);

CREATE INDEX scheduled_runs_project_schedule_completed_idx
ON scheduled_runs(project_id, schedule_id, completed_at DESC, id DESC);

-- +goose Down
DROP TABLE IF EXISTS scheduled_runs;

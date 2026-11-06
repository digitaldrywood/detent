-- +goose Up
CREATE TABLE retro_runs (
  id INTEGER PRIMARY KEY,
  project_id TEXT NOT NULL,
  trigger TEXT NOT NULL,
  started_at TEXT NOT NULL,
  completed_at TEXT NOT NULL,
  findings_count INTEGER NOT NULL DEFAULT 0,
  filed_count INTEGER NOT NULL DEFAULT 0,
  updated_count INTEGER NOT NULL DEFAULT 0,
  error TEXT,
  event_day TEXT NOT NULL
);

CREATE INDEX retro_runs_project_completed_idx
ON retro_runs(project_id, completed_at DESC, id DESC);

CREATE INDEX retro_runs_project_day_idx
ON retro_runs(project_id, event_day);

-- +goose Down
DROP TABLE IF EXISTS retro_runs;

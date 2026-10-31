-- +goose Up
CREATE TABLE usage_events (
  id INTEGER PRIMARY KEY,
  project_id TEXT NOT NULL,
  run_id INTEGER,
  session_id INTEGER,
  issue_id TEXT,
  identifier TEXT,
  pr_number INTEGER,
  model TEXT NOT NULL DEFAULT '',
  input_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  total_tokens INTEGER NOT NULL DEFAULT 0,
  runtime_seconds INTEGER NOT NULL DEFAULT 0,
  started_at TEXT NOT NULL,
  finished_at TEXT NOT NULL,
  event_day TEXT NOT NULL,
  outcome TEXT NOT NULL
);

CREATE INDEX usage_events_project_issue_day_idx
ON usage_events(project_id, issue_id, event_day);

CREATE INDEX usage_events_project_identifier_day_idx
ON usage_events(project_id, identifier, event_day);

CREATE INDEX usage_events_project_day_idx
ON usage_events(project_id, event_day);

-- +goose Down
DROP TABLE IF EXISTS usage_events;

-- +goose Up
CREATE TABLE workflow_phase_events (
  id INTEGER PRIMARY KEY,
  project_id TEXT NOT NULL,
  run_id INTEGER,
  session_id INTEGER,
  issue_id TEXT,
  identifier TEXT,
  issue_url TEXT,
  pr_number INTEGER,
  phase_type TEXT NOT NULL,
  phase_name TEXT NOT NULL,
  previous_phase_name TEXT,
  reason TEXT,
  status TEXT,
  started_at TEXT NOT NULL,
  finished_at TEXT,
  duration_seconds INTEGER NOT NULL DEFAULT 0,
  event_day TEXT NOT NULL,
  command_name TEXT,
  exit_code INTEGER,
  turns INTEGER NOT NULL DEFAULT 0,
  input_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  total_tokens INTEGER NOT NULL DEFAULT 0,
  endpoint_family TEXT,
  metadata_json TEXT NOT NULL DEFAULT '{}'
);

CREATE INDEX workflow_phase_events_project_phase_day_idx
ON workflow_phase_events(project_id, phase_type, phase_name, event_day);

CREATE INDEX workflow_phase_events_issue_idx
ON workflow_phase_events(issue_id, identifier, issue_url);

CREATE INDEX workflow_phase_events_finished_at_idx
ON workflow_phase_events(finished_at, id);

-- +goose Down
DROP TABLE IF EXISTS workflow_phase_events;

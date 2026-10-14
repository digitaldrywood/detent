-- +goose Up
CREATE TABLE work_attempts (
  id INTEGER PRIMARY KEY,
  project_id TEXT NOT NULL,
  issue_id TEXT,
  identifier TEXT,
  issue_url TEXT,
  pr_number INTEGER,
  repo TEXT,
  worker_type TEXT NOT NULL,
  worker_host TEXT,
  lane TEXT,
  attempt_number INTEGER NOT NULL DEFAULT 1,
  status TEXT NOT NULL,
  started_at TEXT NOT NULL,
  lease_expires_at TEXT,
  heartbeat_at TEXT,
  completed_at TEXT,
  terminal_state TEXT,
  error_class TEXT,
  error_message TEXT,
  phase TEXT,
  status_message TEXT,
  current_step INTEGER,
  total_steps INTEGER,
  progress_percent INTEGER,
  current_command TEXT,
  wait_reason TEXT,
  github_rate_snapshot_json TEXT NOT NULL DEFAULT '{}',
  ci_state TEXT,
  capacity_snapshot_json TEXT NOT NULL DEFAULT '{}',
  worker_metadata_json TEXT NOT NULL DEFAULT '{}',
  metrics_json TEXT NOT NULL DEFAULT '{}',
  next_action TEXT
);

CREATE INDEX work_attempts_active_project_idx
ON work_attempts(project_id, completed_at, lease_expires_at);

CREATE INDEX work_attempts_issue_idx
ON work_attempts(issue_id, identifier, issue_url);

CREATE TABLE scheduler_decisions (
  id INTEGER PRIMARY KEY,
  project_id TEXT NOT NULL,
  issue_id TEXT,
  identifier TEXT,
  issue_url TEXT,
  pr_number INTEGER,
  repo TEXT,
  lane TEXT,
  queue_position INTEGER NOT NULL DEFAULT 0,
  result TEXT NOT NULL,
  reason TEXT,
  selected INTEGER NOT NULL DEFAULT 0,
  retry INTEGER NOT NULL DEFAULT 0,
  attempt_number INTEGER NOT NULL DEFAULT 0,
  worker_host TEXT,
  decision_at TEXT NOT NULL,
  wait_reason TEXT,
  capacity_snapshot_json TEXT NOT NULL DEFAULT '{}',
  github_rate_snapshot_json TEXT NOT NULL DEFAULT '{}',
  metadata_json TEXT NOT NULL DEFAULT '{}'
);

CREATE INDEX scheduler_decisions_project_at_idx
ON scheduler_decisions(project_id, decision_at DESC, id DESC);

CREATE INDEX scheduler_decisions_issue_idx
ON scheduler_decisions(issue_id, identifier, issue_url);

-- +goose Down
DROP TABLE IF EXISTS scheduler_decisions;
DROP TABLE IF EXISTS work_attempts;

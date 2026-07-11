-- +goose Up
CREATE TABLE efficiency_receipts (
  id INTEGER PRIMARY KEY,
  project_id TEXT NOT NULL,
  issue_id TEXT NOT NULL,
  identifier TEXT,
  issue_url TEXT,
  pr_number INTEGER,
  sessions INTEGER NOT NULL DEFAULT 0,
  attempts INTEGER NOT NULL DEFAULT 0,
  input_tokens INTEGER NOT NULL DEFAULT 0,
  cached_input_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  reasoning_output_tokens INTEGER NOT NULL DEFAULT 0,
  total_tokens INTEGER NOT NULL DEFAULT 0,
  estimated_cost_usd REAL NOT NULL DEFAULT 0,
  first_dispatched_at TEXT NOT NULL,
  completed_at TEXT NOT NULL,
  wall_seconds INTEGER NOT NULL DEFAULT 0,
  working_seconds INTEGER NOT NULL DEFAULT 0,
  gate_wait_seconds INTEGER NOT NULL DEFAULT 0,
  merge_train_seconds INTEGER NOT NULL DEFAULT 0,
  parked_seconds INTEGER NOT NULL DEFAULT 0,
  redispatches INTEGER NOT NULL DEFAULT 0,
  breaker_trips INTEGER NOT NULL DEFAULT 0,
  ci_reruns INTEGER NOT NULL DEFAULT 0,
  tokens_baseline REAL NOT NULL DEFAULT 0,
  sessions_baseline REAL NOT NULL DEFAULT 0,
  dwell_baseline_seconds REAL NOT NULL DEFAULT 0,
  tokens_anomaly INTEGER NOT NULL DEFAULT 0,
  sessions_anomaly INTEGER NOT NULL DEFAULT 0,
  dwell_anomaly INTEGER NOT NULL DEFAULT 0,
  UNIQUE(project_id, issue_id)
);

CREATE INDEX efficiency_receipts_project_completed_idx
ON efficiency_receipts(project_id, completed_at DESC, id DESC);

-- +goose Down
DROP TABLE IF EXISTS efficiency_receipts;

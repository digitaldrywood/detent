-- +goose Up
CREATE TABLE detent_runs (
  id INTEGER PRIMARY KEY,
  started_at TEXT NOT NULL,
  stopped_at TEXT,
  restart_reason TEXT,
  peak_concurrent_agents INTEGER NOT NULL DEFAULT 0,
  sessions_launched INTEGER NOT NULL DEFAULT 0,
  input_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  total_tokens INTEGER NOT NULL DEFAULT 0,
  runtime_seconds INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE codex_sessions (
  id INTEGER PRIMARY KEY,
  run_id INTEGER,
  issue_id TEXT,
  identifier TEXT,
  issue_url TEXT,
  started_at TEXT,
  completed_at TEXT,
  turns INTEGER NOT NULL DEFAULT 0,
  input_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  total_tokens INTEGER NOT NULL DEFAULT 0,
  runtime_seconds INTEGER NOT NULL DEFAULT 0,
  final_state TEXT,
  model TEXT,
  FOREIGN KEY(run_id) REFERENCES detent_runs(id)
);

CREATE INDEX codex_sessions_completed_at_idx
ON codex_sessions(completed_at DESC, id DESC);

CREATE INDEX codex_sessions_identifier_idx
ON codex_sessions(identifier);

-- +goose Down
DROP TABLE IF EXISTS codex_sessions;
DROP TABLE IF EXISTS detent_runs;

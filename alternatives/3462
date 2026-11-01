-- +goose Up
CREATE TABLE backlog_admission_malformed_results (
  id INTEGER PRIMARY KEY,
  project_id TEXT NOT NULL,
  issue_id TEXT NOT NULL,
  issue_identifier TEXT NOT NULL DEFAULT '',
  issue_url TEXT NOT NULL DEFAULT '',
  candidate_fingerprint TEXT NOT NULL,
  prompt_fingerprint TEXT NOT NULL,
  proposal_fingerprint TEXT NOT NULL,
  error_fingerprint TEXT NOT NULL,
  error_class TEXT NOT NULL,
  error_code TEXT NOT NULL,
  output_excerpt TEXT NOT NULL DEFAULT '',
  attempt_count INTEGER NOT NULL DEFAULT 1,
  status TEXT NOT NULL DEFAULT 'retryable',
  first_seen_at TEXT NOT NULL,
  last_seen_at TEXT NOT NULL,
  resolved_at TEXT,
  UNIQUE (project_id, proposal_fingerprint, error_fingerprint),
  CHECK (error_class IN ('model', 'parse', 'schema', 'transport')),
  CHECK (attempt_count > 0),
  CHECK (status IN ('retryable', 'blocked', 'resolved'))
);

CREATE INDEX backlog_admission_malformed_blocked_idx
ON backlog_admission_malformed_results(project_id, proposal_fingerprint, status);

ALTER TABLE backlog_admission_runs
ADD COLUMN malformed_json TEXT NOT NULL DEFAULT '[]';

-- +goose Down
ALTER TABLE backlog_admission_runs DROP COLUMN malformed_json;
DROP TABLE IF EXISTS backlog_admission_malformed_results;

-- +goose Up
CREATE TABLE backlog_admission_proposals (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  issue_id TEXT NOT NULL,
  issue_identifier TEXT NOT NULL DEFAULT '',
  issue_url TEXT NOT NULL DEFAULT '',
  target_state TEXT NOT NULL,
  fingerprint TEXT NOT NULL,
  criteria_section TEXT NOT NULL,
  criteria_text TEXT NOT NULL,
  findings_json TEXT NOT NULL,
  confidence REAL NOT NULL,
  status TEXT NOT NULL,
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  resolved_at TEXT,
  commented_at TEXT,
  CHECK (status IN ('open', 'accepted', 'rejected', 'expired', 'superseded')),
  CHECK (confidence >= 0.0 AND confidence <= 1.0)
);

CREATE UNIQUE INDEX backlog_admission_proposals_open_fingerprint_idx
ON backlog_admission_proposals(project_id, issue_id, target_state, fingerprint)
WHERE status = 'open';

CREATE INDEX backlog_admission_proposals_open_project_expiry_idx
ON backlog_admission_proposals(project_id, expires_at, id)
WHERE status = 'open';

CREATE INDEX backlog_admission_proposals_issue_history_idx
ON backlog_admission_proposals(project_id, issue_id, created_at DESC, id DESC);

CREATE TABLE backlog_admission_runs (
  id INTEGER PRIMARY KEY,
  project_id TEXT NOT NULL,
  scheduled_for TEXT NOT NULL,
  started_at TEXT NOT NULL,
  completed_at TEXT NOT NULL,
  outcome TEXT NOT NULL,
  deferred_reason TEXT,
  candidates_found_count INTEGER NOT NULL DEFAULT 0,
  candidates_count INTEGER NOT NULL DEFAULT 0,
  proposed_count INTEGER NOT NULL DEFAULT 0,
  skipped_json TEXT NOT NULL DEFAULT '{}',
  truncated_json TEXT NOT NULL DEFAULT '{}',
  issues_json TEXT NOT NULL DEFAULT '[]',
  error TEXT
);

CREATE INDEX backlog_admission_runs_project_completed_idx
ON backlog_admission_runs(project_id, completed_at DESC, id DESC);

-- +goose Down
DROP TABLE IF EXISTS backlog_admission_runs;
DROP TABLE IF EXISTS backlog_admission_proposals;

-- +goose Up
CREATE TABLE backlog_admission_declines (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  issue_id TEXT NOT NULL,
  issue_identifier TEXT NOT NULL DEFAULT '',
  issue_url TEXT NOT NULL DEFAULT '',
  fingerprint TEXT NOT NULL,
  reason TEXT NOT NULL,
  detail TEXT NOT NULL,
  created_at TEXT NOT NULL,
  commented_at TEXT
);

CREATE UNIQUE INDEX backlog_admission_declines_issue_fingerprint_idx
ON backlog_admission_declines(project_id, issue_id, fingerprint);

CREATE INDEX backlog_admission_declines_project_created_idx
ON backlog_admission_declines(project_id, created_at DESC, id DESC);

-- +goose Down
DROP TABLE IF EXISTS backlog_admission_declines;

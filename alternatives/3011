-- +goose Up
CREATE TABLE issue_progress_credits (
  project_id TEXT NOT NULL,
  issue_key TEXT NOT NULL,
  issue_id TEXT,
  identifier TEXT,
  issue_url TEXT,
  credited_at TEXT NOT NULL,
  PRIMARY KEY (project_id, issue_key)
);

CREATE INDEX issue_progress_credits_identity_idx
ON issue_progress_credits(project_id, issue_id, identifier, issue_url);

-- +goose Down
DROP TABLE IF EXISTS issue_progress_credits;

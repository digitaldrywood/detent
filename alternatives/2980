-- +goose Up
CREATE TABLE validator_verdicts (
  id INTEGER PRIMARY KEY,
  project_id TEXT NOT NULL,
  issue_id TEXT NOT NULL,
  head_sha TEXT NOT NULL,
  identifier TEXT,
  issue_url TEXT,
  pr_number INTEGER,
  submitted INTEGER NOT NULL DEFAULT 0,
  verdict TEXT NOT NULL,
  score REAL NOT NULL DEFAULT 0,
  summary TEXT,
  findings_json TEXT NOT NULL DEFAULT '[]',
  commented INTEGER NOT NULL DEFAULT 0,
  recorded_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(project_id, issue_id, head_sha)
);

CREATE INDEX validator_verdicts_issue_idx
ON validator_verdicts(project_id, issue_id, updated_at DESC, id DESC);

-- +goose Down
DROP TABLE IF EXISTS validator_verdicts;

-- +goose Up
CREATE TABLE validator_verdicts_context (
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
  repository TEXT NOT NULL DEFAULT '',
  base_sha TEXT NOT NULL DEFAULT '',
  diff_digest TEXT NOT NULL DEFAULT '',
  diff_files_json TEXT NOT NULL DEFAULT '[]',
  failure_attempts INTEGER NOT NULL DEFAULT 0,
  next_retry_at TEXT,
  context_digest TEXT NOT NULL DEFAULT '',
  UNIQUE(project_id, issue_id, head_sha, repository, pr_number, base_sha, context_digest)
);

INSERT INTO validator_verdicts_context (id, project_id, issue_id, head_sha, identifier, issue_url, pr_number, submitted, verdict, score, summary, findings_json, commented, recorded_at, updated_at, repository, base_sha, diff_digest, diff_files_json, failure_attempts, next_retry_at) SELECT id, project_id, issue_id, head_sha, identifier, issue_url, pr_number, submitted, verdict, score, summary, findings_json, commented, recorded_at, updated_at, repository, base_sha, diff_digest, diff_files_json, failure_attempts, next_retry_at FROM validator_verdicts;
DROP TABLE validator_verdicts;
ALTER TABLE validator_verdicts_context RENAME TO validator_verdicts;
CREATE INDEX validator_verdicts_issue_idx ON validator_verdicts(project_id, issue_id, updated_at DESC, id DESC);

-- +goose Down
-- Context-qualified rows cannot safely be collapsed into the old head-only key.
SELECT * FROM validator_context_migration_is_forward_only;

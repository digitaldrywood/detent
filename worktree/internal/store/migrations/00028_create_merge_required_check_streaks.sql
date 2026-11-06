-- +goose Up
CREATE TABLE merge_required_check_streaks (
  project_id TEXT NOT NULL,
  issue_id TEXT NOT NULL,
  repository TEXT NOT NULL,
  pr_number INTEGER NOT NULL,
  check_name TEXT NOT NULL,
  required_checks_fingerprint TEXT NOT NULL,
  consecutive_missing INTEGER NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (project_id, issue_id, repository, pr_number, check_name),
  CHECK (pr_number > 0),
  CHECK (consecutive_missing > 0)
);

CREATE INDEX merge_required_check_streaks_issue_idx
ON merge_required_check_streaks(project_id, issue_id);

-- +goose Down
DROP TABLE IF EXISTS merge_required_check_streaks;

-- +goose Up
ALTER TABLE routine_runs ADD COLUMN proposed_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE routine_runs ADD COLUMN limited_count INTEGER NOT NULL DEFAULT 0;

CREATE TABLE routine_findings (
  project_id TEXT NOT NULL,
  routine_name TEXT NOT NULL,
  issue_id TEXT NOT NULL,
  identifier TEXT NOT NULL DEFAULT '',
  url TEXT NOT NULL DEFAULT '',
  open INTEGER NOT NULL DEFAULT 1 CHECK (open IN (0, 1)),
  PRIMARY KEY (project_id, routine_name, issue_id)
);

CREATE INDEX routine_findings_open_idx
ON routine_findings(project_id, routine_name, open, issue_id);

INSERT OR IGNORE INTO routine_findings (project_id, routine_name, issue_id, identifier, url)
SELECT
  routine_runs.project_id,
  routine_runs.routine_name,
  trim(json_extract(issue.value, '$.id')),
  COALESCE(trim(json_extract(issue.value, '$.identifier')), ''),
  COALESCE(trim(json_extract(issue.value, '$.url')), '')
FROM routine_runs, json_each(routine_runs.issues_json) AS issue
WHERE trim(json_extract(issue.value, '$.id')) != '';

-- +goose Down
DROP TABLE IF EXISTS routine_findings;
ALTER TABLE routine_runs DROP COLUMN limited_count;
ALTER TABLE routine_runs DROP COLUMN proposed_count;

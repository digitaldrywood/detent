-- +goose Up
CREATE TABLE project_dispatch_status (
  project_id TEXT PRIMARY KEY,
  candidate_count INTEGER NOT NULL DEFAULT 0,
  candidate_fingerprint TEXT NOT NULL DEFAULT '',
  selected_count INTEGER NOT NULL DEFAULT 0,
  skipped_count INTEGER NOT NULL DEFAULT 0,
  wait_reason TEXT,
  all_skipped_since TEXT,
  last_selected_at TEXT,
  observed_at TEXT NOT NULL
);

INSERT INTO project_dispatch_status (
  project_id,
  last_selected_at,
  observed_at
)
SELECT
  project_id,
  MAX(CASE WHEN selected = 1 OR result = 'selected' THEN decision_at END),
  MAX(decision_at)
FROM scheduler_decisions
GROUP BY project_id;

-- +goose Down
DROP TABLE IF EXISTS project_dispatch_status;

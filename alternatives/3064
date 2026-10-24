-- +goose Up
CREATE TABLE budget_overrides (
  project_id TEXT PRIMARY KEY,
  per_day_max_usd REAL,
  per_issue_max_usd REAL,
  expires_at TEXT NOT NULL,
  created_at TEXT NOT NULL,
  reason TEXT NOT NULL
);

CREATE INDEX budget_overrides_expires_at_idx
ON budget_overrides(expires_at);

-- +goose Down
DROP INDEX IF EXISTS budget_overrides_expires_at_idx;
DROP TABLE IF EXISTS budget_overrides;

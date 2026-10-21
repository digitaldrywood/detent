-- +goose Up
CREATE TABLE fair_share_usage (
  project_id TEXT PRIMARY KEY,
  weight INTEGER NOT NULL DEFAULT 1,
  dispatches INTEGER NOT NULL DEFAULT 0,
  runtime_seconds INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL
);

-- +goose Down
DROP TABLE IF EXISTS fair_share_usage;

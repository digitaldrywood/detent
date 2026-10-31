-- +goose Up
DROP TABLE fair_share_usage;

-- +goose Down
CREATE TABLE fair_share_usage (
 project_id TEXT PRIMARY KEY,
 dispatches INTEGER NOT NULL DEFAULT 0,
 runtime_seconds INTEGER NOT NULL DEFAULT 0,
 weight INTEGER NOT NULL DEFAULT 1,
 updated_at TEXT NOT NULL
);

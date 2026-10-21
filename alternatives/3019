-- +goose Up
CREATE TABLE staleness_warning_states (
  project_id TEXT NOT NULL,
  warning_id TEXT NOT NULL,
  reminded_at TEXT,
  acknowledged_at TEXT,
  PRIMARY KEY (project_id, warning_id)
);

-- +goose Down
DROP TABLE IF EXISTS staleness_warning_states;

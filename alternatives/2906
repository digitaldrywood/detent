-- +goose Up
CREATE TABLE issue_park_acknowledgements (
  project_id TEXT NOT NULL,
  issue_key TEXT NOT NULL,
  issue_id TEXT,
  identifier TEXT,
  issue_url TEXT,
  park_sequence INTEGER NOT NULL,
  acknowledged_at TEXT NOT NULL,
  PRIMARY KEY (project_id, issue_key)
);

CREATE INDEX issue_park_acknowledgements_identity_idx
ON issue_park_acknowledgements(project_id, issue_id, identifier, issue_url);

-- +goose Down
DROP TABLE IF EXISTS issue_park_acknowledgements;

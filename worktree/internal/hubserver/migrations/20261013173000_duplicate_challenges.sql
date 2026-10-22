-- +goose Up
ALTER TABLE projects ADD COLUMN duplicate_challenge INTEGER NOT NULL DEFAULT 1 CHECK (duplicate_challenge IN (0, 1));

CREATE TABLE native_duplicate_challenges (
  token TEXT PRIMARY KEY,
  organization_id TEXT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  principal_id TEXT NOT NULL,
  request_id TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  matches_json TEXT NOT NULL,
  consumed_at TEXT,
  created_at TEXT NOT NULL
);

CREATE TABLE issue_relationships (
  first_issue_id INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
  second_issue_id INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
  relationship_type TEXT NOT NULL CHECK (relationship_type = 'related'),
  created_at TEXT NOT NULL,
  PRIMARY KEY (first_issue_id, second_issue_id, relationship_type),
  CHECK (first_issue_id < second_issue_id)
);
CREATE INDEX issue_relationships_second ON issue_relationships(second_issue_id, first_issue_id);

-- +goose Down
DROP TABLE issue_relationships;
DROP TABLE native_duplicate_challenges;
ALTER TABLE projects DROP COLUMN duplicate_challenge;

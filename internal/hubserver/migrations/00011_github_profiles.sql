-- +goose Up
CREATE TABLE collaboration_events_import (
  id TEXT PRIMARY KEY,
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  work_item_id TEXT NOT NULL,
  sequence INTEGER NOT NULL CHECK (sequence > 0),
  type TEXT NOT NULL CHECK (type IN ('issue.created', 'issue.edited', 'comment.created', 'comment.edited', 'dependency.changed', 'workflow.transitioned', 'run.started', 'run.finished', 'run.checkpointed', 'github.imported', 'comment.imported', 'issue.cutover')),
  schema_version INTEGER NOT NULL CHECK (schema_version = 1),
  actor_json TEXT NOT NULL CHECK (json_valid(actor_json)),
  data_json TEXT NOT NULL CHECK (json_valid(data_json)),
  recorded_at TEXT NOT NULL,
  UNIQUE (organization_id, work_item_id, sequence),
  FOREIGN KEY (organization_id, project_id, work_item_id) REFERENCES issues(organization_id, project_id, native_id)
);

INSERT INTO collaboration_events_import SELECT * FROM collaboration_events;
DROP TABLE collaboration_events;
ALTER TABLE collaboration_events_import RENAME TO collaboration_events;
-- +goose StatementBegin
CREATE TRIGGER collaboration_events_no_update BEFORE UPDATE ON collaboration_events
BEGIN
  SELECT RAISE(ABORT, 'collaboration history is append-only');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER collaboration_events_no_delete BEFORE DELETE ON collaboration_events
BEGIN
  SELECT RAISE(ABORT, 'collaboration history is append-only');
END;
-- +goose StatementEnd


ALTER TABLE projects ADD COLUMN integration_revision INTEGER NOT NULL DEFAULT 1;
ALTER TABLE projects ADD COLUMN github_intake TEXT NOT NULL DEFAULT 'disabled' CHECK (github_intake IN ('disabled', 'manual'));
ALTER TABLE projects ADD COLUMN github_projection TEXT NOT NULL DEFAULT 'disabled' CHECK (github_projection IN ('disabled', 'summary'));
ALTER TABLE projects ADD COLUMN github_repository_enabled INTEGER NOT NULL DEFAULT 1 CHECK (github_repository_enabled IN (0, 1));
UPDATE projects SET github_repository_enabled = 0 WHERE profile = 'native';

CREATE TABLE github_imports (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL REFERENCES projects(id),
  issue_number INTEGER NOT NULL CHECK (issue_number > 0),
  work_item_id TEXT REFERENCES issues(native_id),
  stage TEXT NOT NULL DEFAULT 'issue',
  cursor TEXT NOT NULL DEFAULT '',
	edit_sequence INTEGER NOT NULL DEFAULT 0,
  revision INTEGER NOT NULL DEFAULT 1,
  pages INTEGER NOT NULL DEFAULT 0,
	intake_pending INTEGER NOT NULL DEFAULT 0 CHECK (intake_pending IN (0, 1)),
  status TEXT NOT NULL DEFAULT 'pending',
  gaps_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(gaps_json)),
  last_error TEXT NOT NULL DEFAULT '',
  retry_after TEXT,
  source_updated_at TEXT NOT NULL DEFAULT '',
  observed_at TEXT NOT NULL,
  UNIQUE (project_id, issue_number)
);

CREATE TABLE github_import_records (
  sequence INTEGER PRIMARY KEY,
  import_id TEXT NOT NULL REFERENCES github_imports(id),
  source_key TEXT NOT NULL,
  kind TEXT NOT NULL,
  record_json TEXT NOT NULL CHECK (json_valid(record_json)),
  observed_at TEXT NOT NULL,
  UNIQUE (import_id, source_key)
);

CREATE TABLE github_cutovers (
  project_id TEXT PRIMARY KEY REFERENCES projects(id),
  checkpoint TEXT NOT NULL,
  receipt_json TEXT NOT NULL CHECK (json_valid(receipt_json)),
  actor_id TEXT NOT NULL,
  created_at TEXT NOT NULL
);

-- +goose Down
DROP TABLE github_cutovers;
DROP TABLE github_import_records;
DROP TABLE github_imports;
ALTER TABLE projects DROP COLUMN github_repository_enabled;
ALTER TABLE projects DROP COLUMN github_projection;
ALTER TABLE projects DROP COLUMN github_intake;
ALTER TABLE projects DROP COLUMN integration_revision;

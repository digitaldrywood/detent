-- +goose Up
CREATE TABLE collaboration_events_runtime (
  id TEXT PRIMARY KEY,
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  work_item_id TEXT NOT NULL,
  sequence INTEGER NOT NULL CHECK (sequence > 0),
  type TEXT NOT NULL CHECK (type IN ('issue.created', 'issue.edited', 'comment.created', 'comment.edited', 'dependency.changed', 'workflow.transitioned', 'run.started', 'run.finished', 'run.checkpointed', 'github.imported', 'comment.imported', 'issue.cutover', 'change.created', 'change.version_published', 'run.observed', 'scheduler.decision')),
  schema_version INTEGER NOT NULL CHECK (schema_version = 1),
  actor_json TEXT NOT NULL CHECK (json_valid(actor_json)),
  data_json TEXT NOT NULL CHECK (json_valid(data_json)),
  recorded_at TEXT NOT NULL,
  UNIQUE (organization_id, work_item_id, sequence),
  FOREIGN KEY (organization_id, project_id, work_item_id) REFERENCES issues(organization_id, project_id, native_id)
);
INSERT INTO collaboration_events_runtime SELECT * FROM collaboration_events;
DROP TABLE collaboration_events;
ALTER TABLE collaboration_events_runtime RENAME TO collaboration_events;
-- +goose StatementBegin
CREATE TRIGGER collaboration_events_no_update BEFORE UPDATE ON collaboration_events
BEGIN
  SELECT RAISE(ABORT, 'collaboration events are immutable');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER collaboration_events_no_delete BEFORE DELETE ON collaboration_events
BEGIN
  SELECT RAISE(ABORT, 'collaboration events are immutable');
END;
-- +goose StatementEnd

-- +goose Down
CREATE TABLE collaboration_events_prior (
  id TEXT PRIMARY KEY,
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  work_item_id TEXT NOT NULL,
  sequence INTEGER NOT NULL CHECK (sequence > 0),
  type TEXT NOT NULL CHECK (type IN ('issue.created', 'issue.edited', 'comment.created', 'comment.edited', 'dependency.changed', 'workflow.transitioned', 'run.started', 'run.finished', 'run.checkpointed', 'github.imported', 'comment.imported', 'issue.cutover', 'change.created', 'change.version_published')),
  schema_version INTEGER NOT NULL CHECK (schema_version = 1),
  actor_json TEXT NOT NULL CHECK (json_valid(actor_json)),
  data_json TEXT NOT NULL CHECK (json_valid(data_json)),
  recorded_at TEXT NOT NULL,
  UNIQUE (organization_id, work_item_id, sequence),
  FOREIGN KEY (organization_id, project_id, work_item_id) REFERENCES issues(organization_id, project_id, native_id)
);
INSERT INTO collaboration_events_prior SELECT * FROM collaboration_events;
DROP TABLE collaboration_events;
ALTER TABLE collaboration_events_prior RENAME TO collaboration_events;
-- +goose StatementBegin
CREATE TRIGGER collaboration_events_no_update BEFORE UPDATE ON collaboration_events
BEGIN
  SELECT RAISE(ABORT, 'collaboration events are immutable');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER collaboration_events_no_delete BEFORE DELETE ON collaboration_events
BEGIN
  SELECT RAISE(ABORT, 'collaboration events are immutable');
END;
-- +goose StatementEnd

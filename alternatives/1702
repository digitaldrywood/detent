-- +goose Up
CREATE TABLE collaboration_events_changes (
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
INSERT INTO collaboration_events_changes SELECT * FROM collaboration_events;
DROP TABLE collaboration_events;
ALTER TABLE collaboration_events_changes RENAME TO collaboration_events;
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

CREATE TABLE change_review_policies (
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  policy_json TEXT NOT NULL CHECK (json_valid(policy_json)),
  PRIMARY KEY (organization_id, project_id),
  FOREIGN KEY (organization_id, project_id) REFERENCES projects(organization_id, id)
);

CREATE TABLE change_requests (
  id TEXT PRIMARY KEY,
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  work_item_id TEXT NOT NULL,
  record_json TEXT NOT NULL CHECK (json_valid(record_json)),
  FOREIGN KEY (organization_id, project_id, work_item_id) REFERENCES issues(organization_id, project_id, native_id)
);
CREATE INDEX change_requests_scope_idx ON change_requests(organization_id, project_id, work_item_id, id);

CREATE TABLE change_issue_links (
  change_id TEXT NOT NULL REFERENCES change_requests(id),
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  work_item_id TEXT NOT NULL,
  PRIMARY KEY (change_id, work_item_id),
  FOREIGN KEY (organization_id, project_id, work_item_id) REFERENCES issues(organization_id, project_id, native_id)
);

CREATE TABLE change_versions (
  id TEXT PRIMARY KEY,
  change_id TEXT NOT NULL REFERENCES change_requests(id),
  number INTEGER NOT NULL CHECK (number > 0),
  record_json TEXT NOT NULL CHECK (json_valid(record_json)),
  UNIQUE (change_id, number)
);

CREATE TABLE change_evidence (
  sequence INTEGER PRIMARY KEY,
  change_id TEXT NOT NULL REFERENCES change_requests(id),
  version_id TEXT REFERENCES change_versions(id),
  kind TEXT NOT NULL CHECK (kind IN ('review', 'check', 'discussion')),
  source_key TEXT,
  record_json TEXT NOT NULL CHECK (json_valid(record_json)),
  UNIQUE (change_id, kind, source_key)
);
CREATE INDEX change_evidence_page_idx ON change_evidence(change_id, sequence);

-- +goose StatementBegin
CREATE TRIGGER change_versions_immutable BEFORE UPDATE ON change_versions
BEGIN
  SELECT RAISE(ABORT, 'change versions are immutable');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER change_versions_no_delete BEFORE DELETE ON change_versions
BEGIN
  SELECT RAISE(ABORT, 'change versions are immutable');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER change_evidence_immutable BEFORE UPDATE ON change_evidence
BEGIN
  SELECT RAISE(ABORT, 'change evidence is immutable');
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER change_evidence_no_delete BEFORE DELETE ON change_evidence
BEGIN
  SELECT RAISE(ABORT, 'change evidence is immutable');
END;
-- +goose StatementEnd

-- +goose Up
CREATE TABLE hub_identity (
  id TEXT PRIMARY KEY,
  cursor_key BLOB NOT NULL CHECK (length(cursor_key) = 32)
);
INSERT INTO hub_identity VALUES ('hub_' || lower(hex(randomblob(16))), randomblob(32));

CREATE TABLE organizations (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  local INTEGER NOT NULL DEFAULT 0 CHECK (local IN (0, 1)),
  created_at TEXT NOT NULL
);
CREATE UNIQUE INDEX organizations_local_idx ON organizations(local) WHERE local = 1;
INSERT INTO organizations VALUES ('org_' || lower(hex(randomblob(16))), 'Local organization', 1, strftime('%Y-%m-%dT%H:%M:%SZ', 'now'));

CREATE TABLE projects (
  id TEXT PRIMARY KEY DEFAULT ('prj_' || lower(hex(randomblob(16)))),
  organization_id TEXT NOT NULL REFERENCES organizations(id),
  repository_id INTEGER UNIQUE REFERENCES repositories(id),
  name TEXT NOT NULL,
  profile TEXT NOT NULL CHECK (profile IN ('native', 'github_compatible')),
  states_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(states_json)),
  require_dependencies INTEGER NOT NULL DEFAULT 1 CHECK (require_dependencies IN (0, 1)),
  created_at TEXT NOT NULL,
  UNIQUE (organization_id, id),
  UNIQUE (organization_id, name)
);
INSERT INTO projects (organization_id, repository_id, name, profile, created_at)
SELECT o.id, r.id, r.github_owner || '/' || r.github_name, 'github_compatible', r.created_at
FROM repositories r CROSS JOIN organizations o WHERE o.local = 1;

CREATE TABLE workflow_states_native (
  id INTEGER PRIMARY KEY,
  repository_id INTEGER REFERENCES repositories(id) ON DELETE CASCADE,
  project_id TEXT REFERENCES projects(id),
  github_node_id TEXT UNIQUE,
  source_name TEXT NOT NULL,
  detent_state TEXT NOT NULL,
  terminal INTEGER NOT NULL DEFAULT 0 CHECK (terminal IN (0, 1)),
  dispatchable INTEGER NOT NULL DEFAULT 0 CHECK (dispatchable IN (0, 1)),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE (repository_id, source_name),
  UNIQUE (project_id, source_name)
);

INSERT INTO workflow_states_native (id, repository_id, github_node_id, source_name, detent_state, terminal, dispatchable, created_at, updated_at)
SELECT id, repository_id, github_node_id, source_name, detent_state, terminal, dispatchable, created_at, updated_at FROM workflow_states;
DROP TABLE workflow_states;
ALTER TABLE workflow_states_native RENAME TO workflow_states;

CREATE TABLE issues_native (
  id INTEGER PRIMARY KEY,
  repository_id INTEGER REFERENCES repositories(id) ON DELETE CASCADE,
  workflow_state_id INTEGER REFERENCES workflow_states(id) ON DELETE SET NULL,
  github_node_id TEXT UNIQUE,
  github_database_id INTEGER UNIQUE,
  github_number INTEGER CHECK (github_number > 0),
  native_id TEXT NOT NULL UNIQUE DEFAULT ('wi_' || lower(hex(randomblob(16)))),
  organization_id TEXT REFERENCES organizations(id),
  project_id TEXT,
  number INTEGER CHECK (number > 0),
  revision INTEGER NOT NULL DEFAULT 1 CHECK (revision > 0),
  event_sequence INTEGER NOT NULL DEFAULT 0 CHECK (event_sequence >= 0),
  actor_json TEXT NOT NULL DEFAULT '{"kind":"integration","principal_id":"github-import"}' CHECK (json_valid(actor_json)),
  provenance_json TEXT CHECK (provenance_json IS NULL OR json_valid(provenance_json)),
  native_source_key TEXT,
  native_created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
  native_updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
  author_login TEXT NOT NULL DEFAULT '',
  title TEXT NOT NULL,
  body TEXT NOT NULL DEFAULT '',
  url TEXT NOT NULL,
  github_state TEXT NOT NULL,
  labels_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(labels_json)),
  assignees_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(assignees_json)),
  source_version TEXT NOT NULL,
  source_updated_at TEXT NOT NULL,
  synchronized_at TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE (repository_id, github_number),
  UNIQUE (organization_id, project_id, native_id),
  UNIQUE (project_id, number),
  UNIQUE (project_id, native_source_key),
  FOREIGN KEY (organization_id, project_id) REFERENCES projects(organization_id, id)
);

INSERT INTO issues_native (id, repository_id, workflow_state_id, github_node_id, github_database_id, github_number, title, body, url, github_state, labels_json, assignees_json, source_version, source_updated_at, synchronized_at, created_at, updated_at, author_login) SELECT id, repository_id, workflow_state_id, github_node_id, github_database_id, github_number, title, body, url, github_state, labels_json, assignees_json, source_version, source_updated_at, synchronized_at, created_at, updated_at, author_login FROM issues;
DROP TABLE issues;
ALTER TABLE issues_native RENAME TO issues;
UPDATE issues SET organization_id = (SELECT organization_id FROM projects WHERE repository_id = issues.repository_id),
  project_id = (SELECT id FROM projects WHERE repository_id = issues.repository_id), number = github_number;
CREATE INDEX issues_native_scope_idx ON issues(organization_id, project_id, number);

-- +goose StatementBegin
CREATE TRIGGER issues_observed_change AFTER UPDATE OF title, body, labels_json, assignees_json, workflow_state_id, source_updated_at ON issues
WHEN NEW.github_node_id IS NOT NULL
BEGIN
  UPDATE issues SET native_updated_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now') WHERE id = NEW.id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER repositories_project_alias AFTER INSERT ON repositories
BEGIN
  INSERT INTO projects (organization_id, repository_id, name, profile, created_at)
  SELECT id, NEW.id, NEW.github_owner || '/' || NEW.github_name, 'github_compatible', NEW.created_at FROM organizations WHERE local = 1;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER issues_native_alias AFTER INSERT ON issues WHEN NEW.project_id IS NULL
BEGIN
  SELECT CASE WHEN NEW.repository_id IS NULL THEN RAISE(ABORT, 'issue scope is required') END;
  UPDATE issues SET organization_id = (SELECT organization_id FROM projects WHERE repository_id = NEW.repository_id),
    project_id = (SELECT id FROM projects WHERE repository_id = NEW.repository_id), number = NEW.github_number WHERE id = NEW.id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER issues_immutable_identity BEFORE UPDATE OF native_id, organization_id, project_id, number ON issues
WHEN OLD.project_id IS NOT NULL AND (NEW.native_id IS NOT OLD.native_id OR NEW.organization_id IS NOT OLD.organization_id OR NEW.project_id IS NOT OLD.project_id OR NEW.number IS NOT OLD.number)
BEGIN
  SELECT RAISE(ABORT, 'issue identity is immutable');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dependency_organization_insert BEFORE INSERT ON issue_dependencies
WHEN (SELECT organization_id FROM issues WHERE id = NEW.blocker_issue_id) IS NOT (SELECT organization_id FROM issues WHERE id = NEW.dependent_issue_id)
BEGIN
  SELECT RAISE(ABORT, 'dependency organization mismatch');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dependency_organization_update BEFORE UPDATE OF blocker_issue_id, dependent_issue_id ON issue_dependencies
WHEN (SELECT organization_id FROM issues WHERE id = NEW.blocker_issue_id) IS NOT (SELECT organization_id FROM issues WHERE id = NEW.dependent_issue_id)
BEGIN
  SELECT RAISE(ABORT, 'dependency organization mismatch');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER issues_workflow_scope_insert BEFORE INSERT ON issues
WHEN NEW.project_id IS NOT NULL AND NEW.workflow_state_id IS NOT NULL
BEGIN
  SELECT CASE WHEN NOT EXISTS (
    SELECT 1 FROM workflow_states WHERE id = NEW.workflow_state_id
    AND (project_id = NEW.project_id OR repository_id = NEW.repository_id)
  ) THEN RAISE(ABORT, 'workflow scope mismatch') END;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER issues_workflow_scope_update BEFORE UPDATE OF workflow_state_id ON issues
WHEN NEW.project_id IS NOT NULL AND NEW.workflow_state_id IS NOT NULL
BEGIN
  SELECT CASE WHEN NOT EXISTS (
    SELECT 1 FROM workflow_states WHERE id = NEW.workflow_state_id
    AND (project_id = NEW.project_id OR repository_id = NEW.repository_id)
  ) THEN RAISE(ABORT, 'workflow scope mismatch') END;
END;
-- +goose StatementEnd

CREATE TABLE native_comments (
  id TEXT PRIMARY KEY,
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  work_item_id TEXT NOT NULL,
  revision INTEGER NOT NULL CHECK (revision > 0),
  sequence INTEGER NOT NULL CHECK (sequence > 0),
  body TEXT NOT NULL,
  actor_json TEXT NOT NULL CHECK (json_valid(actor_json)),
  edited_by_json TEXT CHECK (edited_by_json IS NULL OR json_valid(edited_by_json)),
  provenance_json TEXT CHECK (provenance_json IS NULL OR json_valid(provenance_json)),
  source_key TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE (organization_id, work_item_id, source_key),
  UNIQUE (organization_id, work_item_id, sequence),
  FOREIGN KEY (organization_id, project_id, work_item_id) REFERENCES issues(organization_id, project_id, native_id)
);
CREATE INDEX native_comments_page_idx ON native_comments(organization_id, project_id, work_item_id, sequence);

CREATE TABLE collaboration_versions (
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  work_item_id TEXT NOT NULL,
  record_id TEXT NOT NULL,
  revision INTEGER NOT NULL CHECK (revision > 0),
  record_json TEXT NOT NULL CHECK (json_valid(record_json)),
  PRIMARY KEY (organization_id, record_id, revision),
  FOREIGN KEY (organization_id, project_id, work_item_id) REFERENCES issues(organization_id, project_id, native_id)
);

CREATE TABLE collaboration_events (
  id TEXT PRIMARY KEY,
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  work_item_id TEXT NOT NULL,
  sequence INTEGER NOT NULL CHECK (sequence > 0),
  type TEXT NOT NULL CHECK (type IN ('issue.created', 'issue.edited', 'comment.created', 'comment.edited', 'dependency.changed', 'workflow.transitioned', 'run.started', 'run.finished', 'run.checkpointed')),
  schema_version INTEGER NOT NULL CHECK (schema_version = 1),
  actor_json TEXT NOT NULL CHECK (json_valid(actor_json)),
  data_json TEXT NOT NULL CHECK (json_valid(data_json)),
  recorded_at TEXT NOT NULL,
  UNIQUE (organization_id, work_item_id, sequence),
  FOREIGN KEY (organization_id, project_id, work_item_id) REFERENCES issues(organization_id, project_id, native_id)
);

CREATE TABLE native_commands (
  organization_id TEXT NOT NULL REFERENCES organizations(id),
  actor_id TEXT NOT NULL REFERENCES api_tokens(id),
  operation TEXT NOT NULL,
  command_key TEXT NOT NULL,
  request_hash TEXT NOT NULL,
  response_json TEXT NOT NULL CHECK (json_valid(response_json)),
  created_at TEXT NOT NULL,
  PRIMARY KEY (organization_id, actor_id, operation, command_key)
);
CREATE TABLE token_grants (
  token_id TEXT NOT NULL REFERENCES api_tokens(id),
  organization_id TEXT NOT NULL REFERENCES organizations(id),
  project_id TEXT NOT NULL,
  PRIMARY KEY (token_id, organization_id, project_id),
  FOREIGN KEY (organization_id, project_id) REFERENCES projects(organization_id, id)
);
ALTER TABLE api_tokens ADD COLUMN native_only INTEGER NOT NULL DEFAULT 0 CHECK (native_only IN (0, 1));
ALTER TABLE machines ADD COLUMN organization_id TEXT REFERENCES organizations(id);
ALTER TABLE machines ADD COLUMN token_id TEXT REFERENCES api_tokens(id);

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

-- +goose StatementBegin
CREATE TRIGGER collaboration_versions_no_update BEFORE UPDATE ON collaboration_versions
BEGIN
  SELECT RAISE(ABORT, 'collaboration history is append-only');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER collaboration_versions_no_delete BEFORE DELETE ON collaboration_versions
BEGIN
  SELECT RAISE(ABORT, 'collaboration history is append-only');
END;
-- +goose StatementEnd

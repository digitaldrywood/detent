-- +goose Up
CREATE TABLE team_threads (
  id TEXT PRIMARY KEY,
  organization_id TEXT NOT NULL REFERENCES organizations(id),
  scope TEXT NOT NULL CHECK (scope IN ('general', 'project', 'issue')),
  project_id TEXT,
  work_item_id TEXT,
  title TEXT NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('open', 'resolved')),
  creator_principal TEXT NOT NULL,
  creator_name TEXT NOT NULL,
  creator_client TEXT NOT NULL,
  request_key TEXT NOT NULL,
  input_hash TEXT NOT NULL,
  post_count INTEGER NOT NULL DEFAULT 0,
  last_preview TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE (organization_id, creator_principal, request_key),
  FOREIGN KEY (organization_id, project_id) REFERENCES projects(organization_id, id),
  FOREIGN KEY (organization_id, project_id, work_item_id) REFERENCES issues(organization_id, project_id, native_id),
  CHECK ((scope = 'general' AND project_id IS NULL AND work_item_id IS NULL)
      OR (scope = 'project' AND project_id IS NOT NULL AND work_item_id IS NULL)
      OR (scope = 'issue' AND project_id IS NOT NULL AND work_item_id IS NOT NULL))
);
CREATE INDEX team_threads_org_page ON team_threads(organization_id, updated_at DESC, id DESC);
CREATE INDEX team_threads_project_page ON team_threads(organization_id, project_id, updated_at DESC, id DESC);
CREATE INDEX team_threads_issue_page ON team_threads(organization_id, work_item_id, updated_at DESC, id DESC);
CREATE INDEX team_threads_status_page ON team_threads(organization_id, status, updated_at DESC, id DESC);
CREATE INDEX team_threads_scope_page ON team_threads(organization_id, scope, status, updated_at DESC, id DESC);
CREATE INDEX team_threads_project_status ON team_threads(organization_id, project_id, status, updated_at DESC, id DESC);
CREATE INDEX team_threads_issue_status ON team_threads(organization_id, work_item_id, status, updated_at DESC, id DESC);

CREATE VIRTUAL TABLE team_thread_fts USING fts5(title, content='team_threads', content_rowid='rowid', tokenize='porter unicode61');
-- +goose StatementBegin
CREATE TRIGGER team_thread_search_insert AFTER INSERT ON team_threads BEGIN
  INSERT INTO team_thread_fts(rowid,title) VALUES(new.rowid,new.title);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER team_thread_search_delete AFTER DELETE ON team_threads BEGIN
  INSERT INTO team_thread_fts(team_thread_fts,rowid,title) VALUES('delete',old.rowid,old.title);
END;
-- +goose StatementEnd

CREATE TABLE team_thread_posts (
  thread_id TEXT NOT NULL REFERENCES team_threads(id) ON DELETE CASCADE,
  seq INTEGER NOT NULL,
  author_principal TEXT NOT NULL,
  author_name TEXT NOT NULL,
  acting_for TEXT NOT NULL,
  client_name TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('note', 'finding', 'question', 'decision')),
  body TEXT NOT NULL,
  request_key TEXT NOT NULL,
  input_hash TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (thread_id, seq),
  UNIQUE (thread_id, author_principal, request_key)
);

CREATE TABLE team_thread_reads (
  thread_id TEXT NOT NULL REFERENCES team_threads(id) ON DELETE CASCADE,
  principal_id TEXT NOT NULL,
  last_seq INTEGER NOT NULL,
  PRIMARY KEY (thread_id, principal_id)
);

CREATE TABLE team_thread_participants (
  thread_id TEXT NOT NULL REFERENCES team_threads(id) ON DELETE CASCADE,
  principal_id TEXT NOT NULL,
  client_name TEXT NOT NULL,
  display_name TEXT NOT NULL,
  PRIMARY KEY (thread_id, principal_id, client_name)
);

CREATE TABLE team_thread_references (
  thread_id TEXT NOT NULL REFERENCES team_threads(id) ON DELETE CASCADE,
  post_seq INTEGER NOT NULL DEFAULT 0,
  kind TEXT NOT NULL CHECK (kind IN ('issue', 'change', 'attachment')),
  target_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  PRIMARY KEY (thread_id, post_seq, kind, target_id)
);

-- +goose Down
DROP TABLE team_thread_references;
DROP TABLE team_thread_participants;
DROP TABLE team_thread_reads;
DROP TABLE team_thread_posts;
DROP TRIGGER team_thread_search_delete;
DROP TRIGGER team_thread_search_insert;
DROP TABLE team_thread_fts;
DROP TABLE team_threads;

-- +goose Up
-- Turn preferences, message references and Settled (decisions section 14).
--
-- The conversations table is rebuilt rather than altered: the status check
-- constraint has to change from ('active', 'archived') to ('active',
-- 'settled'), and SQLite cannot alter a constraint in place. The rebuild also
-- renames archived_at to settled_at and adds preferences_json, so the table
-- is copied once instead of three times.
CREATE TABLE conversations_settled (
  id TEXT PRIMARY KEY,
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  owner_principal_id TEXT NOT NULL REFERENCES api_tokens(id),
  owner_subject TEXT NOT NULL DEFAULT '',
  title TEXT NOT NULL DEFAULT '',
  visibility TEXT NOT NULL CHECK (visibility IN ('private', 'shared')),
  status TEXT NOT NULL CHECK (status IN ('active', 'settled')),
  work_item_id TEXT,
  linked_at TEXT,
  revision INTEGER NOT NULL DEFAULT 1 CHECK (revision > 0),
  provider_thread_id TEXT NOT NULL DEFAULT '',
  provider_thread_runner_id TEXT NOT NULL DEFAULT '',
  execution_json TEXT NOT NULL CHECK (json_valid(execution_json)),
  event_seq INTEGER NOT NULL DEFAULT 0 CHECK (event_seq >= 0),
  preferences_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(preferences_json)),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  last_message_at TEXT,
  settled_at TEXT,
  FOREIGN KEY (organization_id, project_id) REFERENCES projects(organization_id, id)
);

INSERT INTO conversations_settled (
  id, organization_id, project_id, owner_principal_id, owner_subject, title, visibility, status,
  work_item_id, linked_at, revision, provider_thread_id, provider_thread_runner_id, execution_json, event_seq,
  preferences_json, created_at, updated_at, last_message_at, settled_at
)
SELECT id, organization_id, project_id, owner_principal_id, owner_subject, title, visibility,
  CASE WHEN status = 'archived' THEN 'settled' ELSE status END,
  work_item_id, linked_at, revision, provider_thread_id, provider_thread_runner_id, execution_json, event_seq,
  '{}', created_at, updated_at, last_message_at, archived_at
FROM conversations;

DROP TABLE conversations;
ALTER TABLE conversations_settled RENAME TO conversations;

CREATE UNIQUE INDEX conversations_work_item_idx ON conversations(organization_id, project_id, work_item_id) WHERE work_item_id IS NOT NULL;
CREATE INDEX conversations_owner_activity_idx ON conversations(organization_id, project_id, owner_principal_id, updated_at);
CREATE INDEX conversations_activity_idx ON conversations(organization_id, project_id, COALESCE(last_message_at, updated_at), id);
-- The settle sweep asks for active conversations whose last activity is older
-- than the project's window, across every project.
CREATE INDEX conversations_settle_idx ON conversations(status, COALESCE(last_message_at, updated_at));

-- message_references records what a message pointed at. Only targets the hub
-- could resolve are stored, so a row is always a real issue or conversation.
-- The primary key makes re-extraction of the same message idempotent.
CREATE TABLE message_references (
  message_id TEXT NOT NULL REFERENCES conversation_messages(id),
  conversation_id TEXT NOT NULL REFERENCES conversations(id),
  target_kind TEXT NOT NULL CHECK (target_kind IN ('issue', 'conversation')),
  target_id TEXT NOT NULL,
  label TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  PRIMARY KEY (message_id, target_kind, target_id)
);
-- The target's activity lists what referenced it, newest first.
CREATE INDEX message_references_target_idx ON message_references(target_kind, target_id, created_at, message_id);

-- +goose Down
DROP TABLE message_references;

CREATE TABLE conversations_archived (
  id TEXT PRIMARY KEY,
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  owner_principal_id TEXT NOT NULL REFERENCES api_tokens(id),
  owner_subject TEXT NOT NULL DEFAULT '',
  title TEXT NOT NULL DEFAULT '',
  visibility TEXT NOT NULL CHECK (visibility IN ('private', 'shared')),
  status TEXT NOT NULL CHECK (status IN ('active', 'archived')),
  work_item_id TEXT,
  linked_at TEXT,
  revision INTEGER NOT NULL DEFAULT 1 CHECK (revision > 0),
  provider_thread_id TEXT NOT NULL DEFAULT '',
  execution_json TEXT NOT NULL CHECK (json_valid(execution_json)),
  event_seq INTEGER NOT NULL DEFAULT 0 CHECK (event_seq >= 0),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  last_message_at TEXT,
  archived_at TEXT,
  provider_thread_runner_id TEXT NOT NULL DEFAULT '',
  FOREIGN KEY (organization_id, project_id) REFERENCES projects(organization_id, id)
);

INSERT INTO conversations_archived (
  id, organization_id, project_id, owner_principal_id, owner_subject, title, visibility, status,
  work_item_id, linked_at, revision, provider_thread_id, execution_json, event_seq,
  created_at, updated_at, last_message_at, archived_at, provider_thread_runner_id
)
SELECT id, organization_id, project_id, owner_principal_id, owner_subject, title, visibility,
  CASE WHEN status = 'settled' THEN 'archived' ELSE status END,
  work_item_id, linked_at, revision, provider_thread_id, execution_json, event_seq,
  created_at, updated_at, last_message_at, settled_at, provider_thread_runner_id
FROM conversations;

DROP TABLE conversations;
ALTER TABLE conversations_archived RENAME TO conversations;

CREATE UNIQUE INDEX conversations_work_item_idx ON conversations(organization_id, project_id, work_item_id) WHERE work_item_id IS NOT NULL;
CREATE INDEX conversations_owner_activity_idx ON conversations(organization_id, project_id, owner_principal_id, updated_at);
CREATE INDEX conversations_activity_idx ON conversations(organization_id, project_id, COALESCE(last_message_at, updated_at), id);

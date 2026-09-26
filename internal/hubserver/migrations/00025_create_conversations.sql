-- +goose Up
CREATE TABLE conversations (
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
  provider_thread_runner_id TEXT NOT NULL DEFAULT '',
  execution_json TEXT NOT NULL CHECK (json_valid(execution_json)),
  event_seq INTEGER NOT NULL DEFAULT 0 CHECK (event_seq >= 0),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  last_message_at TEXT,
  archived_at TEXT,
  FOREIGN KEY (organization_id, project_id) REFERENCES projects(organization_id, id)
);
CREATE UNIQUE INDEX conversations_work_item_idx ON conversations(organization_id, project_id, work_item_id) WHERE work_item_id IS NOT NULL;
CREATE INDEX conversations_owner_activity_idx ON conversations(organization_id, project_id, owner_principal_id, updated_at);
CREATE INDEX conversations_activity_idx ON conversations(organization_id, project_id, COALESCE(last_message_at, updated_at), id);

CREATE TABLE conversation_messages (
  id TEXT PRIMARY KEY,
  conversation_id TEXT NOT NULL REFERENCES conversations(id),
  seq INTEGER NOT NULL CHECK (seq > 0),
  role TEXT NOT NULL CHECK (role IN ('user', 'assistant', 'system')),
  kind TEXT NOT NULL CHECK (kind IN ('text', 'answer', 'interrupt', 'continue', 'tool', 'status')),
  text TEXT NOT NULL DEFAULT '',
  data_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(data_json)),
  delivery TEXT NOT NULL CHECK (delivery IN ('saved', 'queued', 'sending', 'sent', 'delivered', 'responding', 'completed', 'interrupted', 'rejected', 'failed', 'unknown')),
  attempt_id TEXT NOT NULL DEFAULT '',
  thread_id TEXT NOT NULL DEFAULT '',
  turn_id TEXT NOT NULL DEFAULT '',
  provider_item_id TEXT NOT NULL DEFAULT '',
  actor_json TEXT NOT NULL CHECK (json_valid(actor_json)),
  command_key TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE (conversation_id, seq)
);
CREATE INDEX conversation_messages_delivery_idx ON conversation_messages(delivery);

CREATE TABLE conversation_questions (
  id TEXT PRIMARY KEY,
  conversation_id TEXT NOT NULL REFERENCES conversations(id),
  message_id TEXT NOT NULL DEFAULT '',
  request_id TEXT NOT NULL DEFAULT '',
  attempt_id TEXT NOT NULL DEFAULT '',
  thread_id TEXT NOT NULL DEFAULT '',
  turn_id TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK (status IN ('pending', 'sending', 'sent', 'answered', 'expired', 'unknown')),
  prompts_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(prompts_json)),
  answers_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(answers_json)),
  answered_by TEXT NOT NULL DEFAULT '',
  expires_at TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX conversation_questions_pending_idx ON conversation_questions(conversation_id, status, created_at);

CREATE TABLE conversation_commands (
  conversation_id TEXT NOT NULL REFERENCES conversations(id),
  key TEXT NOT NULL CHECK (length(key) BETWEEN 1 AND 128),
  kind TEXT NOT NULL CHECK (kind IN ('message', 'answer', 'interrupt', 'continue', 'cancel', 'retry')),
  request_hash TEXT NOT NULL,
  receipt_json TEXT NOT NULL CHECK (json_valid(receipt_json)),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (conversation_id, key)
);

CREATE TABLE conversation_events (
  conversation_id TEXT NOT NULL REFERENCES conversations(id),
  seq INTEGER NOT NULL CHECK (seq > 0),
  type TEXT NOT NULL CHECK (type IN ('conversation.updated', 'message.accepted', 'message.delta', 'message.updated', 'question.opened', 'question.updated', 'execution.updated', 'command.receipt', 'heartbeat', 'closed')),
  body_json TEXT NOT NULL CHECK (json_valid(body_json)),
  created_at TEXT NOT NULL,
  PRIMARY KEY (conversation_id, seq)
);

CREATE TABLE conversation_starts (
  attempt_id TEXT PRIMARY KEY,
  conversation_id TEXT NOT NULL REFERENCES conversations(id),
  created_at TEXT NOT NULL
);

CREATE TABLE conversation_audience_events (
  id TEXT PRIMARY KEY,
  conversation_id TEXT NOT NULL REFERENCES conversations(id),
  actor_principal_id TEXT NOT NULL,
  from_visibility TEXT NOT NULL CHECK (from_visibility IN ('private', 'shared')),
  to_visibility TEXT NOT NULL CHECK (to_visibility IN ('private', 'shared')),
  created_at TEXT NOT NULL
);
CREATE INDEX conversation_audience_events_idx ON conversation_audience_events(conversation_id, created_at);

CREATE TABLE conversation_turn_batches (
  attempt_id TEXT NOT NULL,
  batch_key TEXT NOT NULL CHECK (length(batch_key) BETWEEN 1 AND 128),
  event_seq INTEGER NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (attempt_id, batch_key)
);

-- +goose Down
DROP TABLE conversation_turn_batches;
DROP TABLE conversation_audience_events;
DROP TABLE conversation_starts;
DROP TABLE conversation_events;
DROP TABLE conversation_commands;
DROP TABLE conversation_questions;
DROP TABLE conversation_messages;
DROP TABLE conversations;

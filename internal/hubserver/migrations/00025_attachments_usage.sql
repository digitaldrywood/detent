-- +goose Up
-- Attachments and per-attempt usage (decisions section 17, items 1 and 5).

-- conversation_attachments is one uploaded file. message_id is NULL while the
-- upload is unsent: only then may its owner delete it, and only then does it
-- expire. Binding it to a message sets message_id and clears expires_at, so a
-- sent attachment lives exactly as long as the message that carries it. The
-- check keeps those two states from overlapping.
--
-- artifact_ref names where the bytes are. The hub has no blob store of its
-- own -- the artifact service is a separate process with its own object
-- storage, bound per project and reachable only by a runner -- so the bytes
-- live in conversation_attachment_blobs and artifact_ref is the key into it.
-- The column is the seam: an external store only has to change what the ref
-- resolves to.
CREATE TABLE conversation_attachments (
  id TEXT PRIMARY KEY,
  conversation_id TEXT NOT NULL REFERENCES conversations(id),
  message_id TEXT REFERENCES conversation_messages(id),
  principal_id TEXT NOT NULL REFERENCES api_tokens(id),
  name TEXT NOT NULL,
  mime TEXT NOT NULL,
  size INTEGER NOT NULL CHECK (size > 0),
  artifact_ref TEXT NOT NULL UNIQUE,
  created_at TEXT NOT NULL,
  expires_at TEXT,
  deleted_at TEXT,
  CHECK (message_id IS NULL OR expires_at IS NULL)
);
-- The message projection reads every attachment of a page of messages.
CREATE INDEX conversation_attachments_message_idx ON conversation_attachments(message_id, created_at, id) WHERE message_id IS NOT NULL;
-- The command path counts and sizes an actor's unsent uploads; the sweep asks
-- for expired unsent rows across every conversation.
CREATE INDEX conversation_attachments_pending_idx ON conversation_attachments(conversation_id, principal_id) WHERE message_id IS NULL AND deleted_at IS NULL;
CREATE INDEX conversation_attachments_sweep_idx ON conversation_attachments(expires_at) WHERE message_id IS NULL AND deleted_at IS NULL;

-- The bytes of an attachment. The row is deleted when the sweep abandons an
-- upload or an owner deletes it; the attachment row survives as a tombstone
-- so a late reference reads "gone", not "never existed".
CREATE TABLE conversation_attachment_blobs (
  artifact_ref TEXT PRIMARY KEY,
  content BLOB NOT NULL
);

-- attempt_usage is what one attempt spent, split by day, provider and model.
-- The runner reports it on run.checkpointed and run.finished as the running
-- total for the attempt, so each event overwrites the row rather than adding
-- to it: a redelivered event cannot double-count. attempt_id carries no
-- foreign key because usage may arrive on a checkpoint the attempt recorder
-- did not order.
CREATE TABLE attempt_usage (
  attempt_id TEXT NOT NULL,
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  day TEXT NOT NULL,
  provider TEXT NOT NULL,
  model TEXT NOT NULL,
  input INTEGER NOT NULL DEFAULT 0 CHECK (input >= 0),
  cached_input INTEGER NOT NULL DEFAULT 0 CHECK (cached_input >= 0),
  output INTEGER NOT NULL DEFAULT 0 CHECK (output >= 0),
  cost_estimate REAL NOT NULL DEFAULT 0 CHECK (cost_estimate >= 0),
  currency TEXT NOT NULL DEFAULT 'USD',
  updated_at TEXT NOT NULL,
  PRIMARY KEY (attempt_id, day, provider, model)
);
-- The usage report sums one organization over a day range, optionally for one
-- project.
CREATE INDEX attempt_usage_window_idx ON attempt_usage(organization_id, day, project_id);

-- +goose Down
DROP TABLE attempt_usage;
DROP TABLE conversation_attachment_blobs;
DROP TABLE conversation_attachments;

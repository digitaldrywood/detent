-- +goose Up
-- coordinator_items records the native issue that dispatches a coordinator
-- turn for an unlinked conversation to a customer runner. The conversation
-- is not linked: its work_item_id stays empty and it stays private.
CREATE TABLE coordinator_items (
  work_item_id TEXT PRIMARY KEY,
  conversation_id TEXT NOT NULL REFERENCES conversations(id),
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  created_at TEXT NOT NULL,
  closed_at TEXT
);
CREATE INDEX coordinator_items_open_idx ON coordinator_items(conversation_id, closed_at);
-- The claim loop and the hosted issue list ask which items are open in a
-- project, without naming a conversation.
CREATE INDEX coordinator_items_project_open_idx ON coordinator_items(organization_id, project_id) WHERE closed_at IS NULL;

-- conversation_turn_batches makes a turn-event batch idempotent. A worker
-- that retries a batch after a timeout would otherwise append its deltas and
-- items twice; the stored event_seq answers the retry with the outcome of the
-- first attempt.
CREATE TABLE conversation_turn_batches (
  attempt_id TEXT NOT NULL,
  batch_key TEXT NOT NULL CHECK (length(batch_key) BETWEEN 1 AND 128),
  event_seq INTEGER NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (attempt_id, batch_key)
);

-- provider_thread_runner_id records where the conversation's provider thread
-- was produced. It is empty for the transitional hub-side coordinator, so a
-- runner never resumes a thread that lives on another provider login.
ALTER TABLE conversations ADD COLUMN provider_thread_runner_id TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE conversations DROP COLUMN provider_thread_runner_id;
DROP TABLE conversation_turn_batches;
DROP TABLE coordinator_items;

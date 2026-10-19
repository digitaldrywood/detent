-- +goose Up
CREATE TABLE operator_chat_sessions (
  connection_id TEXT PRIMARY KEY,
  organization_id TEXT NOT NULL REFERENCES organizations(id),
  identity_json TEXT NOT NULL CHECK (json_valid(identity_json)),
  client TEXT NOT NULL,
  require_confirmation INTEGER NOT NULL CHECK (require_confirmation IN (0, 1)),
  mode TEXT NOT NULL CHECK (mode IN ('confirmation', 'yolo')),
  actions_json TEXT NOT NULL CHECK (json_valid(actions_json)),
  last_used_at TEXT NOT NULL
);
CREATE INDEX operator_chat_sessions_last_used_idx ON operator_chat_sessions(last_used_at, connection_id);

-- +goose Down
DROP TABLE operator_chat_sessions;

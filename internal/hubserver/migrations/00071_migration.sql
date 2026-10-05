-- +goose Up
CREATE TABLE operator_connections (
  connection_id TEXT PRIMARY KEY,
  organization_id TEXT NOT NULL REFERENCES organizations(id),
  identity_json TEXT NOT NULL CHECK (json_valid(identity_json)),
  mode TEXT NOT NULL CHECK (mode IN ('confirmation', 'yolo'))
);
INSERT INTO operator_connections(connection_id, organization_id, identity_json, mode)
SELECT connection_id, organization_id, identity_json, mode FROM operator_chat_sessions;
ALTER TABLE operator_chat_sessions DROP COLUMN mode;

-- +goose Down
ALTER TABLE operator_chat_sessions ADD COLUMN mode TEXT NOT NULL DEFAULT 'confirmation' CHECK (mode IN ('confirmation', 'yolo'));
UPDATE operator_chat_sessions SET mode = COALESCE((SELECT mode FROM operator_connections WHERE operator_connections.connection_id = operator_chat_sessions.connection_id), 'confirmation');
DROP TABLE operator_connections;

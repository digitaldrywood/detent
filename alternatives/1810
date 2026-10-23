-- +goose Up
ALTER TABLE operator_chat_sessions DROP COLUMN require_confirmation;
ALTER TABLE operator_chat_sessions DROP COLUMN mode;
ALTER TABLE operator_chat_sessions DROP COLUMN actions_json;

-- +goose Down
ALTER TABLE operator_chat_sessions ADD COLUMN require_confirmation INTEGER NOT NULL DEFAULT 0;
ALTER TABLE operator_chat_sessions ADD COLUMN mode TEXT NOT NULL DEFAULT 'confirmation';
ALTER TABLE operator_chat_sessions ADD COLUMN actions_json TEXT NOT NULL DEFAULT '[]';

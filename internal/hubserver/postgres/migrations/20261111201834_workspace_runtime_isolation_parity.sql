-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE workspace_sessions ADD CONSTRAINT workspace_sessions_isolation_parity CHECK (isolation IN ('','user','container','sandbox')) NOT VALID;
ALTER TABLE workspace_terminal_recordings ADD CONSTRAINT workspace_terminal_recordings_isolation_parity CHECK (isolation IN ('user','container','sandbox')) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE workspace_sessions DROP CONSTRAINT workspace_sessions_isolation_parity;
ALTER TABLE workspace_terminal_recordings DROP CONSTRAINT workspace_terminal_recordings_isolation_parity;

-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE workspace_sessions DROP CONSTRAINT workspace_sessions_isolation_check;
ALTER TABLE workspace_sessions RENAME CONSTRAINT workspace_sessions_isolation_parity TO workspace_sessions_isolation_check;
ALTER TABLE workspace_terminal_recordings DROP CONSTRAINT workspace_terminal_recordings_isolation_check;
ALTER TABLE workspace_terminal_recordings RENAME CONSTRAINT workspace_terminal_recordings_isolation_parity TO workspace_terminal_recordings_isolation_check;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE workspace_sessions RENAME CONSTRAINT workspace_sessions_isolation_check TO workspace_sessions_isolation_parity;
ALTER TABLE workspace_sessions ADD CONSTRAINT workspace_sessions_isolation_check CHECK (isolation IN ('','user','container'));
ALTER TABLE workspace_terminal_recordings RENAME CONSTRAINT workspace_terminal_recordings_isolation_check TO workspace_terminal_recordings_isolation_parity;
ALTER TABLE workspace_terminal_recordings ADD CONSTRAINT workspace_terminal_recordings_isolation_check CHECK (isolation IN ('user','container'));

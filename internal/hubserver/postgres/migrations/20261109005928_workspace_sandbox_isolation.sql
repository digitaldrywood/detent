-- +goose Up
ALTER TABLE workspace_sessions DROP CONSTRAINT workspace_sessions_isolation_check;
ALTER TABLE workspace_sessions ADD CONSTRAINT workspace_sessions_isolation_check CHECK (isolation IN ('','sandbox','user','container'));
ALTER TABLE workspace_terminal_recordings DROP CONSTRAINT workspace_terminal_recordings_isolation_check;
ALTER TABLE workspace_terminal_recordings ADD CONSTRAINT workspace_terminal_recordings_isolation_check CHECK (isolation IN ('sandbox','user','container'));

-- +goose Down
ALTER TABLE workspace_terminal_recordings DROP CONSTRAINT workspace_terminal_recordings_isolation_check;
ALTER TABLE workspace_terminal_recordings ADD CONSTRAINT workspace_terminal_recordings_isolation_check CHECK (isolation IN ('user','container'));
ALTER TABLE workspace_sessions DROP CONSTRAINT workspace_sessions_isolation_check;
ALTER TABLE workspace_sessions ADD CONSTRAINT workspace_sessions_isolation_check CHECK (isolation IN ('','user','container'));

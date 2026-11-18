-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE workspace_sessions VALIDATE CONSTRAINT workspace_sessions_isolation_parity;
ALTER TABLE workspace_terminal_recordings VALIDATE CONSTRAINT workspace_terminal_recordings_isolation_parity;

-- +goose Down
SELECT 1;

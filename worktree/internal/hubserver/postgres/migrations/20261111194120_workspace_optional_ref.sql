-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE workspace_sessions DROP CONSTRAINT workspace_sessions_ref_check;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE workspace_sessions ADD CONSTRAINT workspace_sessions_ref_check CHECK (btrim(ref)<>'') NOT VALID;

-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
CREATE INDEX CONCURRENTLY IF NOT EXISTS native_commands_replay_key
 ON native_commands (organization_id, actor_id, command_key, operation);
CREATE INDEX CONCURRENTLY IF NOT EXISTS native_command_states_replay_key
 ON native_command_states (organization_id, actor_id, command_key, operation);
-- +goose StatementBegin
DO $$ BEGIN
 IF EXISTS (
  SELECT 1 FROM pg_index
  WHERE indexrelid IN ('native_commands_replay_key'::regclass, 'native_command_states_replay_key'::regclass)
  AND (NOT indisvalid OR NOT indisready)
 ) THEN RAISE EXCEPTION 'Native receipt replay indexes require repair'; END IF;
END $$;
-- +goose StatementEnd
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS native_command_states_replay_key;
DROP INDEX CONCURRENTLY IF EXISTS native_commands_replay_key;
RESET lock_timeout;

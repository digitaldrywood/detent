-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
CREATE INDEX CONCURRENTLY queue_entries_runner_sync_state
    ON queue_entries (organization_id, project_id, state) WHERE scope = project_id;
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY queue_entries_runner_sync_state;
RESET lock_timeout;

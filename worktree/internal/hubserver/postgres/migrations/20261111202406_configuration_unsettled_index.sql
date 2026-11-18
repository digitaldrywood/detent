-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS native_attempts_configuration_unsettled_idx;
CREATE INDEX CONCURRENTLY native_attempts_configuration_unsettled_idx ON native_attempts(organization_id,lease_id,issue_id) WHERE status='interrupted' AND checkpoint_object_key IS NOT NULL AND cleanup_acknowledged_at IS NULL;
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY native_attempts_configuration_unsettled_idx;
RESET lock_timeout;

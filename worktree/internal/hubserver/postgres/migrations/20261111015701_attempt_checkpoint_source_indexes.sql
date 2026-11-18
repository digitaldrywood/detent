-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS native_attempts_source_checkpoint_idx;
CREATE INDEX CONCURRENTLY native_attempts_source_checkpoint_idx
ON native_attempts (organization_id,issue_id,fencing_token DESC)
WHERE checkpoint_object_key IS NOT NULL AND checkpoint_preserves_source IS DISTINCT FROM FALSE;
DROP INDEX CONCURRENTLY IF EXISTS native_attempts_checkpoint_idx;
CREATE INDEX CONCURRENTLY native_attempts_checkpoint_idx
ON native_attempts (organization_id,issue_id,fencing_token DESC)
WHERE checkpoint_object_key IS NOT NULL;
RESET lock_timeout;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS native_attempts_checkpoint_idx;
DROP INDEX CONCURRENTLY IF EXISTS native_attempts_source_checkpoint_idx;

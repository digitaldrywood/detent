-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS native_attempts_pending_cleanup_idx;
CREATE INDEX CONCURRENTLY native_attempts_pending_cleanup_idx ON native_attempts(organization_id,project_id,id)
WHERE cleanup_acknowledged_at IS NULL AND status IN ('running','interrupted','cancelled') AND (worker_cleanup_required IS DISTINCT FROM FALSE OR runtime_summary_sequence IS DISTINCT FROM sequence);
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS native_attempts_pending_cleanup_idx;
RESET lock_timeout;

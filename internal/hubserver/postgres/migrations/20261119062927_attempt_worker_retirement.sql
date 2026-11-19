-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
ALTER TABLE native_attempts ADD COLUMN IF NOT EXISTS worker_retired_at timestamptz,
    DROP CONSTRAINT IF EXISTS native_attempts_worker_retirement_check,
    ADD CONSTRAINT native_attempts_worker_retirement_check CHECK (worker_retired_at IS NULL OR worker_pid IS NOT NULL) NOT VALID;
ALTER TABLE native_attempts VALIDATE CONSTRAINT native_attempts_worker_retirement_check;
UPDATE native_attempts SET cleanup_acknowledged_at = NULL WHERE worker_pid IS NOT NULL AND cleanup_acknowledged_at IS NOT NULL;
DROP INDEX CONCURRENTLY IF EXISTS native_attempts_worker_inventory_idx;
CREATE INDEX CONCURRENTLY native_attempts_worker_inventory_idx ON native_attempts(organization_id,project_id,worker_runner_id,id)
WHERE worker_pid IS NOT NULL AND worker_retired_at IS NULL;
DROP INDEX CONCURRENTLY IF EXISTS native_attempts_pending_cleanup_idx;
CREATE INDEX CONCURRENTLY native_attempts_pending_cleanup_idx ON native_attempts(organization_id,project_id,id)
WHERE worker_pid IS NULL AND cleanup_acknowledged_at IS NULL AND status IN ('running','interrupted','cancelled') AND (worker_cleanup_required IS DISTINCT FROM FALSE OR runtime_summary_sequence IS DISTINCT FROM sequence);
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS native_attempts_pending_cleanup_idx;
CREATE INDEX CONCURRENTLY native_attempts_pending_cleanup_idx ON native_attempts(organization_id,project_id,id)
WHERE cleanup_acknowledged_at IS NULL AND status IN ('running','interrupted','cancelled') AND (worker_cleanup_required IS DISTINCT FROM FALSE OR runtime_summary_sequence IS DISTINCT FROM sequence);
DROP INDEX CONCURRENTLY IF EXISTS native_attempts_worker_inventory_idx;
CREATE INDEX CONCURRENTLY native_attempts_worker_inventory_idx ON native_attempts(organization_id,project_id,worker_runner_id,id)
WHERE worker_pid IS NOT NULL AND cleanup_acknowledged_at IS NULL;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid='native_attempts'::regclass AND attname='worker_retired_at' AND NOT attisdropped) THEN
        UPDATE native_attempts SET cleanup_acknowledged_at = NULL WHERE worker_pid IS NOT NULL AND worker_retired_at IS NULL AND cleanup_acknowledged_at IS NOT NULL;
        ALTER TABLE native_attempts DROP COLUMN worker_retired_at;
    END IF;
END $$;
-- +goose StatementEnd
RESET lock_timeout;

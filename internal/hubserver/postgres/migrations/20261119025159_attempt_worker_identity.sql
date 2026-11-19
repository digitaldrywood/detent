-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
ALTER TABLE native_attempts
    ADD COLUMN IF NOT EXISTS worker_pid integer,
    ADD COLUMN IF NOT EXISTS worker_pgid integer,
    ADD COLUMN IF NOT EXISTS worker_started_at timestamptz,
    ADD COLUMN IF NOT EXISTS worker_start_key text,
    ADD COLUMN IF NOT EXISTS worker_runner_id text,
    DROP CONSTRAINT IF EXISTS native_attempts_worker_identity_check,
    ADD CONSTRAINT native_attempts_worker_identity_check CHECK (
        num_nonnulls(worker_pid, worker_pgid, worker_started_at, worker_start_key, worker_runner_id) = 0
        OR (num_nonnulls(worker_pid, worker_pgid, worker_started_at, worker_start_key, worker_runner_id) = 5
            AND worker_pid > 0 AND worker_pgid > 0 AND isfinite(worker_started_at)
            AND octet_length(worker_start_key) <= 128 AND btrim(worker_start_key) <> '' AND worker_runner_id <> '')
    ) NOT VALID;
ALTER TABLE native_attempts VALIDATE CONSTRAINT native_attempts_worker_identity_check;
DROP INDEX CONCURRENTLY IF EXISTS native_attempts_worker_inventory_idx;
CREATE INDEX CONCURRENTLY native_attempts_worker_inventory_idx ON native_attempts (organization_id, project_id, worker_runner_id, id)
WHERE worker_pid IS NOT NULL AND cleanup_acknowledged_at IS NULL;
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS native_attempts_worker_inventory_idx;
ALTER TABLE native_attempts
    DROP CONSTRAINT IF EXISTS native_attempts_worker_identity_check,
    DROP COLUMN IF EXISTS worker_pid,
    DROP COLUMN IF EXISTS worker_pgid,
    DROP COLUMN IF EXISTS worker_started_at,
    DROP COLUMN IF EXISTS worker_start_key,
    DROP COLUMN IF EXISTS worker_runner_id;
RESET lock_timeout;

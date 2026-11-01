-- +goose Up
ALTER TABLE native_attempts ADD COLUMN worker_cleanup_required INTEGER;
ALTER TABLE native_attempts ADD COLUMN cleanup_acknowledged_at TEXT;
CREATE INDEX native_attempts_pending_cleanup_idx ON native_attempts(organization_id,project_id,runner_id,id)
WHERE worker_cleanup_required=1 AND cleanup_acknowledged_at IS NULL AND status IN ('running','interrupted','cancelled');
CREATE INDEX native_attempts_cleanup_conversion_idx ON native_attempts(id) WHERE worker_cleanup_required IS NULL;

-- +goose Down
DROP INDEX native_attempts_cleanup_conversion_idx;
DROP INDEX native_attempts_pending_cleanup_idx;
ALTER TABLE native_attempts DROP COLUMN cleanup_acknowledged_at;
ALTER TABLE native_attempts DROP COLUMN worker_cleanup_required;

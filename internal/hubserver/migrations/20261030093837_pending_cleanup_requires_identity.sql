-- +goose Up
DROP INDEX native_attempts_pending_cleanup_idx;
CREATE INDEX native_attempts_pending_cleanup_idx ON native_attempts(organization_id,project_id,runner_id,id)
WHERE worker_cleanup_required=1 AND cleanup_acknowledged_at IS NULL AND status IN ('running','interrupted','cancelled') AND execution_role<>'';

-- +goose Down
DROP INDEX native_attempts_pending_cleanup_idx;
CREATE INDEX native_attempts_pending_cleanup_idx ON native_attempts(organization_id,project_id,runner_id,id)
WHERE worker_cleanup_required=1 AND cleanup_acknowledged_at IS NULL AND status IN ('running','interrupted','cancelled');

-- +goose Up
CREATE INDEX native_execution_gate_failures_idx ON native_execution_check_transitions
    (organization_id, project_id, head_sha, tree_sha, checked_at DESC, receipt_id)
    WHERE status IN ('failed', 'flaky') AND round_id LIKE 'evidence:%';

-- +goose Down
DROP INDEX native_execution_gate_failures_idx;

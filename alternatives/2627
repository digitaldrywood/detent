-- +goose Up
CREATE INDEX collaboration_events_diagnostic_window_idx
    ON collaboration_events (organization_id, project_id, recorded_at, id)
    WHERE type IN ('workflow.transitioned', 'scheduler.decision');
CREATE INDEX native_attempts_diagnostic_window_idx
    ON native_attempts (organization_id, started_at, issue_id);
CREATE INDEX leases_diagnostic_window_idx
    ON leases (organization_id, acquired_at, lease_id);

-- +goose Down
DROP INDEX leases_diagnostic_window_idx;
DROP INDEX native_attempts_diagnostic_window_idx;
DROP INDEX collaboration_events_diagnostic_window_idx;

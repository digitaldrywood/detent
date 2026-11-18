-- +goose Up
CREATE INDEX leases_report_runner_window_idx
    ON leases(organization_id,machine_id,expires_at,lease_id)
    INCLUDE(acquired_at,released_at);

-- +goose Down
DROP INDEX leases_report_runner_window_idx;

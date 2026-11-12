-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
ALTER TABLE runner_lease_assignments VALIDATE CONSTRAINT runner_lease_assignment_binding;
ALTER TABLE leases VALIDATE CONSTRAINT leases_runner_project_present;
RESET lock_timeout;

-- +goose Down
SELECT 1;

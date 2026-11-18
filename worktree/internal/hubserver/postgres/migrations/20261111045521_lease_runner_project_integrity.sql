-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE leases ADD CONSTRAINT leases_assignment_binding_unique UNIQUE USING INDEX leases_assignment_binding_unique;
ALTER TABLE runner_lease_assignments ADD CONSTRAINT runner_lease_assignment_binding
 FOREIGN KEY(organization_id,lease_id,runner_id) REFERENCES leases(organization_id,lease_id,runner_id) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE runner_lease_assignments DROP CONSTRAINT runner_lease_assignment_binding;
ALTER TABLE leases DROP CONSTRAINT leases_assignment_binding_unique;

-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE leases ADD COLUMN project_id text, ADD COLUMN runner_id text;
ALTER TABLE leases ADD CONSTRAINT leases_runner_project_present
 CHECK(released_at IS NOT NULL OR runner_id IS NULL OR project_id IS NOT NULL) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE leases DROP CONSTRAINT leases_runner_project_present;
ALTER TABLE leases DROP COLUMN runner_id, DROP COLUMN project_id;

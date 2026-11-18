-- +goose Up
SET LOCAL lock_timeout = '5s';

ALTER TABLE github_reconciliation
 ADD COLUMN last_successful_sweep_at timestamptz,
 ADD COLUMN last_failure_at timestamptz;

-- +goose Down
SET LOCAL lock_timeout = '5s';

ALTER TABLE github_reconciliation
 DROP COLUMN last_failure_at,
 DROP COLUMN last_successful_sweep_at;

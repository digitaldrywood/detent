-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS leases_monthly_latest;
CREATE INDEX CONCURRENTLY leases_monthly_latest ON leases(organization_id,issue_id,fencing_token DESC);
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY leases_monthly_latest;
RESET lock_timeout;

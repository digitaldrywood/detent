-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS leases_machine_unreleased_seek_idx;
CREATE INDEX CONCURRENTLY leases_machine_unreleased_seek_idx
ON leases (organization_id,machine_id,lease_id)
WHERE released_at IS NULL;
RESET lock_timeout;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS leases_machine_unreleased_seek_idx;

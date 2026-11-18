-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS hub_detail_reclaim_due;
CREATE INDEX CONCURRENTLY hub_detail_reclaim_due ON hub_detail_objects(organization_id,created_at,object_key) WHERE reclaim_pending;
DROP INDEX CONCURRENTLY IF EXISTS platform.organizations_ready_archive_reclaim;
CREATE INDEX CONCURRENTLY organizations_ready_archive_reclaim ON platform.organizations(archive_reclaim_at,id) WHERE state='ready';
DROP INDEX CONCURRENTLY IF EXISTS archive_reclaim_due;
CREATE INDEX CONCURRENTLY archive_reclaim_due ON archive_reclaim_candidates(organization_id,due_at,object_key);
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS archive_reclaim_due;
DROP INDEX CONCURRENTLY IF EXISTS platform.organizations_ready_archive_reclaim;
DROP INDEX CONCURRENTLY IF EXISTS hub_detail_reclaim_due;
RESET lock_timeout;

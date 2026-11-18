-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS change_versions_retained_pr_idx;
CREATE INDEX CONCURRENTLY change_versions_retained_pr_idx ON change_versions (organization_id,change_id,number DESC) WHERE external_provider='github' AND external_url<>'';
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS change_versions_retained_pr_idx;
RESET lock_timeout;

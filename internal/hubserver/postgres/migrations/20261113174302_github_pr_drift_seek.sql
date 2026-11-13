-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS github_pull_request_refresh_drift;
CREATE INDEX CONCURRENTLY github_pull_request_refresh_drift ON github_pull_request_refresh (organization_id,project_id,repository,pull_number) WHERE source_kind='change' AND NOT pending;
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS github_pull_request_refresh_drift;
RESET lock_timeout;

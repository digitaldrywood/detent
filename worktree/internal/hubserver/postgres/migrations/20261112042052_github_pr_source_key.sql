-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS github_pull_request_refresh_source_key;
CREATE UNIQUE INDEX CONCURRENTLY github_pull_request_refresh_source_key ON github_pull_request_refresh (organization_id,project_id,change_id,repository,pull_number) NULLS NOT DISTINCT;
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS github_pull_request_refresh_source_key;
RESET lock_timeout;

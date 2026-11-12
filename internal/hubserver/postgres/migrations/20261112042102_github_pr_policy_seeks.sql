-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS github_pull_request_refresh_policy_landing;
CREATE INDEX CONCURRENTLY github_pull_request_refresh_policy_landing ON github_pull_request_refresh (organization_id,project_id,repository,merge_sha,base_ref,pull_number,change_id ASC NULLS FIRST) WHERE merged_at IS NOT NULL;
DROP INDEX CONCURRENTLY IF EXISTS github_pull_request_refresh_policy_history;
CREATE INDEX CONCURRENTLY github_pull_request_refresh_policy_history ON github_pull_request_refresh (organization_id,project_id,repository,merge_sha,pull_number,change_id ASC NULLS FIRST) WHERE merged_at IS NOT NULL;
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS github_pull_request_refresh_policy_history;
DROP INDEX CONCURRENTLY IF EXISTS github_pull_request_refresh_policy_landing;
RESET lock_timeout;

-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS github_pull_request_refresh_policy_landing;
CREATE INDEX CONCURRENTLY github_pull_request_refresh_policy_landing ON github_pull_request_refresh (organization_id,project_id,repository,merge_sha,base_ref,repository_default_branch,pull_number,change_id ASC NULLS FIRST) WHERE merged_at IS NOT NULL AND merge_policy_bound;
DROP INDEX CONCURRENTLY IF EXISTS github_pull_request_refresh_policy_history;
CREATE INDEX CONCURRENTLY github_pull_request_refresh_policy_history ON github_pull_request_refresh (organization_id,project_id,repository,merge_sha,pull_number,change_id ASC NULLS FIRST) WHERE merged_at IS NOT NULL AND merge_policy_bound;
RESET lock_timeout;

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN RAISE EXCEPTION 'verified pull request source seeks cannot be discarded'; END; $$;
-- +goose StatementEnd

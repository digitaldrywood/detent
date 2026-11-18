-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS change_versions_policy_landing_lookup;
CREATE INDEX CONCURRENTLY change_versions_policy_landing_lookup ON change_versions (organization_id, project_id, landed_merge_sha, lower(repository), landed_base_ref, change_id, number DESC) WHERE landed_at IS NOT NULL AND landing_history_known;
DROP INDEX CONCURRENTLY IF EXISTS change_versions_policy_landing_history;
CREATE INDEX CONCURRENTLY change_versions_policy_landing_history ON change_versions (organization_id, project_id, landed_merge_sha, lower(repository), change_id, number DESC) WHERE landed_at IS NOT NULL AND landing_history_known;
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS change_versions_policy_landing_history;
DROP INDEX CONCURRENTLY IF EXISTS change_versions_policy_landing_lookup;
RESET lock_timeout;

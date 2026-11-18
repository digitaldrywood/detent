-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS issue_dependencies_progress_owner_lookup;
CREATE INDEX CONCURRENTLY issue_dependencies_progress_owner_lookup ON issue_dependencies (organization_id, dependent_project_id, dependent_issue_id, blocker_project_id, blocker_issue_id) WHERE provenance = 'progress_check';
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS issue_dependencies_progress_owner_lookup;
RESET lock_timeout;

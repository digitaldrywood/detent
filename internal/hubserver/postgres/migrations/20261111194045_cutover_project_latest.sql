-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS github_cutovers_project_latest;
CREATE INDEX CONCURRENTLY github_cutovers_project_latest ON github_cutovers(organization_id,project_id,created_at DESC,checkpoint DESC);
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY github_cutovers_project_latest;
RESET lock_timeout;

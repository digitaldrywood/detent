-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS artifact_grants_project;
CREATE INDEX CONCURRENTLY artifact_grants_project ON artifact_grants(organization_id,project_id,token_hash);
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS artifact_grants_project;
RESET lock_timeout;

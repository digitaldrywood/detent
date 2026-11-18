-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS project_artifact_retained_uploads;
CREATE INDEX CONCURRENTLY project_artifact_retained_uploads
 ON artifact_catalog_uploads(organization_id,project_id)
 WHERE state<>'deleted';
RESET lock_timeout;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS project_artifact_retained_uploads;

-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS workspace_project_created;
CREATE INDEX CONCURRENTLY workspace_project_created ON workspace_sessions(organization_id,project_id,created_at DESC,id DESC);
DROP INDEX CONCURRENTLY IF EXISTS workspace_project_state_created;
CREATE INDEX CONCURRENTLY workspace_project_state_created ON workspace_sessions(organization_id,project_id,state,created_at DESC,id DESC);
DROP INDEX CONCURRENTLY IF EXISTS workspace_subject_state_created;
CREATE INDEX CONCURRENTLY workspace_subject_state_created ON workspace_sessions(organization_id,project_id,subject_work_item_id,state,created_at DESC,id DESC);
DROP INDEX CONCURRENTLY IF EXISTS workspace_subject_created;
CREATE INDEX CONCURRENTLY workspace_subject_created ON workspace_sessions(organization_id,project_id,subject_work_item_id,created_at DESC,id DESC);
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY workspace_subject_created;
DROP INDEX CONCURRENTLY workspace_subject_state_created;
DROP INDEX CONCURRENTLY workspace_project_state_created;
DROP INDEX CONCURRENTLY workspace_project_created;
RESET lock_timeout;

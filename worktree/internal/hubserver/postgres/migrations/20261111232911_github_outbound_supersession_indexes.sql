-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
ALTER TABLE github_outbound VALIDATE CONSTRAINT github_outbound_status_check;
ALTER TABLE github_outbound VALIDATE CONSTRAINT github_outbound_supersession_check;
DROP INDEX CONCURRENTLY IF EXISTS github_outbound_active_ready;
CREATE INDEX CONCURRENTLY github_outbound_active_ready ON github_outbound(organization_id,next_attempt_at,id) WHERE status IN ('pending','retrying','processing');
DROP INDEX CONCURRENTLY IF EXISTS github_outbound_ready;
DROP INDEX CONCURRENTLY IF EXISTS github_outbound_coalescing;
CREATE INDEX CONCURRENTLY github_outbound_coalescing ON github_outbound(organization_id,project_id,coalesce_key,id) WHERE status IN ('pending','retrying','processing') AND coalesce_key <> '';
DROP INDEX CONCURRENTLY IF EXISTS github_imports_work_item;
CREATE INDEX CONCURRENTLY github_imports_work_item ON github_imports(organization_id,project_id,work_item_id) WHERE work_item_id <> '';
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS github_outbound_ready;
CREATE INDEX CONCURRENTLY github_outbound_ready ON github_outbound(organization_id,next_attempt_at,id) WHERE status <> 'completed';
DROP INDEX CONCURRENTLY IF EXISTS github_outbound_active_ready;
DROP INDEX CONCURRENTLY IF EXISTS github_imports_work_item;
DROP INDEX CONCURRENTLY IF EXISTS github_outbound_coalescing;
RESET lock_timeout;

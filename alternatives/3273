-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS collaboration_events_report_state_seek;
CREATE INDEX CONCURRENTLY collaboration_events_report_state_seek ON collaboration_events (organization_id,project_id,to_state,recorded_at,work_item_id) WHERE type='workflow.transitioned';
DROP INDEX CONCURRENTLY IF EXISTS collaboration_events_report_item_seek;
CREATE INDEX CONCURRENTLY collaboration_events_report_item_seek ON collaboration_events (organization_id,project_id,work_item_id,recorded_at,sequence) WHERE type='workflow.transitioned';
DROP INDEX CONCURRENTLY IF EXISTS issues_report_principal_seek;
CREATE INDEX CONCURRENTLY issues_report_principal_seek ON issues (organization_id,project_id,actor_principal_id,created_at);
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY issues_report_principal_seek;
DROP INDEX CONCURRENTLY collaboration_events_report_item_seek;
DROP INDEX CONCURRENTLY collaboration_events_report_state_seek;
RESET lock_timeout;

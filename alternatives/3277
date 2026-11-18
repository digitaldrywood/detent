-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS collaboration_events_report_item_cover;
CREATE INDEX CONCURRENTLY collaboration_events_report_item_cover ON collaboration_events (organization_id,project_id,work_item_id,recorded_at,sequence) INCLUDE (from_state,to_state) WHERE type='workflow.transitioned';
DROP INDEX CONCURRENTLY IF EXISTS collaboration_events_report_item_seek;
DROP INDEX CONCURRENTLY IF EXISTS collaboration_events_report_first_transition;
CREATE INDEX CONCURRENTLY collaboration_events_report_first_transition ON collaboration_events (organization_id,project_id,work_item_id,sequence) INCLUDE (from_state) WHERE type='workflow.transitioned';
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS collaboration_events_report_first_transition;
DROP INDEX CONCURRENTLY IF EXISTS collaboration_events_report_item_seek;
CREATE INDEX CONCURRENTLY collaboration_events_report_item_seek ON collaboration_events (organization_id,project_id,work_item_id,recorded_at,sequence) WHERE type='workflow.transitioned';
DROP INDEX CONCURRENTLY IF EXISTS collaboration_events_report_item_cover;
RESET lock_timeout;

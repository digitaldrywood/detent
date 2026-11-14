-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS collaboration_events_report_from_state_seek;
CREATE INDEX CONCURRENTLY collaboration_events_report_from_state_seek ON collaboration_events (organization_id,project_id,from_state,recorded_at,work_item_id) WHERE type='workflow.transitioned';
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY collaboration_events_report_from_state_seek;
RESET lock_timeout;

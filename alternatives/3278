-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS collaboration_events_report_created_seek;
CREATE INDEX CONCURRENTLY collaboration_events_report_created_seek ON collaboration_events (organization_id,project_id,work_item_id,sequence) INCLUDE (recorded_at) WHERE type='issue.created';
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS collaboration_events_report_created_seek;
RESET lock_timeout;

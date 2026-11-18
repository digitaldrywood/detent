-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS collaboration_events_analytics_residence_seek;
CREATE INDEX CONCURRENTLY collaboration_events_analytics_residence_seek ON collaboration_events (organization_id,project_id,work_item_id,sequence) WHERE type='workflow.transitioned' OR type='scheduler.decision' AND scheduler_outcome='claimed';
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY collaboration_events_analytics_residence_seek;
RESET lock_timeout;

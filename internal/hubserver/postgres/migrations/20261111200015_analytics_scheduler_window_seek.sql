-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS collaboration_events_analytics_scheduler_seek;
CREATE INDEX CONCURRENTLY collaboration_events_analytics_scheduler_seek ON collaboration_events (organization_id,project_id,scheduler_outcome,recorded_at,id) WHERE type='scheduler.decision';
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY collaboration_events_analytics_scheduler_seek;
RESET lock_timeout;

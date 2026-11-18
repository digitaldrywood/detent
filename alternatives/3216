-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS collaboration_events_analytics_failure_seek;
CREATE INDEX CONCURRENTLY collaboration_events_analytics_failure_seek ON collaboration_events (organization_id,project_id,recorded_at DESC,id DESC) WHERE type IN('run.observed','run.checkpointed','run.finished');
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY collaboration_events_analytics_failure_seek;
RESET lock_timeout;

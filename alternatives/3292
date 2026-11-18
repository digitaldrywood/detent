-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS collaboration_events_activity_history_cutoff_idx;
CREATE INDEX CONCURRENTLY collaboration_events_activity_history_cutoff_idx
    ON collaboration_events (organization_id, project_id, work_item_id, sequence DESC, recorded_at)
    INCLUDE (type, from_state, to_state) WHERE type IN ('issue.created','workflow.transitioned');
DROP INDEX CONCURRENTLY IF EXISTS collaboration_events_activity_history_idx;
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS collaboration_events_activity_history_idx;
CREATE INDEX CONCURRENTLY collaboration_events_activity_history_idx
    ON collaboration_events (organization_id, project_id, work_item_id, sequence DESC)
    INCLUDE (type, from_state, to_state, recorded_at) WHERE type IN ('issue.created','workflow.transitioned');
DROP INDEX CONCURRENTLY IF EXISTS collaboration_events_activity_history_cutoff_idx;
RESET lock_timeout;

-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS collaboration_events_claimed_decision_idx;
CREATE INDEX CONCURRENTLY collaboration_events_claimed_decision_idx
ON collaboration_events(organization_id,project_id,work_item_id,sequence DESC)
WHERE type='scheduler.decision' AND scheduler_outcome='claimed';
RESET lock_timeout;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS collaboration_events_claimed_decision_idx;

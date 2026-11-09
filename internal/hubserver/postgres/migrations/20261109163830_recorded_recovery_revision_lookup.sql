-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS collaboration_events_recovery_revision_idx;
CREATE INDEX CONCURRENTLY collaboration_events_recovery_revision_idx ON collaboration_events (organization_id,project_id,work_item_id,event_revision,sequence) WHERE event_revision>0;
RESET lock_timeout;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS collaboration_events_recovery_revision_idx;

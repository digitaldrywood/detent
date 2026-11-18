-- +goose NO TRANSACTION
-- +goose Up
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS collaboration_events_diagnostics_merge_entry_idx;
CREATE INDEX CONCURRENTLY collaboration_events_diagnostics_merge_entry_idx ON collaboration_events (organization_id,project_id,work_item_id,recorded_at DESC,id DESC) WHERE type='workflow.transitioned' AND to_state='Merging';
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS collaboration_events_diagnostics_merge_entry_idx;
RESET lock_timeout;

-- +goose Up
CREATE INDEX collaboration_events_activity_history_idx ON collaboration_events (organization_id,project_id,work_item_id,sequence DESC) INCLUDE (type,from_state,to_state,recorded_at) WHERE type IN ('issue.created','workflow.transitioned');

-- +goose Down
DROP INDEX collaboration_events_activity_history_idx;

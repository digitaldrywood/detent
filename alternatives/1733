-- +goose Up
ALTER TABLE collaboration_events ADD COLUMN analytics_recorded_ns INTEGER;
ALTER TABLE collaboration_events ADD COLUMN analytics_from_state TEXT;
ALTER TABLE collaboration_events ADD COLUMN analytics_to_state TEXT;
ALTER TABLE collaboration_events ADD COLUMN analytics_attempt_id TEXT;
ALTER TABLE collaboration_events ADD COLUMN analytics_skip_key TEXT;
ALTER TABLE collaboration_events ADD COLUMN analytics_projection_version INTEGER;
CREATE INDEX collaboration_events_analytics_item_pending ON collaboration_events(organization_id,project_id,work_item_id) WHERE analytics_projection_version IS NULL;
CREATE INDEX collaboration_events_analytics_pending ON collaboration_events(id) WHERE analytics_projection_version IS NULL;

-- +goose Down
DROP INDEX collaboration_events_analytics_pending;
DROP INDEX collaboration_events_analytics_item_pending;
ALTER TABLE collaboration_events DROP COLUMN analytics_projection_version;
ALTER TABLE collaboration_events DROP COLUMN analytics_skip_key;
ALTER TABLE collaboration_events DROP COLUMN analytics_attempt_id;
ALTER TABLE collaboration_events DROP COLUMN analytics_to_state;
ALTER TABLE collaboration_events DROP COLUMN analytics_from_state;
ALTER TABLE collaboration_events DROP COLUMN analytics_recorded_ns;

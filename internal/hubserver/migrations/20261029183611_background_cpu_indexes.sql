-- +goose Up
CREATE INDEX native_attempts_usage_drain ON native_attempts(organization_id,project_id,work_item_id,id);
CREATE INDEX collaboration_events_usage_drain ON collaboration_events(organization_id,project_id,work_item_id,analytics_attempt_id);
CREATE INDEX attempt_usage_drain ON attempt_usage(attempt_id);

-- +goose Down
DROP INDEX attempt_usage_drain;
DROP INDEX collaboration_events_usage_drain;
DROP INDEX native_attempts_usage_drain;

-- +goose Up
DROP INDEX analytics_attempt_window;
DROP INDEX analytics_attempt_updated;

-- +goose Down
CREATE INDEX analytics_attempt_window ON analytics_hourly_rollups(organization_id,project_id,julianday(json_started_at),julianday(json_updated_at)) WHERE metric='attempt';
CREATE INDEX analytics_attempt_updated ON analytics_hourly_rollups(organization_id,project_id,julianday(json_updated_at)) WHERE metric='attempt';

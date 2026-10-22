-- +goose Up
CREATE INDEX native_attempts_reports_updated ON native_attempts(organization_id,project_id,updated_at,started_at);
CREATE INDEX analytics_attempt_reports_updated ON analytics_hourly_rollups(organization_id,project_id,json_updated_at,json_started_at) WHERE metric='attempt';

-- +goose Down
DROP INDEX analytics_attempt_reports_updated;
DROP INDEX native_attempts_reports_updated;

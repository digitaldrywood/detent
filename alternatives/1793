-- +goose Up
CREATE INDEX analytics_attempt_payload_pending_scope ON analytics_hourly_rollups(organization_id,project_id) WHERE metric='attempt' AND payload_object_key='' AND data_json<>'{}';

-- +goose Down
DROP INDEX analytics_attempt_payload_pending_scope;

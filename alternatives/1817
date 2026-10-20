-- +goose Up
ALTER TABLE analytics_hourly_rollups ADD COLUMN attempt_role TEXT GENERATED ALWAYS AS (coalesce(json_extract(data_json,'$.runtime.identity.role'),json_extract(data_json,'$.identity.role'))) VIRTUAL;
DROP INDEX analytics_attempt_started;
CREATE INDEX analytics_attempt_started ON analytics_hourly_rollups(organization_id,project_id,json_started_at,dimension,json_updated_at,attempt_role,json_work_item_id) WHERE metric='attempt';
CREATE INDEX change_requests_analytics_landed ON change_requests(organization_id,project_id,json_extract(record_json,'$.landed.landed_at'),id);
CREATE INDEX quality_landings_analytics_landed ON quality_landings(json_extract(record_json,'$.landed_at'),version_id);
CREATE INDEX change_evidence_analytics_review ON change_evidence(julianday(json_extract(record_json,'$.created_at')),sequence,change_id,version_id) WHERE kind='review' AND json_extract(record_json,'$.decision')='changes_requested';
CREATE INDEX change_evidence_analytics_escape ON change_evidence(julianday(json_extract(record_json,'$.escape.observed_at')),sequence,change_id,version_id) WHERE kind='discussion' AND json_extract(record_json,'$.escape') IS NOT NULL;
CREATE INDEX analytics_event_order ON analytics_hourly_rollups(organization_id,project_id,json_type,json_recorded_at,json_sequence,dimension) WHERE metric='event';
CREATE INDEX collaboration_events_analytics_order ON collaboration_events(organization_id,project_id,type,recorded_at,sequence,id);
INSERT INTO analytics_rollup_backfill SELECT organization_id,id FROM projects WHERE true ON CONFLICT DO NOTHING;
DELETE FROM analytics_rollup_positions;

-- +goose Down
DROP INDEX collaboration_events_analytics_order;
DROP INDEX analytics_event_order;
DROP INDEX change_evidence_analytics_escape;
DROP INDEX change_evidence_analytics_review;
DROP INDEX quality_landings_analytics_landed;
DROP INDEX change_requests_analytics_landed;
DROP INDEX analytics_attempt_started;
ALTER TABLE analytics_hourly_rollups DROP COLUMN attempt_role;
CREATE INDEX analytics_attempt_started ON analytics_hourly_rollups(organization_id,project_id,json_started_at,dimension,json_updated_at) WHERE metric='attempt';
INSERT INTO analytics_rollup_backfill SELECT organization_id,id FROM projects WHERE true ON CONFLICT DO NOTHING;
DELETE FROM analytics_rollup_positions;

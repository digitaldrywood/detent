-- +goose Up
DROP TRIGGER storage_conversion_enqueue_analytics;
ALTER TABLE analytics_hourly_rollups ADD COLUMN payload_object_key TEXT NOT NULL DEFAULT '';
CREATE INDEX analytics_attempt_payload_pending ON analytics_hourly_rollups(payload_object_key) WHERE metric='attempt' AND payload_object_key='' AND data_json<>'{}';
DROP VIEW analytics_native_attempts;
CREATE VIEW analytics_native_attempts AS
SELECT r.organization_id,r.project_id,r.dimension AS id,r.work_item_id,r.lease_id,r.status,r.started_at,r.updated_at,
COALESCE(NULLIF(r.payload_object_key,''),NULLIF(r.data_json,'{}'),NULLIF(a.payload_object_key,''),a.data_json,'{}') AS data_json
FROM analytics_hourly_rollups r LEFT JOIN native_attempts a ON a.id=r.dimension AND a.organization_id=r.organization_id AND a.project_id=r.project_id WHERE r.metric='attempt';

-- +goose Down
CREATE TABLE analytics_object_rollback_guard(value INTEGER CHECK(value=0));
INSERT INTO analytics_object_rollback_guard SELECT EXISTS(SELECT 1 FROM analytics_hourly_rollups WHERE payload_object_key<>'');
DROP TABLE analytics_object_rollback_guard;
DROP VIEW analytics_native_attempts;
CREATE VIEW analytics_native_attempts AS SELECT organization_id,project_id,dimension AS id,work_item_id,lease_id,status,started_at,updated_at,data_json FROM analytics_hourly_rollups WHERE metric='attempt';
DROP INDEX analytics_attempt_payload_pending;
ALTER TABLE analytics_hourly_rollups DROP COLUMN payload_object_key;

-- +goose StatementBegin
CREATE TRIGGER storage_conversion_enqueue_analytics AFTER INSERT ON analytics_hourly_rollups
BEGIN
INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,format_version,through_rowid,updated_at)
SELECT r.organization_id,r.project_id,'analytics','',1,r.rowid,'' FROM analytics_hourly_rollups r
WHERE (r.metric='attempt') AND r.rowid=NEW.rowid
ON CONFLICT(organization_id,project_id,family,aggregate_id) DO UPDATE SET
 through_rowid=max(through_rowid,excluded.through_rowid),
 cursor_rowid=CASE WHEN format_version<>1 THEN 0 ELSE cursor_rowid END,
 complete=0,
 format_version=1;
UPDATE analytics_hourly_rollups SET conversion_state=CASE WHEN changes()=0 THEN 2 ELSE 1 END WHERE rowid=NEW.rowid;
END;
-- +goose StatementEnd

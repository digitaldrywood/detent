-- +goose Up
ALTER TABLE analytics_hourly_rollups ADD COLUMN sample_created TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN sample_initial TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN sample_state TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN sample_entered TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN sample_started TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN sample_partial INTEGER;
ALTER TABLE analytics_hourly_rollups ADD COLUMN sample_events INTEGER;
ALTER TABLE analytics_hourly_rollups ADD COLUMN sample_transitions INTEGER;
ALTER TABLE analytics_hourly_rollups ADD COLUMN sample_queue REAL;
ALTER TABLE analytics_hourly_rollups ADD COLUMN sample_queue_to TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN sample_last_done TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN sample_merge_claim TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN sample_sequence INTEGER;
ALTER TABLE analytics_hourly_rollups ADD COLUMN sample_queue_ns INTEGER;
ALTER TABLE analytics_hourly_rollups ADD COLUMN sample_from TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN sample_to TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN sample_claim TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN sample_start TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN sample_seconds_ns INTEGER;
ALTER TABLE analytics_hourly_rollups ADD COLUMN sample_valid INTEGER;
ALTER TABLE analytics_hourly_rollups ADD COLUMN sample_storage_version INTEGER NOT NULL DEFAULT 0;
CREATE TABLE analytics_sample_lanes (
 organization_id TEXT NOT NULL,
 project_id TEXT NOT NULL,
 metric TEXT NOT NULL,
 hour INTEGER NOT NULL,
 dimension TEXT NOT NULL,
 lane TEXT NOT NULL,
 seconds REAL,
 nanoseconds INTEGER,
 PRIMARY KEY(organization_id,project_id,metric,hour,dimension,lane),
 FOREIGN KEY(organization_id,project_id,metric,hour,dimension) REFERENCES analytics_hourly_rollups(organization_id,project_id,metric,hour,dimension) ON DELETE CASCADE ON UPDATE CASCADE
) WITHOUT ROWID;
CREATE TABLE analytics_sample_causes (
 organization_id TEXT NOT NULL,
 project_id TEXT NOT NULL,
 metric TEXT NOT NULL,
 hour INTEGER NOT NULL,
 dimension TEXT NOT NULL,
 cause TEXT NOT NULL,
 from_state TEXT NOT NULL,
 reason_detail TEXT NOT NULL,
 count INTEGER NOT NULL,
 PRIMARY KEY(organization_id,project_id,metric,hour,dimension,cause),
 FOREIGN KEY(organization_id,project_id,metric,hour,dimension) REFERENCES analytics_hourly_rollups(organization_id,project_id,metric,hour,dimension) ON DELETE CASCADE ON UPDATE CASCADE
) WITHOUT ROWID;
DROP INDEX analytics_attempt_payload_pending;
DROP INDEX analytics_attempt_payload_pending_scope;
CREATE INDEX analytics_payload_pending_scope ON analytics_hourly_rollups(organization_id,project_id) WHERE data_json<>'{}';

-- +goose Down
CREATE TABLE analytics_sample_rollback_guard(value INTEGER CHECK(value=0));
INSERT INTO analytics_sample_rollback_guard SELECT EXISTS(SELECT 1 FROM analytics_hourly_rollups WHERE sample_storage_version=1);
DROP TABLE analytics_sample_rollback_guard;
DROP INDEX analytics_payload_pending_scope;
CREATE INDEX analytics_attempt_payload_pending ON analytics_hourly_rollups(payload_object_key) WHERE metric='attempt' AND payload_object_key='' AND data_json<>'{}';
CREATE INDEX analytics_attempt_payload_pending_scope ON analytics_hourly_rollups(organization_id,project_id) WHERE metric='attempt' AND payload_object_key='' AND data_json<>'{}';
DROP TABLE analytics_sample_causes;
DROP TABLE analytics_sample_lanes;
ALTER TABLE analytics_hourly_rollups DROP COLUMN sample_storage_version;
ALTER TABLE analytics_hourly_rollups DROP COLUMN sample_valid;
ALTER TABLE analytics_hourly_rollups DROP COLUMN sample_seconds_ns;
ALTER TABLE analytics_hourly_rollups DROP COLUMN sample_start;
ALTER TABLE analytics_hourly_rollups DROP COLUMN sample_claim;
ALTER TABLE analytics_hourly_rollups DROP COLUMN sample_to;
ALTER TABLE analytics_hourly_rollups DROP COLUMN sample_from;
ALTER TABLE analytics_hourly_rollups DROP COLUMN sample_queue_ns;
ALTER TABLE analytics_hourly_rollups DROP COLUMN sample_sequence;
ALTER TABLE analytics_hourly_rollups DROP COLUMN sample_merge_claim;
ALTER TABLE analytics_hourly_rollups DROP COLUMN sample_last_done;
ALTER TABLE analytics_hourly_rollups DROP COLUMN sample_queue_to;
ALTER TABLE analytics_hourly_rollups DROP COLUMN sample_queue;
ALTER TABLE analytics_hourly_rollups DROP COLUMN sample_transitions;
ALTER TABLE analytics_hourly_rollups DROP COLUMN sample_events;
ALTER TABLE analytics_hourly_rollups DROP COLUMN sample_partial;
ALTER TABLE analytics_hourly_rollups DROP COLUMN sample_started;
ALTER TABLE analytics_hourly_rollups DROP COLUMN sample_entered;
ALTER TABLE analytics_hourly_rollups DROP COLUMN sample_state;
ALTER TABLE analytics_hourly_rollups DROP COLUMN sample_initial;
ALTER TABLE analytics_hourly_rollups DROP COLUMN sample_created;

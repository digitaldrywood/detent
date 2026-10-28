-- +goose Up
DROP TRIGGER IF EXISTS analytics_event_counts;
DROP TRIGGER IF EXISTS storage_conversion_enqueue_analytics;
DROP VIEW IF EXISTS analytics_issues;
DROP VIEW IF EXISTS analytics_native_attempts;
DROP VIEW IF EXISTS analytics_attempt_usage;
DROP VIEW IF EXISTS analytics_leases;
DROP INDEX IF EXISTS analytics_activity_issue_edges;
DROP INDEX IF EXISTS analytics_attempt_failed;
DROP INDEX IF EXISTS analytics_attempt_identity;
DROP INDEX IF EXISTS analytics_attempt_item;
DROP INDEX IF EXISTS analytics_attempt_reports_updated;
DROP INDEX IF EXISTS analytics_attempt_started;
DROP INDEX IF EXISTS analytics_attempt_updated;
DROP INDEX IF EXISTS analytics_attempt_window;
DROP INDEX IF EXISTS analytics_capacity_order;
DROP INDEX IF EXISTS analytics_capacity_window;
DROP INDEX IF EXISTS analytics_completion;
DROP INDEX IF EXISTS analytics_event_item;
DROP INDEX IF EXISTS analytics_event_order;
DROP INDEX IF EXISTS analytics_event_window;
DROP INDEX IF EXISTS analytics_issue_native;
DROP INDEX IF EXISTS analytics_lease_source;
DROP INDEX IF EXISTS analytics_merge_entry;
DROP INDEX IF EXISTS analytics_projection_pending;
DROP INDEX IF EXISTS analytics_projection_rowid_pending;
DROP INDEX IF EXISTS analytics_projection_source;
DROP INDEX IF EXISTS analytics_residence_edge;
DROP INDEX IF EXISTS analytics_residence_hour;
DROP INDEX IF EXISTS analytics_residence_identity;
DROP INDEX IF EXISTS analytics_sample_compaction_order;
DROP INDEX IF EXISTS analytics_skip_order;
DROP INDEX IF EXISTS analytics_skip_window;
DROP INDEX IF EXISTS analytics_snapshot_identity;
DROP INDEX IF EXISTS analytics_usage_attempt;
DROP INDEX IF EXISTS analytics_usage_source;
DROP INDEX IF EXISTS analytics_usage_window;
DROP INDEX IF EXISTS storage_discovery_analytics;
DROP INDEX IF EXISTS storage_queued_analytics;
ALTER TABLE analytics_hourly_rollups RENAME TO analytics_rollup_legacy;
CREATE TABLE analytics_hourly_rollups (
 organization_id TEXT NOT NULL,
 project_id TEXT NOT NULL,
 hour INTEGER NOT NULL,
 metric TEXT NOT NULL CHECK(metric IN ('ready','merge_entered','skipped')),
 dimension TEXT NOT NULL,
 value INTEGER NOT NULL,
 skip_source TEXT NOT NULL DEFAULT '',
 skip_reason TEXT NOT NULL DEFAULT '',
 last_at TEXT NOT NULL DEFAULT '',
 last_ns INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(organization_id,project_id,metric,hour,dimension),
 FOREIGN KEY(organization_id,project_id) REFERENCES projects(organization_id,id)
);
ALTER TABLE analytics_rollup_positions ADD COLUMN aggregate_version INTEGER NOT NULL DEFAULT 0;
CREATE INDEX attempt_usage_analytics_window ON attempt_usage(organization_id,project_id,period,provider,model,attempt_id);
CREATE INDEX native_attempts_analytics_order ON native_attempts(organization_id,project_id,started_at,id,updated_at,work_item_id);
ALTER TABLE storage_conversion_progress ADD COLUMN dispatch_ns INTEGER NOT NULL DEFAULT 0;
CREATE INDEX storage_conversion_dispatch ON storage_conversion_progress(dispatch_ns,organization_id,project_id,family,aggregate_id) WHERE family<>'analytics' AND (complete=0 OR format_version<>1);
-- +goose Down
SELECT 1;

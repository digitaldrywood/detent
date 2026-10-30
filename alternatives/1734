-- +goose Up
DROP TRIGGER analytics_event_counts;
DROP VIEW analytics_issues;
DROP VIEW analytics_native_attempts;
DROP VIEW analytics_attempt_usage;
DROP VIEW analytics_leases;
DROP INDEX analytics_event_item;
DROP INDEX analytics_event_window;
DROP INDEX analytics_usage_window;
DROP INDEX analytics_residence_edge;
DROP INDEX analytics_skip_window;
DROP INDEX analytics_skip_order;
DROP INDEX analytics_capacity_window;
DROP INDEX analytics_residence_identity;
DROP INDEX analytics_completion;
DROP INDEX analytics_usage_attempt;
DROP INDEX analytics_merge_entry;
DROP INDEX analytics_attempt_item;
DROP INDEX analytics_attempt_started;
DROP INDEX analytics_attempt_failed;
DROP INDEX analytics_issue_native;
DROP INDEX analytics_capacity_order;
DROP INDEX analytics_event_order;
DROP INDEX analytics_attempt_reports_updated;
ALTER TABLE analytics_hourly_rollups DROP COLUMN json_work_item_id;
ALTER TABLE analytics_hourly_rollups DROP COLUMN json_sequence;
ALTER TABLE analytics_hourly_rollups DROP COLUMN json_type;
ALTER TABLE analytics_hourly_rollups DROP COLUMN json_recorded_at;
ALTER TABLE analytics_hourly_rollups DROP COLUMN json_started_at;
ALTER TABLE analytics_hourly_rollups DROP COLUMN json_updated_at;
ALTER TABLE analytics_hourly_rollups DROP COLUMN json_attempt_id;
ALTER TABLE analytics_hourly_rollups DROP COLUMN json_period;
ALTER TABLE analytics_hourly_rollups DROP COLUMN attempt_role;
ALTER TABLE analytics_hourly_rollups ADD COLUMN work_item_id TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN sequence INTEGER;
ALTER TABLE analytics_hourly_rollups ADD COLUMN type TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN recorded_at TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN started_at TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN updated_at TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN attempt_id TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN period TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN attempt_role TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN sample_id TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN sample_at TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN sample_ns INTEGER;
ALTER TABLE analytics_hourly_rollups ADD COLUMN sample_start_ns INTEGER;
ALTER TABLE analytics_hourly_rollups ADD COLUMN sample_from_ns INTEGER;
ALTER TABLE analytics_hourly_rollups ADD COLUMN skip_source TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN skip_reason TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN last_at TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN last_ns INTEGER;
ALTER TABLE analytics_hourly_rollups ADD COLUMN skip_key TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN issue_id TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN native_id TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN title TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN created_at TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN workflow_state_id TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN lease_id TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN status TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN provider TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN model TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN input INTEGER;
ALTER TABLE analytics_hourly_rollups ADD COLUMN cached_input INTEGER;
ALTER TABLE analytics_hourly_rollups ADD COLUMN output INTEGER;
ALTER TABLE analytics_hourly_rollups ADD COLUMN currency TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN acquired_at TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN released_at TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN expires_at TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN failed_at TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN projection_version INTEGER;
ALTER TABLE analytics_hourly_rollups ADD COLUMN failed_day REAL;
ALTER TABLE analytics_hourly_rollups ADD COLUMN recorded_day REAL;
CREATE INDEX analytics_event_item ON analytics_hourly_rollups(organization_id,project_id,work_item_id,sequence) WHERE metric='event';
CREATE INDEX analytics_event_window ON analytics_hourly_rollups(organization_id,project_id,type,recorded_day) WHERE metric='event';
CREATE INDEX analytics_usage_window ON analytics_hourly_rollups(organization_id,project_id,period,provider,model,attempt_id) WHERE metric='usage';
CREATE INDEX analytics_residence_edge ON analytics_hourly_rollups(organization_id,project_id,sample_id,hour,sample_ns DESC,value DESC) WHERE metric='residence_edge';
CREATE INDEX analytics_skip_window ON analytics_hourly_rollups(organization_id,project_id,sample_ns,dimension) WHERE metric='skip_sample';
CREATE INDEX analytics_skip_order ON analytics_hourly_rollups(organization_id,project_id,sample_at,dimension) WHERE metric='skip_sample';
CREATE INDEX analytics_capacity_window ON analytics_hourly_rollups(organization_id,project_id,sample_ns,dimension) WHERE metric='capacity_claim';
CREATE INDEX analytics_residence_identity ON analytics_hourly_rollups(organization_id,project_id,sample_id,metric) WHERE metric IN ('residence_hour','residence_edge','completion','residence_merge','merge_entry');
CREATE INDEX analytics_completion ON analytics_hourly_rollups(organization_id,project_id,sample_id,sample_ns,value) WHERE metric='completion';
CREATE INDEX analytics_usage_attempt ON analytics_hourly_rollups(organization_id,project_id,attempt_id,period,value,input,output) WHERE metric='usage';
CREATE INDEX analytics_merge_entry ON analytics_hourly_rollups(organization_id,project_id,sample_id,sample_ns) WHERE metric='merge_entry';
CREATE INDEX analytics_attempt_item ON analytics_hourly_rollups(organization_id,project_id,work_item_id,dimension) WHERE metric='attempt';
CREATE INDEX analytics_attempt_started ON analytics_hourly_rollups(organization_id,project_id,started_at,dimension,updated_at,attempt_role,work_item_id) WHERE metric='attempt';
CREATE INDEX analytics_attempt_failed ON analytics_hourly_rollups(organization_id,project_id,failed_day,dimension) WHERE metric='attempt' AND failed_at IS NOT NULL;
CREATE INDEX analytics_issue_native ON analytics_hourly_rollups(organization_id,project_id,native_id) WHERE metric='issue';
CREATE INDEX analytics_capacity_order ON analytics_hourly_rollups(organization_id,project_id,sample_at,dimension,sample_ns) WHERE metric='capacity_claim';
CREATE INDEX analytics_event_order ON analytics_hourly_rollups(organization_id,project_id,type,recorded_at,sequence,dimension) WHERE metric='event';
CREATE INDEX analytics_attempt_reports_updated ON analytics_hourly_rollups(organization_id,project_id,updated_at,started_at) WHERE metric='attempt';
CREATE INDEX analytics_projection_pending ON analytics_hourly_rollups(organization_id,project_id,metric,hour,dimension) WHERE projection_version IS NULL;
ALTER TABLE analytics_hourly_rollups ADD COLUMN scheduler_outcome TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN scheduler_source TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN scheduler_reason TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN analytics_from_state TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN analytics_to_state TEXT;
ALTER TABLE analytics_hourly_rollups ADD COLUMN analytics_attempt_id TEXT;
CREATE VIEW analytics_issues AS SELECT organization_id,project_id,issue_id AS id,native_id,title,created_at,updated_at,workflow_state_id FROM analytics_hourly_rollups WHERE metric='issue';
CREATE VIEW analytics_native_attempts AS SELECT organization_id,project_id,dimension AS id,work_item_id,lease_id,status,started_at,updated_at,data_json FROM analytics_hourly_rollups WHERE metric='attempt';
CREATE VIEW analytics_attempt_usage AS SELECT organization_id,project_id,attempt_id,period,provider,model,input,cached_input,output,value AS cost_estimate,currency,updated_at FROM analytics_hourly_rollups WHERE metric='usage';
CREATE VIEW analytics_leases AS SELECT organization_id,project_id,lease_id,issue_id,acquired_at,released_at,expires_at FROM analytics_hourly_rollups WHERE metric='lease';
-- +goose StatementBegin
CREATE TRIGGER analytics_event_counts AFTER INSERT ON analytics_hourly_rollups WHEN NEW.metric='event' AND NEW.projection_version=1
BEGIN
 INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json,projection_version)
 SELECT NEW.organization_id,NEW.project_id,NEW.hour,'ready','',1,'{}',1 WHERE NEW.type='scheduler.decision' AND NEW.scheduler_outcome='ready'
 ON CONFLICT(organization_id,project_id,metric,hour,dimension) DO UPDATE SET value=value+1;
 INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json,projection_version)
 SELECT NEW.organization_id,NEW.project_id,NEW.hour,'merge_entered','',1,'{}',1 WHERE NEW.type='workflow.transitioned' AND NEW.analytics_to_state='Merging'
 ON CONFLICT(organization_id,project_id,metric,hour,dimension) DO UPDATE SET value=value+1;
 INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json,skip_source,skip_reason,last_at,last_ns,projection_version)
 SELECT NEW.organization_id,NEW.project_id,NEW.hour,'skipped',NEW.skip_key,1,'{}',coalesce(NEW.scheduler_source,''),coalesce(NEW.scheduler_reason,''),NEW.recorded_at,NEW.sample_ns,1
 WHERE NEW.type='scheduler.decision' AND NEW.scheduler_outcome='skipped'
 ON CONFLICT(organization_id,project_id,metric,hour,dimension) DO UPDATE SET value=value+1,last_at=CASE WHEN excluded.last_ns>last_ns THEN excluded.last_at ELSE last_at END,last_ns=max(last_ns,excluded.last_ns);
END;
-- +goose StatementEnd
-- +goose Down
SELECT 1;

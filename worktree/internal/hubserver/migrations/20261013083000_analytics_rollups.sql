-- +goose Up
CREATE TABLE analytics_hourly_rollups (
 organization_id TEXT NOT NULL,
 project_id TEXT NOT NULL,
 hour INTEGER NOT NULL,
 metric TEXT NOT NULL,
 dimension TEXT NOT NULL,
 value REAL NOT NULL,
 data_json TEXT NOT NULL CHECK(json_valid(data_json)),
 PRIMARY KEY(organization_id,project_id,metric,hour,dimension),
 FOREIGN KEY(organization_id,project_id) REFERENCES projects(organization_id,id)
);
CREATE TABLE analytics_rollup_positions (
 organization_id TEXT NOT NULL,
 project_id TEXT NOT NULL,
 work_item_id TEXT NOT NULL,
 sequence INTEGER NOT NULL,
 failed INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(organization_id,project_id,work_item_id)
);
CREATE INDEX analytics_rollup_failed ON analytics_rollup_positions(organization_id,project_id,work_item_id) WHERE failed=1;
CREATE INDEX analytics_rollup_item ON analytics_rollup_positions(work_item_id);
CREATE TABLE analytics_rollup_backfill (
 organization_id TEXT NOT NULL,
 project_id TEXT NOT NULL,
 PRIMARY KEY(organization_id,project_id)
);
INSERT INTO analytics_rollup_backfill SELECT organization_id,id FROM projects;
CREATE TABLE analytics_rollup_dirty (work_item_id TEXT PRIMARY KEY);
CREATE INDEX leases_issue_idx ON leases(issue_id);
CREATE VIEW analytics_issues AS SELECT organization_id,project_id,json_extract(data_json,'$.id') AS id,json_extract(data_json,'$.native_id') AS native_id,json_extract(data_json,'$.title') AS title,json_extract(data_json,'$.created_at') AS created_at,json_extract(data_json,'$.updated_at') AS updated_at,json_extract(data_json,'$.workflow_state_id') AS workflow_state_id FROM analytics_hourly_rollups WHERE metric='issue';
CREATE VIEW analytics_native_attempts AS SELECT organization_id,project_id,json_extract(data_json,'$.id') AS id,json_extract(data_json,'$.work_item_id') AS work_item_id,json_extract(data_json,'$.lease_id') AS lease_id,json_extract(data_json,'$.status') AS status,json_extract(data_json,'$.started_at') AS started_at,json_extract(data_json,'$.updated_at') AS updated_at,json_extract(data_json,'$.data_json') AS data_json FROM analytics_hourly_rollups WHERE metric='attempt';
CREATE VIEW analytics_attempt_usage AS SELECT organization_id,project_id,json_extract(data_json,'$.attempt_id') AS attempt_id,json_extract(data_json,'$.period') AS period,json_extract(data_json,'$.provider') AS provider,json_extract(data_json,'$.model') AS model,json_extract(data_json,'$.input') AS input,json_extract(data_json,'$.cached_input') AS cached_input,json_extract(data_json,'$.output') AS output,value AS cost_estimate,json_extract(data_json,'$.currency') AS currency,json_extract(data_json,'$.updated_at') AS updated_at FROM analytics_hourly_rollups WHERE metric='usage';
CREATE VIEW analytics_leases AS SELECT organization_id,project_id,
json_extract(data_json,'$.lease_id') AS lease_id,json_extract(data_json,'$.issue_id') AS issue_id,
json_extract(data_json,'$.acquired_at') AS acquired_at,json_extract(data_json,'$.released_at') AS released_at,
json_extract(data_json,'$.expires_at') AS expires_at FROM analytics_hourly_rollups WHERE metric='lease';
CREATE INDEX analytics_event_item ON analytics_hourly_rollups(organization_id,project_id,json_extract(data_json,'$.work_item_id'),json_extract(data_json,'$.sequence')) WHERE metric='event';
CREATE INDEX analytics_event_window ON analytics_hourly_rollups(organization_id,project_id,json_extract(data_json,'$.type'),julianday(json_extract(data_json,'$.recorded_at'))) WHERE metric='event';
CREATE UNIQUE INDEX analytics_snapshot_identity ON analytics_hourly_rollups(organization_id,project_id,metric,dimension) WHERE metric IN ('issue','attempt','usage','lease');
CREATE INDEX analytics_attempt_window ON analytics_hourly_rollups(organization_id,project_id,julianday(json_extract(data_json,'$.started_at')),julianday(json_extract(data_json,'$.updated_at'))) WHERE metric='attempt';
CREATE INDEX analytics_usage_window ON analytics_hourly_rollups(organization_id,json_extract(data_json,'$.period'),project_id) WHERE metric='usage';
CREATE INDEX analytics_residence_hour ON analytics_hourly_rollups(organization_id,project_id,dimension,hour DESC) WHERE metric='residence_hour';
CREATE INDEX analytics_residence_edge ON analytics_hourly_rollups(organization_id,project_id,json_extract(data_json,'$.ID'),hour,json_extract(data_json,'$.AtNS') DESC,value DESC) WHERE metric='residence_edge';
CREATE INDEX analytics_skip_window ON analytics_hourly_rollups(organization_id,project_id,json_extract(data_json,'$.AtNS'),dimension) WHERE metric='skip_sample';
CREATE INDEX analytics_skip_order ON analytics_hourly_rollups(organization_id,project_id,json_extract(data_json,'$.At'),dimension) WHERE metric='skip_sample';
CREATE INDEX analytics_capacity_window ON analytics_hourly_rollups(organization_id,project_id,json_extract(data_json,'$.AtNS'),dimension) WHERE metric='capacity_claim';
CREATE INDEX analytics_residence_identity ON analytics_hourly_rollups(organization_id,project_id,json_extract(data_json,'$.ID'),metric) WHERE metric IN ('residence_hour','residence_edge','completion','residence_merge','merge_entry');
CREATE INDEX analytics_completion ON analytics_hourly_rollups(organization_id,project_id,json_extract(data_json,'$.ID'),json_extract(data_json,'$.AtNS')) WHERE metric='completion';
-- +goose StatementBegin
CREATE TRIGGER analytics_event_counts AFTER INSERT ON analytics_hourly_rollups WHEN NEW.metric='event'
BEGIN
 INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json)
 SELECT NEW.organization_id,NEW.project_id,NEW.hour,'ready','',1,'{}'
 WHERE json_extract(NEW.data_json,'$.type')='scheduler.decision' AND json_extract(NEW.data_json,'$.outcome')='ready'
 ON CONFLICT(organization_id,project_id,metric,hour,dimension) DO UPDATE SET value=value+1;
 INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json)
 SELECT NEW.organization_id,NEW.project_id,NEW.hour,'merge_entered','',1,'{}'
 WHERE json_extract(NEW.data_json,'$.type')='workflow.transitioned' AND json_extract(NEW.data_json,'$.to_state')='Merging'
 ON CONFLICT(organization_id,project_id,metric,hour,dimension) DO UPDATE SET value=value+1;
 INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json)
 SELECT NEW.organization_id,NEW.project_id,NEW.hour,'skipped',json_array(coalesce(json_extract(NEW.data_json,'$.source'),''),coalesce(json_extract(NEW.data_json,'$.reason'),'')),1,
 json_object('source',coalesce(json_extract(NEW.data_json,'$.source'),''),'reason',coalesce(json_extract(NEW.data_json,'$.reason'),''),'last_at',json_extract(NEW.data_json,'$.recorded_at'),'last_ns',(unixepoch(json_extract(NEW.data_json,'$.recorded_at'))*1000000000+CASE WHEN substr(json_extract(NEW.data_json,'$.recorded_at'),20,1)='.' THEN CAST(substr(rtrim(substr(json_extract(NEW.data_json,'$.recorded_at'),21),'Z')||'000000000',1,9) AS INTEGER) ELSE 0 END))
 WHERE json_extract(NEW.data_json,'$.type')='scheduler.decision' AND json_extract(NEW.data_json,'$.outcome')='skipped'
 ON CONFLICT(organization_id,project_id,metric,hour,dimension) DO UPDATE SET value=value+1,data_json=CASE WHEN json_extract(excluded.data_json,'$.last_ns')>json_extract(data_json,'$.last_ns') THEN excluded.data_json ELSE data_json END;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER analytics_usage_inserted AFTER INSERT ON attempt_usage
BEGIN
 INSERT INTO analytics_rollup_dirty SELECT work_item_id FROM native_attempts WHERE id=NEW.attempt_id AND organization_id=NEW.organization_id AND project_id=NEW.project_id ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER analytics_usage_updated AFTER UPDATE ON attempt_usage
BEGIN
 INSERT INTO analytics_rollup_dirty SELECT work_item_id FROM native_attempts WHERE id=NEW.attempt_id AND organization_id=NEW.organization_id AND project_id=NEW.project_id ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER analytics_events_inserted AFTER INSERT ON collaboration_events
BEGIN
 INSERT INTO analytics_rollup_dirty SELECT NEW.work_item_id WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER analytics_issues_inserted AFTER INSERT ON issues
BEGIN
 INSERT INTO analytics_rollup_dirty SELECT NEW.native_id WHERE NEW.native_id IS NOT NULL ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER analytics_issues_updated AFTER UPDATE ON issues
BEGIN
 INSERT INTO analytics_rollup_dirty SELECT NEW.native_id WHERE NEW.native_id IS NOT NULL ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER analytics_issues_deleted AFTER DELETE ON issues
BEGIN
 INSERT INTO analytics_rollup_dirty SELECT OLD.native_id WHERE OLD.native_id IS NOT NULL ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER analytics_attempts_inserted AFTER INSERT ON native_attempts
BEGIN
 INSERT INTO analytics_rollup_dirty SELECT NEW.work_item_id WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER analytics_attempts_updated AFTER UPDATE ON native_attempts
BEGIN
 INSERT INTO analytics_rollup_dirty SELECT NEW.work_item_id WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER analytics_leases_inserted AFTER INSERT ON leases
BEGIN
 INSERT INTO analytics_rollup_dirty SELECT native_id FROM issues WHERE id=NEW.issue_id AND native_id IS NOT NULL ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER analytics_leases_updated AFTER UPDATE ON leases
BEGIN
 INSERT INTO analytics_rollup_dirty SELECT native_id FROM issues WHERE id=NEW.issue_id AND native_id IS NOT NULL ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd
-- +goose Down
DROP TRIGGER analytics_leases_updated;
DROP TRIGGER analytics_leases_inserted;
DROP TRIGGER analytics_attempts_updated;
DROP TRIGGER analytics_attempts_inserted;
DROP TRIGGER analytics_issues_deleted;
DROP TRIGGER analytics_issues_updated;
DROP TRIGGER analytics_issues_inserted;
DROP TRIGGER analytics_events_inserted;
DROP TRIGGER analytics_usage_updated;
DROP TRIGGER analytics_usage_inserted;
DROP TRIGGER analytics_event_counts;
DROP VIEW analytics_issues;
DROP VIEW analytics_native_attempts;
DROP VIEW analytics_attempt_usage;
DROP VIEW analytics_leases;
DROP INDEX leases_issue_idx;
DROP TABLE analytics_rollup_dirty;
DROP TABLE analytics_rollup_backfill;
DROP TABLE analytics_rollup_positions;
DROP TABLE analytics_hourly_rollups;

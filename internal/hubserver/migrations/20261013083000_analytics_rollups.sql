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
 PRIMARY KEY(organization_id,project_id,work_item_id)
);
CREATE TABLE analytics_rollup_status (
 organization_id TEXT NOT NULL,
 project_id TEXT NOT NULL,
 available INTEGER NOT NULL,
 PRIMARY KEY(organization_id,project_id)
);
CREATE VIEW analytics_source_issues AS SELECT organization_id,project_id,unixepoch(created_at)/3600*3600 AS hour,'issue' AS metric,native_id AS dimension,1 AS value,json_object('id',id,'native_id',native_id,'title',title,'created_at',created_at,'updated_at',updated_at,'workflow_state_id',workflow_state_id) AS data_json FROM issues WHERE organization_id IS NOT NULL AND project_id IS NOT NULL AND native_id IS NOT NULL;
CREATE VIEW analytics_issues AS SELECT organization_id,project_id,json_extract(data_json,'$.id') AS id,json_extract(data_json,'$.native_id') AS native_id,json_extract(data_json,'$.title') AS title,json_extract(data_json,'$.created_at') AS created_at,json_extract(data_json,'$.updated_at') AS updated_at,json_extract(data_json,'$.workflow_state_id') AS workflow_state_id FROM analytics_hourly_rollups WHERE metric='issue';
CREATE VIEW analytics_source_native_attempts AS SELECT organization_id,project_id,unixepoch(started_at)/3600*3600 AS hour,'attempt' AS metric,id AS dimension,1 AS value,json_object('id',id,'work_item_id',work_item_id,'lease_id',lease_id,'status',status,'started_at',started_at,'updated_at',updated_at,'data_json',data_json) AS data_json,work_item_id FROM native_attempts WHERE organization_id IS NOT NULL AND project_id IS NOT NULL;
CREATE VIEW analytics_native_attempts AS SELECT organization_id,project_id,json_extract(data_json,'$.id') AS id,json_extract(data_json,'$.work_item_id') AS work_item_id,json_extract(data_json,'$.lease_id') AS lease_id,json_extract(data_json,'$.status') AS status,json_extract(data_json,'$.started_at') AS started_at,json_extract(data_json,'$.updated_at') AS updated_at,json_extract(data_json,'$.data_json') AS data_json FROM analytics_hourly_rollups WHERE metric='attempt';
CREATE VIEW analytics_source_attempt_usage AS SELECT organization_id,project_id,unixepoch(period)/3600*3600 AS hour,'usage' AS metric,json_array(attempt_id,period,provider,model,currency) AS dimension,cost_estimate AS value,json_object('attempt_id',attempt_id,'period',period,'provider',provider,'model',model,'input',input,'cached_input',cached_input,'output',output,'cost_estimate',cost_estimate,'currency',currency,'updated_at',updated_at) AS data_json,attempt_id FROM attempt_usage WHERE organization_id IS NOT NULL AND project_id IS NOT NULL;
CREATE VIEW analytics_attempt_usage AS SELECT organization_id,project_id,json_extract(data_json,'$.attempt_id') AS attempt_id,json_extract(data_json,'$.period') AS period,json_extract(data_json,'$.provider') AS provider,json_extract(data_json,'$.model') AS model,json_extract(data_json,'$.input') AS input,json_extract(data_json,'$.cached_input') AS cached_input,json_extract(data_json,'$.output') AS output,value AS cost_estimate,json_extract(data_json,'$.currency') AS currency,json_extract(data_json,'$.updated_at') AS updated_at FROM analytics_hourly_rollups WHERE metric='usage';
CREATE VIEW analytics_source_events AS SELECT e.organization_id,e.project_id,unixepoch(e.recorded_at)/3600*3600 AS hour,'event' AS metric,e.id AS dimension,1 AS value,
json_object('id',e.id,'work_item_id',e.work_item_id,'sequence',e.sequence,'type',e.type,'recorded_at',e.recorded_at,'data_json',e.data_json,
'prior_data',CASE WHEN e.type='scheduler.decision' AND json_extract(e.data_json,'$.decision.source')='native_claim' AND json_extract(e.data_json,'$.decision.outcome')='claimed' THEN (SELECT p.data_json FROM collaboration_events p WHERE p.organization_id=e.organization_id AND p.project_id=e.project_id AND p.work_item_id=e.work_item_id AND p.sequence=e.sequence-1 LIMIT 1) END,
'prior_type',CASE WHEN e.type='scheduler.decision' AND json_extract(e.data_json,'$.decision.source')='native_claim' AND json_extract(e.data_json,'$.decision.outcome')='claimed' THEN (SELECT p.type FROM collaboration_events p WHERE p.organization_id=e.organization_id AND p.project_id=e.project_id AND p.work_item_id=e.work_item_id AND p.sequence=e.sequence-1 LIMIT 1) END,
'prior_at',CASE WHEN e.type='scheduler.decision' AND json_extract(e.data_json,'$.decision.source')='native_claim' AND json_extract(e.data_json,'$.decision.outcome')='claimed' THEN (SELECT p.recorded_at FROM collaboration_events p WHERE p.organization_id=e.organization_id AND p.project_id=e.project_id AND p.work_item_id=e.work_item_id AND p.sequence=e.sequence-1 LIMIT 1) END) AS data_json,e.work_item_id,e.sequence
FROM collaboration_events e WHERE e.type IN ('workflow.transitioned','scheduler.decision','issue.created');
CREATE VIEW analytics_collaboration_events AS SELECT organization_id,project_id,
json_extract(data_json,'$.id') AS id,json_extract(data_json,'$.work_item_id') AS work_item_id,
json_extract(data_json,'$.sequence') AS sequence,json_extract(data_json,'$.type') AS type,
json_extract(data_json,'$.recorded_at') AS recorded_at,json_extract(data_json,'$.data_json') AS data_json,
coalesce(json_extract(data_json,'$.prior_data'),'null') AS prior_data,
coalesce(json_extract(data_json,'$.prior_type'),'') AS prior_type,
coalesce(json_extract(data_json,'$.prior_at'),'') AS prior_at
FROM analytics_hourly_rollups WHERE metric='event';
CREATE VIEW analytics_source_leases AS SELECT i.organization_id,i.project_id,unixepoch(l.acquired_at)/3600*3600 AS hour,'lease' AS metric,l.lease_id AS dimension,1 AS value,
json_object('lease_id',l.lease_id,'issue_id',l.issue_id,'acquired_at',l.acquired_at,'released_at',l.released_at,'expires_at',l.expires_at) AS data_json,l.issue_id
FROM leases l JOIN issues i ON i.id=l.issue_id WHERE i.native_id IS NOT NULL;
CREATE VIEW analytics_leases AS SELECT organization_id,project_id,
json_extract(data_json,'$.lease_id') AS lease_id,json_extract(data_json,'$.issue_id') AS issue_id,
json_extract(data_json,'$.acquired_at') AS acquired_at,json_extract(data_json,'$.released_at') AS released_at,
json_extract(data_json,'$.expires_at') AS expires_at FROM analytics_hourly_rollups WHERE metric='lease';
CREATE INDEX analytics_event_item ON analytics_hourly_rollups(organization_id,project_id,json_extract(data_json,'$.work_item_id'),json_extract(data_json,'$.sequence')) WHERE metric='event';
CREATE INDEX analytics_event_window ON analytics_hourly_rollups(organization_id,project_id,json_extract(data_json,'$.type'),julianday(json_extract(data_json,'$.recorded_at'))) WHERE metric='event';
CREATE UNIQUE INDEX analytics_snapshot_identity ON analytics_hourly_rollups(organization_id,project_id,metric,dimension) WHERE metric IN ('issue','attempt','usage','lease');
CREATE INDEX analytics_attempt_window ON analytics_hourly_rollups(organization_id,project_id,julianday(json_extract(data_json,'$.started_at')),julianday(json_extract(data_json,'$.updated_at'))) WHERE metric='attempt';
CREATE INDEX analytics_usage_window ON analytics_hourly_rollups(organization_id,json_extract(data_json,'$.period'),project_id) WHERE metric='usage';
-- +goose StatementBegin
CREATE TRIGGER analytics_event_counts AFTER INSERT ON analytics_hourly_rollups WHEN NEW.metric='event'
BEGIN
 INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json)
 SELECT NEW.organization_id,NEW.project_id,NEW.hour,'ready','',1,'{}'
 WHERE json_extract(NEW.data_json,'$.type')='scheduler.decision' AND json_extract(json_extract(NEW.data_json,'$.data_json'),'$.decision.outcome')='ready'
 ON CONFLICT(organization_id,project_id,metric,hour,dimension) DO UPDATE SET value=value+1;
 INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json)
 SELECT NEW.organization_id,NEW.project_id,NEW.hour,'merge_entered','',1,'{}'
 WHERE json_extract(NEW.data_json,'$.type')='workflow.transitioned' AND json_extract(json_extract(NEW.data_json,'$.data_json'),'$.to_state')='Merging'
 ON CONFLICT(organization_id,project_id,metric,hour,dimension) DO UPDATE SET value=value+1;
 INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json)
 SELECT NEW.organization_id,NEW.project_id,NEW.hour,'skipped',json_array(coalesce(json_extract(json_extract(NEW.data_json,'$.data_json'),'$.decision.source'),''),coalesce(json_extract(json_extract(NEW.data_json,'$.data_json'),'$.decision.reason'),'')),1,
 json_object('source',coalesce(json_extract(json_extract(NEW.data_json,'$.data_json'),'$.decision.source'),''),'reason',coalesce(json_extract(json_extract(NEW.data_json,'$.data_json'),'$.decision.reason'),''),'last_at',json_extract(NEW.data_json,'$.recorded_at'),'last_ns',(unixepoch(json_extract(NEW.data_json,'$.recorded_at'))*1000000000+CASE WHEN substr(json_extract(NEW.data_json,'$.recorded_at'),20,1)='.' THEN CAST(substr(rtrim(substr(json_extract(NEW.data_json,'$.recorded_at'),21),'Z')||'000000000',1,9) AS INTEGER) ELSE 0 END))
 WHERE json_extract(NEW.data_json,'$.type')='scheduler.decision' AND json_extract(json_extract(NEW.data_json,'$.data_json'),'$.decision.outcome')='skipped'
 ON CONFLICT(organization_id,project_id,metric,hour,dimension) DO UPDATE SET value=value+1,data_json=CASE WHEN json_extract(excluded.data_json,'$.last_ns')>json_extract(data_json,'$.last_ns') THEN excluded.data_json ELSE data_json END;
END;
-- +goose StatementEnd
INSERT INTO analytics_hourly_rollups SELECT organization_id,project_id,hour,metric,dimension,value,data_json FROM analytics_source_issues;
INSERT INTO analytics_hourly_rollups SELECT organization_id,project_id,hour,metric,dimension,value,data_json FROM analytics_source_native_attempts;
INSERT INTO analytics_hourly_rollups SELECT organization_id,project_id,hour,metric,dimension,value,data_json FROM analytics_source_attempt_usage;
INSERT INTO analytics_hourly_rollups SELECT organization_id,project_id,hour,metric,dimension,value,data_json FROM analytics_source_events;
INSERT INTO analytics_hourly_rollups SELECT organization_id,project_id,hour,metric,dimension,value,data_json FROM analytics_source_leases;
INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json)
WITH RECURSIVE hours(organization_id,project_id,dimension,data_json,hour,start,finish) AS (
 SELECT organization_id,project_id,dimension,data_json,hour,(unixepoch(json_extract(data_json,'$.acquired_at'))*1000000000+CASE WHEN substr(json_extract(data_json,'$.acquired_at'),20,1)='.' THEN CAST(substr(rtrim(substr(json_extract(data_json,'$.acquired_at'),21),'Z')||'000000000',1,9) AS INTEGER) ELSE 0 END),min((unixepoch(coalesce(json_extract(data_json,'$.released_at'),json_extract(data_json,'$.expires_at')))*1000000000+CASE WHEN substr(coalesce(json_extract(data_json,'$.released_at'),json_extract(data_json,'$.expires_at')),20,1)='.' THEN CAST(substr(rtrim(substr(coalesce(json_extract(data_json,'$.released_at'),json_extract(data_json,'$.expires_at')),21),'Z')||'000000000',1,9) AS INTEGER) ELSE 0 END),(unixepoch(json_extract(data_json,'$.expires_at'))*1000000000+CASE WHEN substr(json_extract(data_json,'$.expires_at'),20,1)='.' THEN CAST(substr(rtrim(substr(json_extract(data_json,'$.expires_at'),21),'Z')||'000000000',1,9) AS INTEGER) ELSE 0 END)) FROM analytics_hourly_rollups WHERE metric='lease'
 UNION ALL SELECT organization_id,project_id,dimension,data_json,hour+3600,start,finish FROM hours WHERE (hour+3600)*1000000000<finish
)
SELECT organization_id,project_id,hour,'slots',dimension,max(0,min(finish,(hour+3600)*1000000000)-max(start,hour*1000000000))/1000000000.0/3600,data_json FROM hours WHERE finish>start;
INSERT INTO analytics_rollup_positions SELECT organization_id,project_id,native_id,event_sequence FROM issues WHERE native_id IS NOT NULL;
INSERT INTO analytics_rollup_status SELECT organization_id,id,1 FROM projects;
-- +goose Down
DROP TRIGGER analytics_event_counts;
DROP VIEW analytics_source_issues;
DROP VIEW analytics_source_native_attempts;
DROP VIEW analytics_source_attempt_usage;
DROP VIEW analytics_source_events;
DROP VIEW analytics_source_leases;
DROP VIEW analytics_issues;
DROP VIEW analytics_native_attempts;
DROP VIEW analytics_attempt_usage;
DROP VIEW analytics_collaboration_events;
DROP VIEW analytics_leases;
DROP TABLE analytics_rollup_status;
DROP TABLE analytics_rollup_positions;
DROP TABLE analytics_hourly_rollups;

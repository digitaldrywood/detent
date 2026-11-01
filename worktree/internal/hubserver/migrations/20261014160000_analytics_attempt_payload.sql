-- +goose Up
UPDATE analytics_hourly_rollups AS r SET data_json=json_set(json_remove(p.payload,'$.runtime.validation.output','$.runtime.landing.gate.output'),
 '$.id',p.id,'$.work_item_id',p.work_item_id,'$.lease_id',p.lease_id,'$.status',p.status,'$.started_at',p.started_at,'$.updated_at',p.updated_at,
 '$.failed_at',CASE WHEN p.status IN ('failed','interrupted') OR json_extract(p.payload,'$.runtime.landing.refusal_kind') IN ('conflict','base_protected') OR coalesce(json_extract(p.payload,'$.runtime.landing.gate_failed'),0)=1
 THEN CASE WHEN (json_extract(p.payload,'$.runtime.landing.refusal_kind') IN ('conflict','base_protected') AND coalesce(json_extract(p.payload,'$.runtime.landing.landed'),0)=0) OR coalesce(json_extract(p.payload,'$.runtime.landing.gate_failed'),0)=1
 THEN coalesce(nullif(json_extract(p.payload,'$.runtime.landing.observed_at'),'0001-01-01T00:00:00Z'),p.updated_at) ELSE p.updated_at END END)
FROM (SELECT rowid AS row,json_extract(data_json,'$.data_json') AS payload,json_extract(data_json,'$.id') AS id,json_extract(data_json,'$.work_item_id') AS work_item_id,
 json_extract(data_json,'$.lease_id') AS lease_id,json_extract(data_json,'$.status') AS status,json_extract(data_json,'$.started_at') AS started_at,json_extract(data_json,'$.updated_at') AS updated_at
 FROM analytics_hourly_rollups WHERE metric='attempt' AND json_type(data_json,'$.data_json') IS NOT NULL) AS p
WHERE r.rowid=p.row;
DROP VIEW analytics_native_attempts;
CREATE VIEW analytics_native_attempts AS SELECT organization_id,project_id,dimension AS id,json_work_item_id AS work_item_id,json_extract(data_json,'$.lease_id') AS lease_id,json_extract(data_json,'$.status') AS status,json_started_at AS started_at,json_updated_at AS updated_at,data_json FROM analytics_hourly_rollups WHERE metric='attempt';
CREATE INDEX analytics_attempt_item ON analytics_hourly_rollups(organization_id,project_id,json_work_item_id,dimension) WHERE metric='attempt';
CREATE INDEX analytics_attempt_started ON analytics_hourly_rollups(organization_id,project_id,json_started_at,dimension,json_updated_at) WHERE metric='attempt';
CREATE INDEX analytics_attempt_failed ON analytics_hourly_rollups(organization_id,project_id,julianday(json_extract(data_json,'$.failed_at')),dimension) WHERE metric='attempt' AND json_extract(data_json,'$.failed_at') IS NOT NULL;
CREATE INDEX analytics_issue_native ON analytics_hourly_rollups(organization_id,project_id,json_extract(data_json,'$.native_id')) WHERE metric='issue';
CREATE INDEX analytics_capacity_order ON analytics_hourly_rollups(organization_id,project_id,json_extract(data_json,'$.At'),dimension,json_extract(data_json,'$.AtNS')) WHERE metric='capacity_claim';
DROP INDEX analytics_usage_window;
CREATE INDEX analytics_usage_window ON analytics_hourly_rollups(organization_id,project_id,json_period,json_extract(data_json,'$.provider'),json_extract(data_json,'$.model'),json_attempt_id) WHERE metric='usage';
DROP INDEX analytics_completion;
CREATE INDEX analytics_completion ON analytics_hourly_rollups(organization_id,project_id,json_extract(data_json,'$.ID'),json_extract(data_json,'$.AtNS'),value) WHERE metric='completion';

-- +goose Down
DROP INDEX analytics_completion;
CREATE INDEX analytics_completion ON analytics_hourly_rollups(organization_id,project_id,json_extract(data_json,'$.ID'),json_extract(data_json,'$.AtNS')) WHERE metric='completion';
DROP INDEX analytics_usage_window;
CREATE INDEX analytics_usage_window ON analytics_hourly_rollups(organization_id,project_id,json_period) WHERE metric='usage';
DROP INDEX analytics_capacity_order;
DROP INDEX analytics_issue_native;
DROP INDEX analytics_attempt_failed;
DROP INDEX analytics_attempt_started;
DROP INDEX analytics_attempt_item;
DROP VIEW analytics_native_attempts;
CREATE VIEW analytics_native_attempts AS SELECT organization_id,project_id,dimension AS id,json_work_item_id AS work_item_id,json_extract(data_json,'$.lease_id') AS lease_id,json_extract(data_json,'$.status') AS status,json_started_at AS started_at,json_updated_at AS updated_at,json_extract(data_json,'$.data_json') AS data_json FROM analytics_hourly_rollups WHERE metric='attempt';
UPDATE analytics_hourly_rollups SET data_json=json_object('id',json_extract(data_json,'$.id'),'work_item_id',json_extract(data_json,'$.work_item_id'),'lease_id',json_extract(data_json,'$.lease_id'),
 'status',json_extract(data_json,'$.status'),'started_at',json_extract(data_json,'$.started_at'),'updated_at',json_extract(data_json,'$.updated_at'),
 'data_json',json_remove(data_json,'$.id','$.work_item_id','$.status','$.started_at','$.updated_at','$.failed_at'))
WHERE metric='attempt' AND json_type(data_json,'$.data_json') IS NULL;

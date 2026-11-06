-- +goose Up
ALTER TABLE native_attempts ADD COLUMN role TEXT GENERATED ALWAYS AS (coalesce(json_extract(data_json,'$.runtime.identity.role'),'')) VIRTUAL;
ALTER TABLE native_attempts ADD COLUMN activity_started_at REAL GENERATED ALWAYS AS (julianday(started_at)) VIRTUAL;
ALTER TABLE native_attempts ADD COLUMN activity_updated_at REAL GENERATED ALWAYS AS (julianday(updated_at)) VIRTUAL;
ALTER TABLE native_attempts ADD COLUMN finished_at TEXT GENERATED ALWAYS AS (CASE WHEN status!='running' THEN updated_at END) VIRTUAL;
CREATE INDEX native_attempts_activity_running ON native_attempts(organization_id,project_id,status,activity_started_at,id);
CREATE INDEX native_attempts_activity_finished ON native_attempts(organization_id,project_id,activity_updated_at DESC,id DESC) WHERE status!='running';
CREATE INDEX change_requests_activity_latest ON change_requests(organization_id,project_id,work_item_id,julianday(json_extract(record_json,'$.created_at')) DESC,id DESC);

CREATE TABLE activity_attempt_durations (
 attempt_id TEXT PRIMARY KEY REFERENCES native_attempts(id) ON DELETE CASCADE,
 organization_id TEXT NOT NULL,
 project_id TEXT NOT NULL,
 status TEXT NOT NULL,
 role TEXT NOT NULL,
 started_at REAL,
 updated_at REAL,
 seconds REAL NOT NULL,
 partial INTEGER NOT NULL
);
CREATE INDEX activity_attempt_durations_window ON activity_attempt_durations(organization_id,project_id,updated_at,started_at,attempt_id);
CREATE VIEW activity_attempt_duration_projection AS
SELECT id AS attempt_id,organization_id,project_id,status,role,
 activity_started_at AS started_at,activity_updated_at AS updated_at,
 CASE WHEN status IN ('succeeded','failed') AND NOT (EXISTS (SELECT 1 FROM (SELECT type,
 json_extract(CASE WHEN type='object' THEN value ELSE '{}' END,'$.started_at') AS started,
 json_extract(CASE WHEN type='object' THEN value ELSE '{}' END,'$.finished_at') AS finished
 FROM json_each(CASE WHEN json_type(data_json,'$.runtime.phases')='array' THEN json_extract(data_json,'$.runtime.phases') ELSE '[]' END)) WHERE type NOT IN ('object','null') OR (started IS NOT NULL AND unixepoch(started) IS NULL) OR (finished IS NOT NULL AND unixepoch(finished) IS NULL))) THEN
 coalesce((SELECT sum(max(0,unixepoch(substr(finished,1,19)||CASE WHEN substr(finished,-1)='Z' THEN 'Z' ELSE substr(finished,-6) END)-unixepoch(substr(coalesce(started,'0001-01-01T00:00:00Z'),1,19)||CASE WHEN substr(coalesce(started,'0001-01-01T00:00:00Z'),-1)='Z' THEN 'Z' ELSE substr(coalesce(started,'0001-01-01T00:00:00Z'),-6) END)+(CASE WHEN substr(finished,20,1)='.' THEN CAST('0.'||substr(finished,21,length(finished)-20-CASE WHEN substr(finished,-1)='Z' THEN 1 ELSE 6 END) AS REAL) ELSE 0 END)-(CASE WHEN substr(started,20,1)='.' THEN CAST('0.'||substr(started,21,length(started)-20-CASE WHEN substr(started,-1)='Z' THEN 1 ELSE 6 END) AS REAL) ELSE 0 END)))
 FROM (SELECT type,
 json_extract(CASE WHEN type='object' THEN value ELSE '{}' END,'$.started_at') AS started,
 json_extract(CASE WHEN type='object' THEN value ELSE '{}' END,'$.finished_at') AS finished
 FROM json_each(CASE WHEN json_type(data_json,'$.runtime.phases')='array' THEN json_extract(data_json,'$.runtime.phases') ELSE '[]' END)) WHERE finished IS NOT NULL AND finished!='0001-01-01T00:00:00Z'),0)
 ELSE 0 END AS seconds,
 CASE WHEN json_type(data_json,'$.runtime.phases')!='array'
 OR coalesce(json_array_length(CASE WHEN json_type(data_json,'$.runtime.phases')='array' THEN json_extract(data_json,'$.runtime.phases') ELSE '[]' END),0)=0
 OR coalesce(json_extract(data_json,'$.runtime.phases_dropped'),0)>0
 OR EXISTS (SELECT 1 FROM (SELECT type,
 json_extract(CASE WHEN type='object' THEN value ELSE '{}' END,'$.started_at') AS started,
 json_extract(CASE WHEN type='object' THEN value ELSE '{}' END,'$.finished_at') AS finished
 FROM json_each(CASE WHEN json_type(data_json,'$.runtime.phases')='array' THEN json_extract(data_json,'$.runtime.phases') ELSE '[]' END)) WHERE type NOT IN ('object','null') OR (started IS NOT NULL AND unixepoch(started) IS NULL) OR (finished IS NOT NULL AND unixepoch(finished) IS NULL)) THEN 1 ELSE 0 END AS partial
FROM native_attempts;
INSERT INTO activity_attempt_durations SELECT * FROM activity_attempt_duration_projection;

-- +goose StatementBegin
CREATE TRIGGER activity_attempt_durations_insert AFTER INSERT ON native_attempts
BEGIN
 INSERT INTO activity_attempt_durations SELECT * FROM activity_attempt_duration_projection WHERE attempt_id=NEW.id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER activity_attempt_durations_update AFTER UPDATE OF data_json,status,started_at,updated_at,organization_id,project_id ON native_attempts
BEGIN
 INSERT INTO activity_attempt_durations SELECT * FROM activity_attempt_duration_projection WHERE attempt_id=NEW.id
 ON CONFLICT(attempt_id) DO UPDATE SET organization_id=excluded.organization_id,project_id=excluded.project_id,status=excluded.status,role=excluded.role,started_at=excluded.started_at,updated_at=excluded.updated_at,seconds=excluded.seconds,partial=excluded.partial;
END;
-- +goose StatementEnd

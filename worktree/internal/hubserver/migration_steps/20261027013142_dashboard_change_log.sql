-- +goose Up
ALTER TABLE native_board_deltas ADD COLUMN topic TEXT NOT NULL DEFAULT 'board' CHECK(topic IN ('board','work_item','lane','activity','human_action','workspace','health','action_run'));
ALTER TABLE native_board_deltas ADD COLUMN entity_id TEXT NOT NULL DEFAULT '';
ALTER TABLE native_board_deltas ADD COLUMN entity_revision INTEGER NOT NULL DEFAULT 0;
ALTER TABLE native_board_deltas ADD COLUMN kind TEXT NOT NULL DEFAULT 'upsert' CHECK(kind IN ('upsert','delete'));
ALTER TABLE native_board_deltas ADD COLUMN status TEXT NOT NULL DEFAULT '';
ALTER TABLE native_board_deltas ADD COLUMN related_id TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX native_board_source_revision ON native_board_deltas(organization_id,project_id,topic,entity_id,entity_revision) WHERE topic<>'board';
CREATE INDEX native_board_topic_replay ON native_board_deltas(organization_id,project_id,topic,sequence);
ALTER TABLE native_board_sequences ADD COLUMN retained_count INTEGER NOT NULL DEFAULT 0 CHECK(retained_count>=0);
UPDATE native_board_sequences SET retained_count=(SELECT count(*) FROM native_board_deltas d WHERE d.organization_id=native_board_sequences.organization_id AND d.project_id=native_board_sequences.project_id);
CREATE INDEX native_board_retention_due ON native_board_sequences(organization_id,project_id) WHERE retained_count>2048;
ALTER TABLE health_findings ADD COLUMN dashboard_revision INTEGER NOT NULL DEFAULT 1;
CREATE TABLE health_finding_projects (
 finding_id TEXT NOT NULL REFERENCES health_findings(id) ON DELETE CASCADE,
 organization_id TEXT NOT NULL, project_id TEXT NOT NULL,
 PRIMARY KEY(finding_id,project_id)
);
CREATE INDEX health_finding_projects_scope ON health_finding_projects(organization_id,project_id,finding_id);

-- +goose StatementBegin
CREATE TRIGGER dashboard_head AFTER INSERT ON native_board_deltas
BEGIN
 INSERT INTO native_board_sequences(organization_id,project_id,sequence,retained_count) VALUES(NEW.organization_id,NEW.project_id,NEW.sequence,1)
 ON CONFLICT(organization_id,project_id) DO UPDATE SET sequence=excluded.sequence,retained_count=native_board_sequences.retained_count+1;
 UPDATE native_board_deltas SET entity_revision=NEW.sequence WHERE sequence=NEW.sequence AND NEW.topic IN ('board','lane');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_work_item_insert AFTER INSERT ON issues
WHEN NEW.organization_id IS NOT NULL AND NEW.project_id IS NOT NULL
BEGIN
 INSERT OR IGNORE INTO native_board_deltas(organization_id,project_id,topic,entity_id,entity_revision,kind,status,related_id) SELECT NEW.organization_id,NEW.project_id,'work_item',NEW.native_id,NEW.revision,'upsert',COALESCE((SELECT detent_state FROM workflow_states WHERE id=NEW.workflow_state_id),''),'';
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_work_item_update AFTER UPDATE ON issues
WHEN NEW.organization_id IS NOT NULL AND NEW.project_id IS NOT NULL AND NEW.revision<>OLD.revision
BEGIN
 INSERT OR IGNORE INTO native_board_deltas(organization_id,project_id,topic,entity_id,entity_revision,kind,status,related_id) SELECT NEW.organization_id,NEW.project_id,'work_item',NEW.native_id,NEW.revision,'upsert',COALESCE((SELECT detent_state FROM workflow_states WHERE id=NEW.workflow_state_id),''),'';
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_work_item_delete AFTER DELETE ON issues
WHEN OLD.organization_id IS NOT NULL AND OLD.project_id IS NOT NULL
BEGIN
 INSERT OR IGNORE INTO native_board_deltas(organization_id,project_id,topic,entity_id,entity_revision,kind,status,related_id) SELECT OLD.organization_id,OLD.project_id,'work_item',OLD.native_id,OLD.revision+1,'delete',COALESCE((SELECT detent_state FROM workflow_states WHERE id=OLD.workflow_state_id),''),'';
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_workspace_insert AFTER INSERT ON workspace_sessions
BEGIN
 INSERT OR IGNORE INTO native_board_deltas(organization_id,project_id,topic,entity_id,entity_revision,kind,status,related_id) SELECT NEW.organization_id,NEW.project_id,'workspace',NEW.id,NEW.revision,'upsert',NEW.state,NEW.subject_work_item_id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_workspace_update AFTER UPDATE ON workspace_sessions
WHEN NEW.revision<>OLD.revision
BEGIN
 INSERT OR IGNORE INTO native_board_deltas(organization_id,project_id,topic,entity_id,entity_revision,kind,status,related_id) SELECT NEW.organization_id,NEW.project_id,'workspace',NEW.id,NEW.revision,'upsert',NEW.state,NEW.subject_work_item_id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_workspace_delete AFTER DELETE ON workspace_sessions
BEGIN
 INSERT OR IGNORE INTO native_board_deltas(organization_id,project_id,topic,entity_id,entity_revision,kind,status,related_id) SELECT OLD.organization_id,OLD.project_id,'workspace',OLD.id,OLD.revision+1,'delete',OLD.state,OLD.subject_work_item_id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_action_run_insert AFTER INSERT ON project_action_runs
BEGIN
 INSERT OR IGNORE INTO native_board_deltas(organization_id,project_id,topic,entity_id,entity_revision,kind,status,related_id) SELECT NEW.organization_id,NEW.project_id,'action_run',NEW.id,NEW.revision,'upsert',NEW.status,NEW.workspace_id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_action_run_update AFTER UPDATE ON project_action_runs
WHEN NEW.revision<>OLD.revision
BEGIN
 INSERT OR IGNORE INTO native_board_deltas(organization_id,project_id,topic,entity_id,entity_revision,kind,status,related_id) SELECT NEW.organization_id,NEW.project_id,'action_run',NEW.id,NEW.revision,'upsert',NEW.status,NEW.workspace_id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_action_run_delete AFTER DELETE ON project_action_runs
BEGIN
 INSERT OR IGNORE INTO native_board_deltas(organization_id,project_id,topic,entity_id,entity_revision,kind,status,related_id) SELECT OLD.organization_id,OLD.project_id,'action_run',OLD.id,OLD.revision+1,'delete',OLD.status,OLD.workspace_id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_human_action_insert AFTER INSERT ON issues
WHEN NEW.organization_id IS NOT NULL AND NEW.project_id IS NOT NULL
BEGIN
 INSERT OR IGNORE INTO native_board_deltas(organization_id,project_id,topic,entity_id,entity_revision,kind,status,related_id) SELECT NEW.organization_id,NEW.project_id,'human_action',NEW.native_id,NEW.revision,'upsert','','';
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_human_action_update AFTER UPDATE ON issues
WHEN NEW.organization_id IS NOT NULL AND NEW.project_id IS NOT NULL AND NEW.revision<>OLD.revision
BEGIN
 INSERT OR IGNORE INTO native_board_deltas(organization_id,project_id,topic,entity_id,entity_revision,kind,status,related_id) SELECT NEW.organization_id,NEW.project_id,'human_action',NEW.native_id,NEW.revision,'upsert','','';
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_human_action_delete AFTER DELETE ON issues
WHEN OLD.organization_id IS NOT NULL AND OLD.project_id IS NOT NULL
BEGIN
 INSERT OR IGNORE INTO native_board_deltas(organization_id,project_id,topic,entity_id,entity_revision,kind,status,related_id) SELECT OLD.organization_id,OLD.project_id,'human_action',OLD.native_id,OLD.revision+1,'delete','','';
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_human_attempt AFTER UPDATE OF disposition_status,disposition_human_action ON native_attempts
WHEN NEW.disposition_status<>OLD.disposition_status OR NEW.disposition_human_action<>OLD.disposition_human_action
BEGIN
 INSERT OR IGNORE INTO native_board_deltas(organization_id,project_id,topic,entity_id,entity_revision,kind,status,related_id) SELECT NEW.organization_id,NEW.project_id,'human_action',NEW.id,NEW.sequence,'upsert',NEW.disposition_status,NEW.work_item_id ;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_activity AFTER INSERT ON collaboration_events
BEGIN
 INSERT OR IGNORE INTO native_board_deltas(organization_id,project_id,topic,entity_id,entity_revision,kind,status,related_id) SELECT NEW.organization_id,NEW.project_id,'activity',NEW.id,NEW.sequence,'upsert',NEW.type,NEW.work_item_id ;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_lane_insert AFTER INSERT ON workflow_states
WHEN NEW.project_id IS NOT NULL
BEGIN
 INSERT INTO native_board_deltas(organization_id,project_id,topic,entity_id,entity_revision,kind,status)
 SELECT p.organization_id,NEW.project_id,'lane',NEW.source_name,0,'upsert',NEW.detent_state FROM projects p WHERE p.id=NEW.project_id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_lane_update AFTER UPDATE ON workflow_states
WHEN NEW.project_id IS NOT NULL
BEGIN
 INSERT INTO native_board_deltas(organization_id,project_id,topic,entity_id,entity_revision,kind,status)
 SELECT p.organization_id,NEW.project_id,'lane',NEW.source_name,0,'upsert',NEW.detent_state FROM projects p WHERE p.id=NEW.project_id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_lane_delete AFTER DELETE ON workflow_states
WHEN OLD.project_id IS NOT NULL
BEGIN
 INSERT INTO native_board_deltas(organization_id,project_id,topic,entity_id,entity_revision,kind,status)
 SELECT p.organization_id,OLD.project_id,'lane',OLD.source_name,0,'delete',OLD.detent_state FROM projects p WHERE p.id=OLD.project_id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_health_update AFTER UPDATE ON health_findings
WHEN NEW.dashboard_revision=OLD.dashboard_revision
BEGIN
 UPDATE health_findings SET dashboard_revision=dashboard_revision+1 WHERE id=NEW.id;
 INSERT OR IGNORE INTO native_board_deltas(organization_id,project_id,topic,entity_id,entity_revision,kind,status,related_id) SELECT NEW.organization_id,hp.project_id,'health',NEW.id,NEW.dashboard_revision+1,'upsert',CASE WHEN NEW.resolved_at IS NULL THEN NEW.severity ELSE 'resolved' END,'' FROM health_finding_projects hp WHERE hp.finding_id=NEW.id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_health_delete BEFORE DELETE ON health_findings
BEGIN
 INSERT OR IGNORE INTO native_board_deltas(organization_id,project_id,topic,entity_id,entity_revision,kind,status,related_id) SELECT OLD.organization_id,hp.project_id,'health',OLD.id,OLD.dashboard_revision+1,'delete',OLD.severity,'' FROM health_finding_projects hp WHERE hp.finding_id=OLD.id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_health_project_delete BEFORE DELETE ON health_finding_projects
BEGIN
 INSERT OR IGNORE INTO native_board_deltas(organization_id,project_id,topic,entity_id,entity_revision,kind,status)
 SELECT OLD.organization_id,OLD.project_id,'health',OLD.finding_id,f.dashboard_revision+1,'delete',f.severity FROM health_findings f WHERE f.id=OLD.finding_id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER dashboard_human_attempt_delete AFTER DELETE ON native_attempts
WHEN OLD.disposition_status<>'' OR OLD.disposition_human_action<>0
BEGIN
 INSERT OR IGNORE INTO native_board_deltas(organization_id,project_id,topic,entity_id,entity_revision,kind,status,related_id)
 SELECT OLD.organization_id,OLD.project_id,'human_action',OLD.id,OLD.sequence+1,'delete',OLD.disposition_status,OLD.work_item_id;
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER dashboard_human_attempt_delete;
DROP TRIGGER dashboard_health_project_delete;
DROP TRIGGER dashboard_health_delete;
DROP TRIGGER dashboard_health_update;
DROP TRIGGER dashboard_lane_delete;
DROP TRIGGER dashboard_lane_update;
DROP TRIGGER dashboard_lane_insert;
DROP TRIGGER dashboard_activity;
DROP TRIGGER dashboard_human_attempt;
DROP TRIGGER dashboard_human_action_delete;
DROP TRIGGER dashboard_human_action_update;
DROP TRIGGER dashboard_human_action_insert;
DROP TRIGGER dashboard_action_run_delete;
DROP TRIGGER dashboard_action_run_update;
DROP TRIGGER dashboard_action_run_insert;
DROP TRIGGER dashboard_workspace_delete;
DROP TRIGGER dashboard_workspace_update;
DROP TRIGGER dashboard_workspace_insert;
DROP TRIGGER dashboard_work_item_delete;
DROP TRIGGER dashboard_work_item_update;
DROP TRIGGER dashboard_work_item_insert;
DROP TRIGGER dashboard_head;
DROP TABLE health_finding_projects;
DROP INDEX native_board_retention_due;
ALTER TABLE native_board_sequences DROP COLUMN retained_count;
ALTER TABLE health_findings DROP COLUMN dashboard_revision;
DROP INDEX native_board_topic_replay;
DROP INDEX native_board_source_revision;
ALTER TABLE native_board_deltas DROP COLUMN related_id;
ALTER TABLE native_board_deltas DROP COLUMN status;
ALTER TABLE native_board_deltas DROP COLUMN kind;
ALTER TABLE native_board_deltas DROP COLUMN entity_revision;
ALTER TABLE native_board_deltas DROP COLUMN entity_id;
ALTER TABLE native_board_deltas DROP COLUMN topic;

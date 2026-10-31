-- +goose Up
ALTER TABLE collaboration_events ADD COLUMN payload_version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE collaboration_events ADD COLUMN payload_attachment_id TEXT NOT NULL DEFAULT '';
ALTER TABLE collaboration_events ADD COLUMN payload_entry TEXT NOT NULL DEFAULT '';
ALTER TABLE collaboration_events ADD COLUMN payload_object_key TEXT NOT NULL DEFAULT '';
ALTER TABLE collaboration_events ADD COLUMN payload_sha256 TEXT NOT NULL DEFAULT '';
ALTER TABLE collaboration_events ADD COLUMN payload_bytes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE collaboration_events ADD COLUMN payload_content_bytes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE collaboration_events ADD COLUMN scheduler_change_id TEXT NOT NULL DEFAULT '';
ALTER TABLE collaboration_events ADD COLUMN scheduler_version_id TEXT NOT NULL DEFAULT '';
ALTER TABLE collaboration_events ADD COLUMN scheduler_head_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE collaboration_events ADD COLUMN scheduler_change_present INTEGER NOT NULL DEFAULT 0;
ALTER TABLE collaboration_events ADD COLUMN run_outcome TEXT NOT NULL DEFAULT '';
ALTER TABLE collaboration_events ADD COLUMN run_machine_id TEXT NOT NULL DEFAULT '';
ALTER TABLE collaboration_events ADD COLUMN run_session_id TEXT NOT NULL DEFAULT '';
ALTER TABLE collaboration_events ADD COLUMN run_runner_id TEXT NOT NULL DEFAULT '';
DROP INDEX storage_discovery_runs;
CREATE INDEX storage_discovery_runs ON collaboration_events(id) WHERE conversion_state<>1 AND payload_version=0 AND type IN ('run.started','run.checkpointed','run.observed','run.finished','scheduler.decision');
ALTER TABLE collaboration_events ADD COLUMN push_effect_id TEXT NOT NULL DEFAULT '';
ALTER TABLE collaboration_events ADD COLUMN push_checkpoint_head_sha TEXT NOT NULL DEFAULT '';
CREATE INDEX storage_conversion_pending_scopes ON storage_conversion_progress(updated_at,organization_id,project_id) WHERE (complete=0 OR format_version<>1) AND family IN ('runs','attempts','analytics','conversation_events','messages','versions','evidence','sources','diff_files');
DROP TRIGGER collaboration_events_no_update;
-- +goose StatementBegin
CREATE TRIGGER collaboration_events_no_update BEFORE UPDATE ON collaboration_events
WHEN NEW.rowid IS NOT OLD.rowid
 OR NEW.id IS NOT OLD.id
 OR NEW.organization_id IS NOT OLD.organization_id
 OR NEW.project_id IS NOT OLD.project_id
 OR NEW.work_item_id IS NOT OLD.work_item_id
 OR NEW.sequence IS NOT OLD.sequence
 OR NEW.type IS NOT OLD.type
 OR NEW.schema_version IS NOT OLD.schema_version
 OR NEW.actor_json IS NOT OLD.actor_json
 OR NEW.recorded_at IS NOT OLD.recorded_at
 OR ((NEW.data_json IS NOT OLD.data_json OR NEW.payload_version IS NOT OLD.payload_version OR NEW.payload_attachment_id IS NOT OLD.payload_attachment_id OR NEW.payload_entry IS NOT OLD.payload_entry OR NEW.payload_object_key IS NOT OLD.payload_object_key OR NEW.payload_sha256 IS NOT OLD.payload_sha256 OR NEW.payload_bytes IS NOT OLD.payload_bytes OR NEW.payload_content_bytes IS NOT OLD.payload_content_bytes) AND NOT EXISTS (SELECT 1 FROM storage_conversion_progress
 WHERE family='runs' AND conversion_rowid=OLD.rowid AND conversion_rowid<>0
 AND conversion_before_sha256=storage_conversion_digest(OLD.data_json,OLD.payload_version,OLD.payload_attachment_id,OLD.payload_entry,OLD.payload_object_key,OLD.payload_sha256,OLD.payload_bytes,OLD.payload_content_bytes)
 AND conversion_after_sha256=storage_conversion_digest(NEW.data_json,NEW.payload_version,NEW.payload_attachment_id,NEW.payload_entry,NEW.payload_object_key,NEW.payload_sha256,NEW.payload_bytes,NEW.payload_content_bytes))
 AND NOT EXISTS (SELECT 1 FROM event_compactions
 WHERE OLD.payload_version=0 AND NEW.payload_version=OLD.payload_version AND NEW.payload_attachment_id=OLD.payload_attachment_id AND NEW.payload_entry=OLD.payload_entry AND NEW.payload_object_key=OLD.payload_object_key AND NEW.payload_sha256=OLD.payload_sha256 AND NEW.payload_bytes=OLD.payload_bytes AND NEW.payload_content_bytes=OLD.payload_content_bytes
 AND organization_id=OLD.organization_id AND project_id=OLD.project_id
 AND aggregate_kind='work_item' AND aggregate_id=OLD.work_item_id
 AND mutation_sequence=OLD.sequence AND mutation_delete=0
 AND mutation_before_sha256=storage_conversion_digest(OLD.data_json)
 AND mutation_after_sha256=storage_conversion_digest(NEW.data_json)))
BEGIN SELECT RAISE(ABORT, 'collaboration events are immutable'); END;
-- +goose StatementEnd
DROP TRIGGER storage_conversion_enqueue_runs;
-- +goose StatementBegin
CREATE TRIGGER storage_conversion_enqueue_runs AFTER INSERT ON collaboration_events
BEGIN
INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,format_version,through_rowid,updated_at)
SELECT NEW.organization_id,NEW.project_id,'runs',NEW.work_item_id,1,NEW.rowid,''
WHERE NEW.payload_version=0 AND NEW.type IN ('run.started','run.checkpointed','run.observed','run.finished','scheduler.decision')
ON CONFLICT(organization_id,project_id,family,aggregate_id) DO UPDATE SET
 through_rowid=max(through_rowid,excluded.through_rowid),complete=0,format_version=1;
UPDATE collaboration_events SET conversion_state=CASE WHEN changes()=0 THEN 2 ELSE 1 END WHERE rowid=NEW.rowid;
END;
-- +goose StatementEnd

-- +goose Down
CREATE TEMP TABLE collaboration_payload_rollback_check (marker INTEGER);
-- +goose StatementBegin
CREATE TEMP TRIGGER collaboration_payload_rollback_check BEFORE INSERT ON collaboration_payload_rollback_check
WHEN EXISTS(SELECT 1 FROM collaboration_events WHERE payload_version=1 LIMIT 1)
BEGIN SELECT RAISE(ABORT, 'archived collaboration payloads require a forward migration'); END;
-- +goose StatementEnd
INSERT INTO collaboration_payload_rollback_check VALUES(1);
DROP TABLE collaboration_payload_rollback_check;
DROP TRIGGER collaboration_events_no_update;
-- +goose StatementBegin
CREATE TRIGGER collaboration_events_no_update BEFORE UPDATE ON collaboration_events
WHEN NEW.rowid IS NOT OLD.rowid
 OR NEW.id IS NOT OLD.id
 OR NEW.organization_id IS NOT OLD.organization_id
 OR NEW.project_id IS NOT OLD.project_id
 OR NEW.work_item_id IS NOT OLD.work_item_id
 OR NEW.sequence IS NOT OLD.sequence
 OR NEW.type IS NOT OLD.type
 OR NEW.schema_version IS NOT OLD.schema_version
 OR NEW.actor_json IS NOT OLD.actor_json
 OR NEW.recorded_at IS NOT OLD.recorded_at
 OR (NEW.data_json IS NOT OLD.data_json AND NOT EXISTS (SELECT 1 FROM storage_conversion_progress
 WHERE family='runs' AND conversion_rowid=OLD.rowid AND conversion_rowid<>0
 AND conversion_before_sha256=storage_conversion_digest(OLD.data_json)
 AND conversion_after_sha256=storage_conversion_digest(NEW.data_json))
 AND NOT EXISTS (SELECT 1 FROM event_compactions
 WHERE organization_id=OLD.organization_id AND project_id=OLD.project_id
 AND aggregate_kind='work_item' AND aggregate_id=OLD.work_item_id
 AND mutation_sequence=OLD.sequence AND mutation_delete=0
 AND mutation_before_sha256=storage_conversion_digest(OLD.data_json)
 AND mutation_after_sha256=storage_conversion_digest(NEW.data_json)))
BEGIN SELECT RAISE(ABORT, 'collaboration events are immutable'); END;
-- +goose StatementEnd
DROP TRIGGER storage_conversion_enqueue_runs;
-- +goose StatementBegin
CREATE TRIGGER storage_conversion_enqueue_runs AFTER INSERT ON collaboration_events
BEGIN
INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,format_version,through_rowid,updated_at)
SELECT r.organization_id,r.project_id,'runs',r.work_item_id,1,r.rowid,'' FROM collaboration_events r
WHERE (r.type IN ('run.started','run.checkpointed','run.observed','run.finished')) AND r.rowid=NEW.rowid
ON CONFLICT(organization_id,project_id,family,aggregate_id) DO UPDATE SET
 through_rowid=max(through_rowid,excluded.through_rowid),
 cursor_rowid=CASE WHEN format_version<>1 THEN 0 ELSE cursor_rowid END,
 complete=0,
 format_version=1;
UPDATE collaboration_events SET conversion_state=CASE WHEN changes()=0 THEN 2 ELSE 1 END WHERE rowid=NEW.rowid;
END;
-- +goose StatementEnd
DROP INDEX storage_conversion_pending_scopes;
DROP INDEX storage_discovery_runs;
CREATE INDEX storage_discovery_runs ON collaboration_events(conversion_state) WHERE conversion_state=0;
ALTER TABLE collaboration_events DROP COLUMN push_checkpoint_head_sha;
ALTER TABLE collaboration_events DROP COLUMN push_effect_id;
ALTER TABLE collaboration_events DROP COLUMN run_runner_id;
ALTER TABLE collaboration_events DROP COLUMN run_session_id;
ALTER TABLE collaboration_events DROP COLUMN run_machine_id;
ALTER TABLE collaboration_events DROP COLUMN run_outcome;
ALTER TABLE collaboration_events DROP COLUMN scheduler_change_present;
ALTER TABLE collaboration_events DROP COLUMN scheduler_head_sha;
ALTER TABLE collaboration_events DROP COLUMN scheduler_version_id;
ALTER TABLE collaboration_events DROP COLUMN scheduler_change_id;
ALTER TABLE collaboration_events DROP COLUMN payload_content_bytes;
ALTER TABLE collaboration_events DROP COLUMN payload_bytes;
ALTER TABLE collaboration_events DROP COLUMN payload_sha256;
ALTER TABLE collaboration_events DROP COLUMN payload_object_key;
ALTER TABLE collaboration_events DROP COLUMN payload_entry;
ALTER TABLE collaboration_events DROP COLUMN payload_attachment_id;
ALTER TABLE collaboration_events DROP COLUMN payload_version;

-- +goose Up

ALTER TABLE storage_conversion_progress ADD COLUMN conversion_rowid INTEGER NOT NULL DEFAULT 0;
ALTER TABLE storage_conversion_progress ADD COLUMN conversion_before_sha256 TEXT NOT NULL DEFAULT '';
ALTER TABLE storage_conversion_progress ADD COLUMN conversion_after_sha256 TEXT NOT NULL DEFAULT '';
CREATE INDEX storage_conversion_pending ON storage_conversion_progress(organization_id,project_id,family,updated_at,aggregate_id) WHERE complete=0 OR format_version<>1;
CREATE UNIQUE INDEX storage_conversion_authorized ON storage_conversion_progress(family,conversion_rowid) WHERE conversion_rowid<>0;

ALTER TABLE event_compactions ADD COLUMN mutation_sequence INTEGER NOT NULL DEFAULT 0;
ALTER TABLE event_compactions ADD COLUMN mutation_delete INTEGER NOT NULL DEFAULT 0 CHECK(mutation_delete IN (0,1));
ALTER TABLE event_compactions ADD COLUMN mutation_before_sha256 TEXT NOT NULL DEFAULT '';
ALTER TABLE event_compactions ADD COLUMN mutation_after_sha256 TEXT NOT NULL DEFAULT '';
ALTER TABLE native_archive_sections ADD COLUMN retirement_rowid INTEGER NOT NULL DEFAULT 0;
CREATE UNIQUE INDEX native_archive_retirement_row ON native_archive_sections(retirement_rowid) WHERE retirement_rowid<>0;

DROP TRIGGER collaboration_events_no_delete;
-- +goose StatementBegin
CREATE TRIGGER collaboration_events_no_delete BEFORE DELETE ON collaboration_events
WHEN NOT EXISTS (SELECT 1 FROM event_compactions
 WHERE organization_id=OLD.organization_id AND project_id=OLD.project_id
 AND aggregate_kind='work_item' AND aggregate_id=OLD.work_item_id
 AND mutation_sequence=OLD.sequence AND mutation_delete=1)
BEGIN SELECT RAISE(ABORT, 'collaboration events are immutable'); END;
-- +goose StatementEnd

DROP TRIGGER collaboration_versions_no_delete;
-- +goose StatementBegin
CREATE TRIGGER collaboration_versions_no_delete BEFORE DELETE ON collaboration_versions
WHEN NOT EXISTS (SELECT 1 FROM native_archive_sections
 WHERE retirement_rowid=OLD.rowid AND retirement_rowid<>0
 AND organization_id=OLD.organization_id AND project_id=OLD.project_id
 AND work_item_id=OLD.work_item_id AND kind='revision' AND subject_id=OLD.record_id)
BEGIN SELECT RAISE(ABORT, 'collaboration history is append-only'); END;
-- +goose StatementEnd

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

DROP TRIGGER change_versions_immutable;
-- +goose StatementBegin
CREATE TRIGGER change_versions_immutable BEFORE UPDATE ON change_versions
WHEN NEW.rowid IS NOT OLD.rowid
 OR NEW.id IS NOT OLD.id
 OR NEW.change_id IS NOT OLD.change_id
 OR NEW.number IS NOT OLD.number
 OR NEW.policy_revision_id IS NOT OLD.policy_revision_id
 OR (NEW.record_json IS NOT OLD.record_json AND NOT EXISTS (SELECT 1 FROM storage_conversion_progress
 WHERE family='versions' AND conversion_rowid=OLD.rowid AND conversion_rowid<>0
 AND conversion_before_sha256=storage_conversion_digest(OLD.record_json)
 AND conversion_after_sha256=storage_conversion_digest(NEW.record_json)))
BEGIN SELECT RAISE(ABORT, 'change versions are immutable'); END;
-- +goose StatementEnd

DROP TRIGGER change_evidence_immutable;
-- +goose StatementBegin
CREATE TRIGGER change_evidence_immutable BEFORE UPDATE ON change_evidence
WHEN NEW.sequence IS NOT OLD.sequence
 OR NEW.change_id IS NOT OLD.change_id
 OR NEW.version_id IS NOT OLD.version_id
 OR NEW.kind IS NOT OLD.kind
 OR NEW.source_key IS NOT OLD.source_key
 OR (NEW.record_json IS NOT OLD.record_json AND NOT EXISTS (SELECT 1 FROM storage_conversion_progress
 WHERE family='evidence' AND conversion_rowid=OLD.rowid AND conversion_rowid<>0
 AND conversion_before_sha256=storage_conversion_digest(OLD.record_json)
 AND conversion_after_sha256=storage_conversion_digest(NEW.record_json)))
BEGIN SELECT RAISE(ABORT, 'change evidence is immutable'); END;
-- +goose StatementEnd

DROP TRIGGER change_sources_no_update;
-- +goose StatementBegin
CREATE TRIGGER change_sources_no_update BEFORE UPDATE ON change_sources
WHEN NEW.rowid IS NOT OLD.rowid
 OR NEW.version_id IS NOT OLD.version_id
 OR ((substr(OLD.bundle,1,4) = x'00444353'
  OR NOT (
    (substr(NEW.bundle,1,6) = x'004443530101' AND length(NEW.bundle) < length(OLD.bundle))
    OR (substr(NEW.bundle,1,6) = x'004443530100' AND substr(NEW.bundle,7) = OLD.bundle)
  )) AND NOT EXISTS (SELECT 1 FROM storage_conversion_progress
 WHERE family='sources' AND conversion_rowid=OLD.rowid AND conversion_rowid<>0
 AND conversion_before_sha256=storage_conversion_digest(OLD.bundle,OLD.object_key,OLD.object_sha256,OLD.object_bytes,OLD.content_bytes)
 AND conversion_after_sha256=storage_conversion_digest(NEW.bundle,NEW.object_key,NEW.object_sha256,NEW.object_bytes,NEW.content_bytes)))
BEGIN SELECT RAISE(ABORT, 'change source is immutable'); END;
-- +goose StatementEnd

DROP TRIGGER change_sources_immutable_reference;
-- +goose StatementBegin
CREATE TRIGGER change_sources_immutable_reference BEFORE UPDATE ON change_sources
WHEN (NEW.object_key IS NOT OLD.object_key OR NEW.object_sha256 IS NOT OLD.object_sha256
 OR NEW.object_bytes IS NOT OLD.object_bytes OR NEW.content_bytes IS NOT OLD.content_bytes) AND NOT EXISTS (SELECT 1 FROM storage_conversion_progress
 WHERE family='sources' AND conversion_rowid=OLD.rowid AND conversion_rowid<>0
 AND conversion_before_sha256=storage_conversion_digest(OLD.bundle,OLD.object_key,OLD.object_sha256,OLD.object_bytes,OLD.content_bytes)
 AND conversion_after_sha256=storage_conversion_digest(NEW.bundle,NEW.object_key,NEW.object_sha256,NEW.object_bytes,NEW.content_bytes))
BEGIN SELECT RAISE(ABORT, 'change source reference is immutable'); END;
-- +goose StatementEnd

INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,format_version,through_rowid,updated_at)
SELECT r.organization_id,r.project_id,'runs',r.work_item_id,1,max(r.rowid),'' FROM collaboration_events r
WHERE (r.type IN ('run.started','run.checkpointed','run.observed','run.finished')) GROUP BY r.organization_id,r.project_id,r.work_item_id
ON CONFLICT(organization_id,project_id,family,aggregate_id) DO UPDATE SET
 through_rowid=max(through_rowid,excluded.through_rowid),
 cursor_rowid=CASE WHEN format_version<>1 THEN 0 ELSE cursor_rowid END,
 complete=CASE WHEN format_version<>1 OR excluded.through_rowid>cursor_rowid THEN 0 ELSE complete END,
 format_version=1;

-- +goose StatementBegin
CREATE TRIGGER storage_conversion_enqueue_runs AFTER INSERT ON collaboration_events
BEGIN
INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,format_version,through_rowid,updated_at)
SELECT r.organization_id,r.project_id,'runs',r.work_item_id,1,r.rowid,'' FROM collaboration_events r
WHERE (r.type IN ('run.started','run.checkpointed','run.observed','run.finished')) AND r.rowid=NEW.rowid
ON CONFLICT(organization_id,project_id,family,aggregate_id) DO UPDATE SET
 through_rowid=max(through_rowid,excluded.through_rowid),
 cursor_rowid=CASE WHEN format_version<>1 THEN 0 ELSE cursor_rowid END,
 complete=CASE WHEN format_version<>1 OR excluded.through_rowid>cursor_rowid THEN 0 ELSE complete END,
 format_version=1;
END;
-- +goose StatementEnd

INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,format_version,through_rowid,updated_at)
SELECT r.organization_id,r.project_id,'attempts',r.work_item_id,1,max(r.rowid),'' FROM native_attempts r
WHERE (1) GROUP BY r.organization_id,r.project_id,r.work_item_id
ON CONFLICT(organization_id,project_id,family,aggregate_id) DO UPDATE SET
 through_rowid=max(through_rowid,excluded.through_rowid),
 cursor_rowid=CASE WHEN format_version<>1 THEN 0 ELSE cursor_rowid END,
 complete=CASE WHEN format_version<>1 OR excluded.through_rowid>cursor_rowid THEN 0 ELSE complete END,
 format_version=1;

-- +goose StatementBegin
CREATE TRIGGER storage_conversion_enqueue_attempts AFTER INSERT ON native_attempts
BEGIN
INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,format_version,through_rowid,updated_at)
SELECT r.organization_id,r.project_id,'attempts',r.work_item_id,1,r.rowid,'' FROM native_attempts r
WHERE (1) AND r.rowid=NEW.rowid
ON CONFLICT(organization_id,project_id,family,aggregate_id) DO UPDATE SET
 through_rowid=max(through_rowid,excluded.through_rowid),
 cursor_rowid=CASE WHEN format_version<>1 THEN 0 ELSE cursor_rowid END,
 complete=CASE WHEN format_version<>1 OR excluded.through_rowid>cursor_rowid THEN 0 ELSE complete END,
 format_version=1;
END;
-- +goose StatementEnd

INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,format_version,through_rowid,updated_at)
SELECT r.organization_id,r.project_id,'analytics','',1,max(r.rowid),'' FROM analytics_hourly_rollups r
WHERE (r.metric='attempt') GROUP BY r.organization_id,r.project_id,''
ON CONFLICT(organization_id,project_id,family,aggregate_id) DO UPDATE SET
 through_rowid=max(through_rowid,excluded.through_rowid),
 cursor_rowid=CASE WHEN format_version<>1 THEN 0 ELSE cursor_rowid END,
 complete=CASE WHEN format_version<>1 OR excluded.through_rowid>cursor_rowid THEN 0 ELSE complete END,
 format_version=1;

-- +goose StatementBegin
CREATE TRIGGER storage_conversion_enqueue_analytics AFTER INSERT ON analytics_hourly_rollups
BEGIN
INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,format_version,through_rowid,updated_at)
SELECT r.organization_id,r.project_id,'analytics','',1,r.rowid,'' FROM analytics_hourly_rollups r
WHERE (r.metric='attempt') AND r.rowid=NEW.rowid
ON CONFLICT(organization_id,project_id,family,aggregate_id) DO UPDATE SET
 through_rowid=max(through_rowid,excluded.through_rowid),
 cursor_rowid=CASE WHEN format_version<>1 THEN 0 ELSE cursor_rowid END,
 complete=CASE WHEN format_version<>1 OR excluded.through_rowid>cursor_rowid THEN 0 ELSE complete END,
 format_version=1;
END;
-- +goose StatementEnd

INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,format_version,through_rowid,updated_at)
SELECT c.organization_id,c.project_id,'conversation_events',r.conversation_id,1,max(r.rowid),'' FROM conversation_events r JOIN conversations c ON c.id=r.conversation_id
WHERE (r.type IN ('message.accepted','message.updated')) GROUP BY c.organization_id,c.project_id,r.conversation_id
ON CONFLICT(organization_id,project_id,family,aggregate_id) DO UPDATE SET
 through_rowid=max(through_rowid,excluded.through_rowid),
 cursor_rowid=CASE WHEN format_version<>1 THEN 0 ELSE cursor_rowid END,
 complete=CASE WHEN format_version<>1 OR excluded.through_rowid>cursor_rowid THEN 0 ELSE complete END,
 format_version=1;

-- +goose StatementBegin
CREATE TRIGGER storage_conversion_enqueue_conversation_events AFTER INSERT ON conversation_events
BEGIN
INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,format_version,through_rowid,updated_at)
SELECT c.organization_id,c.project_id,'conversation_events',r.conversation_id,1,r.rowid,'' FROM conversation_events r JOIN conversations c ON c.id=r.conversation_id
WHERE (r.type IN ('message.accepted','message.updated')) AND r.rowid=NEW.rowid
ON CONFLICT(organization_id,project_id,family,aggregate_id) DO UPDATE SET
 through_rowid=max(through_rowid,excluded.through_rowid),
 cursor_rowid=CASE WHEN format_version<>1 THEN 0 ELSE cursor_rowid END,
 complete=CASE WHEN format_version<>1 OR excluded.through_rowid>cursor_rowid THEN 0 ELSE complete END,
 format_version=1;
END;
-- +goose StatementEnd

INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,format_version,through_rowid,updated_at)
SELECT c.organization_id,c.project_id,'messages',r.conversation_id,1,max(r.rowid),'' FROM conversation_messages r JOIN conversations c ON c.id=r.conversation_id
WHERE (r.content_attachment_id='') GROUP BY c.organization_id,c.project_id,r.conversation_id
ON CONFLICT(organization_id,project_id,family,aggregate_id) DO UPDATE SET
 through_rowid=max(through_rowid,excluded.through_rowid),
 cursor_rowid=CASE WHEN format_version<>1 THEN 0 ELSE cursor_rowid END,
 complete=CASE WHEN format_version<>1 OR excluded.through_rowid>cursor_rowid THEN 0 ELSE complete END,
 format_version=1;

-- +goose StatementBegin
CREATE TRIGGER storage_conversion_enqueue_messages AFTER INSERT ON conversation_messages
BEGIN
INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,format_version,through_rowid,updated_at)
SELECT c.organization_id,c.project_id,'messages',r.conversation_id,1,r.rowid,'' FROM conversation_messages r JOIN conversations c ON c.id=r.conversation_id
WHERE (r.content_attachment_id='') AND r.rowid=NEW.rowid
ON CONFLICT(organization_id,project_id,family,aggregate_id) DO UPDATE SET
 through_rowid=max(through_rowid,excluded.through_rowid),
 cursor_rowid=CASE WHEN format_version<>1 THEN 0 ELSE cursor_rowid END,
 complete=CASE WHEN format_version<>1 OR excluded.through_rowid>cursor_rowid THEN 0 ELSE complete END,
 format_version=1;
END;
-- +goose StatementEnd

INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,format_version,through_rowid,updated_at)
SELECT c.organization_id,c.project_id,'versions',c.work_item_id,1,max(r.rowid),'' FROM change_versions r JOIN change_requests c ON c.id=r.change_id
WHERE (1) GROUP BY c.organization_id,c.project_id,c.work_item_id
ON CONFLICT(organization_id,project_id,family,aggregate_id) DO UPDATE SET
 through_rowid=max(through_rowid,excluded.through_rowid),
 cursor_rowid=CASE WHEN format_version<>1 THEN 0 ELSE cursor_rowid END,
 complete=CASE WHEN format_version<>1 OR excluded.through_rowid>cursor_rowid THEN 0 ELSE complete END,
 format_version=1;

-- +goose StatementBegin
CREATE TRIGGER storage_conversion_enqueue_versions AFTER INSERT ON change_versions
BEGIN
INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,format_version,through_rowid,updated_at)
SELECT c.organization_id,c.project_id,'versions',c.work_item_id,1,r.rowid,'' FROM change_versions r JOIN change_requests c ON c.id=r.change_id
WHERE (1) AND r.rowid=NEW.rowid
ON CONFLICT(organization_id,project_id,family,aggregate_id) DO UPDATE SET
 through_rowid=max(through_rowid,excluded.through_rowid),
 cursor_rowid=CASE WHEN format_version<>1 THEN 0 ELSE cursor_rowid END,
 complete=CASE WHEN format_version<>1 OR excluded.through_rowid>cursor_rowid THEN 0 ELSE complete END,
 format_version=1;
END;
-- +goose StatementEnd

INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,format_version,through_rowid,updated_at)
SELECT c.organization_id,c.project_id,'evidence',c.work_item_id,1,max(r.rowid),'' FROM change_evidence r JOIN change_requests c ON c.id=r.change_id
WHERE (1) GROUP BY c.organization_id,c.project_id,c.work_item_id
ON CONFLICT(organization_id,project_id,family,aggregate_id) DO UPDATE SET
 through_rowid=max(through_rowid,excluded.through_rowid),
 cursor_rowid=CASE WHEN format_version<>1 THEN 0 ELSE cursor_rowid END,
 complete=CASE WHEN format_version<>1 OR excluded.through_rowid>cursor_rowid THEN 0 ELSE complete END,
 format_version=1;

-- +goose StatementBegin
CREATE TRIGGER storage_conversion_enqueue_evidence AFTER INSERT ON change_evidence
BEGIN
INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,format_version,through_rowid,updated_at)
SELECT c.organization_id,c.project_id,'evidence',c.work_item_id,1,r.rowid,'' FROM change_evidence r JOIN change_requests c ON c.id=r.change_id
WHERE (1) AND r.rowid=NEW.rowid
ON CONFLICT(organization_id,project_id,family,aggregate_id) DO UPDATE SET
 through_rowid=max(through_rowid,excluded.through_rowid),
 cursor_rowid=CASE WHEN format_version<>1 THEN 0 ELSE cursor_rowid END,
 complete=CASE WHEN format_version<>1 OR excluded.through_rowid>cursor_rowid THEN 0 ELSE complete END,
 format_version=1;
END;
-- +goose StatementEnd

INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,format_version,through_rowid,updated_at)
SELECT c.organization_id,c.project_id,'sources',c.work_item_id,1,max(r.rowid),'' FROM change_sources r JOIN change_versions v ON v.id=r.version_id JOIN change_requests c ON c.id=v.change_id
WHERE (r.object_key='') GROUP BY c.organization_id,c.project_id,c.work_item_id
ON CONFLICT(organization_id,project_id,family,aggregate_id) DO UPDATE SET
 through_rowid=max(through_rowid,excluded.through_rowid),
 cursor_rowid=CASE WHEN format_version<>1 THEN 0 ELSE cursor_rowid END,
 complete=CASE WHEN format_version<>1 OR excluded.through_rowid>cursor_rowid THEN 0 ELSE complete END,
 format_version=1;

-- +goose StatementBegin
CREATE TRIGGER storage_conversion_enqueue_sources AFTER INSERT ON change_sources
BEGIN
INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,format_version,through_rowid,updated_at)
SELECT c.organization_id,c.project_id,'sources',c.work_item_id,1,r.rowid,'' FROM change_sources r JOIN change_versions v ON v.id=r.version_id JOIN change_requests c ON c.id=v.change_id
WHERE (r.object_key='') AND r.rowid=NEW.rowid
ON CONFLICT(organization_id,project_id,family,aggregate_id) DO UPDATE SET
 through_rowid=max(through_rowid,excluded.through_rowid),
 cursor_rowid=CASE WHEN format_version<>1 THEN 0 ELSE cursor_rowid END,
 complete=CASE WHEN format_version<>1 OR excluded.through_rowid>cursor_rowid THEN 0 ELSE complete END,
 format_version=1;
END;
-- +goose StatementEnd

INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,format_version,through_rowid,updated_at)
SELECT c.organization_id,c.project_id,'diff_files',c.work_item_id,1,max(r.rowid),'' FROM attempt_diff_files r JOIN attempt_diffs c ON c.id=r.diff_id
WHERE (c.object_key='') GROUP BY c.organization_id,c.project_id,c.work_item_id
ON CONFLICT(organization_id,project_id,family,aggregate_id) DO UPDATE SET
 through_rowid=max(through_rowid,excluded.through_rowid),
 cursor_rowid=CASE WHEN format_version<>1 THEN 0 ELSE cursor_rowid END,
 complete=CASE WHEN format_version<>1 OR excluded.through_rowid>cursor_rowid THEN 0 ELSE complete END,
 format_version=1;

-- +goose StatementBegin
CREATE TRIGGER storage_conversion_enqueue_diff_files AFTER INSERT ON attempt_diff_files
BEGIN
INSERT INTO storage_conversion_progress(organization_id,project_id,family,aggregate_id,format_version,through_rowid,updated_at)
SELECT c.organization_id,c.project_id,'diff_files',c.work_item_id,1,r.rowid,'' FROM attempt_diff_files r JOIN attempt_diffs c ON c.id=r.diff_id
WHERE (c.object_key='') AND r.rowid=NEW.rowid
ON CONFLICT(organization_id,project_id,family,aggregate_id) DO UPDATE SET
 through_rowid=max(through_rowid,excluded.through_rowid),
 cursor_rowid=CASE WHEN format_version<>1 THEN 0 ELSE cursor_rowid END,
 complete=CASE WHEN format_version<>1 OR excluded.through_rowid>cursor_rowid THEN 0 ELSE complete END,
 format_version=1;
END;
-- +goose StatementEnd

-- +goose Down

DROP TRIGGER storage_conversion_enqueue_runs;

DROP TRIGGER storage_conversion_enqueue_attempts;

DROP TRIGGER storage_conversion_enqueue_analytics;

DROP TRIGGER storage_conversion_enqueue_conversation_events;

DROP TRIGGER storage_conversion_enqueue_messages;

DROP TRIGGER storage_conversion_enqueue_versions;

DROP TRIGGER storage_conversion_enqueue_evidence;

DROP TRIGGER storage_conversion_enqueue_sources;

DROP TRIGGER storage_conversion_enqueue_diff_files;

DROP TRIGGER change_evidence_immutable;
-- +goose StatementBegin
CREATE TRIGGER change_evidence_immutable BEFORE UPDATE ON change_evidence
WHEN 1
BEGIN SELECT RAISE(ABORT, 'change evidence is immutable'); END;
-- +goose StatementEnd

DROP TRIGGER change_versions_immutable;
-- +goose StatementBegin
CREATE TRIGGER change_versions_immutable BEFORE UPDATE ON change_versions
WHEN 1
BEGIN SELECT RAISE(ABORT, 'change versions are immutable'); END;
-- +goose StatementEnd

DROP TRIGGER collaboration_events_no_update;
-- +goose StatementBegin
CREATE TRIGGER collaboration_events_no_update BEFORE UPDATE OF id,organization_id,project_id,work_item_id,sequence,type,schema_version,actor_json,data_json,recorded_at ON collaboration_events
BEGIN SELECT RAISE(ABORT, 'collaboration events are immutable'); END;
-- +goose StatementEnd

DROP TRIGGER change_sources_no_update;
-- +goose StatementBegin
CREATE TRIGGER change_sources_no_update BEFORE UPDATE ON change_sources
WHEN NEW.version_id IS NOT OLD.version_id
  OR substr(OLD.bundle,1,4) = x'00444353'
  OR NOT (
    (substr(NEW.bundle,1,6) = x'004443530101' AND length(NEW.bundle) < length(OLD.bundle))
    OR (substr(NEW.bundle,1,6) = x'004443530100' AND substr(NEW.bundle,7) = OLD.bundle)
  )
BEGIN SELECT RAISE(ABORT, 'change source is immutable'); END;
-- +goose StatementEnd

DROP TRIGGER change_sources_immutable_reference;
-- +goose StatementBegin
CREATE TRIGGER change_sources_immutable_reference BEFORE UPDATE ON change_sources
WHEN NEW.object_key IS NOT OLD.object_key OR NEW.object_sha256 IS NOT OLD.object_sha256
 OR NEW.object_bytes IS NOT OLD.object_bytes OR NEW.content_bytes IS NOT OLD.content_bytes
BEGIN SELECT RAISE(ABORT, 'change source reference is immutable'); END;
-- +goose StatementEnd

DROP INDEX storage_conversion_authorized;

DROP INDEX storage_conversion_pending;

ALTER TABLE storage_conversion_progress DROP COLUMN conversion_after_sha256;

ALTER TABLE storage_conversion_progress DROP COLUMN conversion_before_sha256;

ALTER TABLE storage_conversion_progress DROP COLUMN conversion_rowid;

DROP TRIGGER collaboration_events_no_delete;
-- +goose StatementBegin
CREATE TRIGGER collaboration_events_no_delete BEFORE DELETE ON collaboration_events
BEGIN SELECT RAISE(ABORT, 'collaboration events are immutable'); END;
-- +goose StatementEnd
DROP TRIGGER collaboration_versions_no_delete;
-- +goose StatementBegin
CREATE TRIGGER collaboration_versions_no_delete BEFORE DELETE ON collaboration_versions
BEGIN SELECT RAISE(ABORT, 'collaboration history is append-only'); END;
-- +goose StatementEnd
DROP INDEX native_archive_retirement_row;
ALTER TABLE native_archive_sections DROP COLUMN retirement_rowid;
ALTER TABLE event_compactions DROP COLUMN mutation_after_sha256;
ALTER TABLE event_compactions DROP COLUMN mutation_before_sha256;
ALTER TABLE event_compactions DROP COLUMN mutation_delete;
ALTER TABLE event_compactions DROP COLUMN mutation_sequence;

-- +goose Up
ALTER TABLE native_attempts ADD COLUMN payload_object_key TEXT NOT NULL DEFAULT '';
ALTER TABLE change_versions ADD COLUMN payload_object_key TEXT NOT NULL DEFAULT '';
ALTER TABLE change_evidence ADD COLUMN payload_object_key TEXT NOT NULL DEFAULT '';
ALTER TABLE storage_conversion_progress ADD COLUMN conversion_policy_revision_id TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN checkpoint_object_key TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN checkpoint_summary_ready INTEGER NOT NULL DEFAULT 0;
ALTER TABLE native_attempts ADD COLUMN checkpoint_resume TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN checkpoint_availability TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN checkpoint_storage TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN checkpoint_worktree_state TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN checkpoint_head_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN checkpoint_workspace_digest TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN checkpoint_expected_head_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN checkpoint_effect_id TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN checkpoint_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN checkpoint_commit_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN checkpoint_base_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN checkpoint_tree_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN checkpoint_change_id_summary TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN checkpoint_version_id_summary TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN checkpoint_change_head_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE native_attempts ADD COLUMN checkpoint_publication_version_id TEXT NOT NULL DEFAULT '';
CREATE INDEX native_attempts_payload_pending ON native_attempts(payload_object_key) WHERE payload_object_key='';
CREATE INDEX change_versions_payload_pending ON change_versions(payload_object_key) WHERE payload_object_key='';
CREATE INDEX change_evidence_payload_pending ON change_evidence(payload_object_key) WHERE payload_object_key='';
CREATE INDEX native_attempts_checkpoint_pending ON native_attempts(checkpoint_object_key) WHERE checkpoint_object_key='' AND checkpoint_json IS NOT NULL AND checkpoint_json<>'null';
CREATE INDEX native_attempts_payload_scope ON native_attempts(organization_id,project_id,work_item_id) WHERE payload_object_key='';
CREATE INDEX native_attempts_checkpoint_scope ON native_attempts(organization_id,project_id,work_item_id) WHERE checkpoint_object_key='' AND checkpoint_json IS NOT NULL AND checkpoint_json<>'null';
CREATE INDEX change_versions_payload_change ON change_versions(change_id) WHERE payload_object_key='';
CREATE INDEX change_evidence_payload_change ON change_evidence(change_id) WHERE payload_object_key='';
CREATE INDEX storage_conversion_legacy_objects ON storage_conversion_progress(family,format_version) WHERE format_version<>2;
CREATE INDEX change_content_objects_key ON change_content_objects(organization_id,object_key);
CREATE INDEX storage_conversion_pending_work ON storage_conversion_progress(updated_at,organization_id,project_id,family,aggregate_id) WHERE (complete=0 OR format_version<>1) AND family NOT IN ('attempts','checkpoints','versions','evidence','sources','diff_files');
DROP TRIGGER change_versions_immutable;
-- +goose StatementBegin
CREATE TRIGGER change_versions_immutable BEFORE UPDATE ON change_versions
WHEN NEW.rowid IS NOT OLD.rowid
 OR NEW.id IS NOT OLD.id
 OR NEW.change_id IS NOT OLD.change_id
 OR NEW.number IS NOT OLD.number
 OR (NEW.policy_revision_id IS NOT OLD.policy_revision_id AND NOT (OLD.policy_revision_id='' AND EXISTS (SELECT 1 FROM storage_conversion_progress
 WHERE family='versions' AND conversion_rowid=OLD.rowid AND conversion_rowid<>0
 AND conversion_policy_revision_id=NEW.policy_revision_id
 AND conversion_before_sha256=storage_conversion_digest(OLD.record_json,OLD.payload_object_key)
 AND conversion_after_sha256=storage_conversion_digest(NEW.record_json,NEW.payload_object_key))))
 OR ((NEW.record_json IS NOT OLD.record_json OR NEW.payload_object_key IS NOT OLD.payload_object_key) AND NOT EXISTS (SELECT 1 FROM storage_conversion_progress
 WHERE family='versions' AND conversion_rowid=OLD.rowid AND conversion_rowid<>0
 AND conversion_before_sha256=storage_conversion_digest(OLD.record_json,OLD.payload_object_key)
 AND conversion_after_sha256=storage_conversion_digest(NEW.record_json,NEW.payload_object_key)))
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
 OR ((NEW.record_json IS NOT OLD.record_json OR NEW.payload_object_key IS NOT OLD.payload_object_key) AND NOT EXISTS (SELECT 1 FROM storage_conversion_progress
 WHERE family='evidence' AND conversion_rowid=OLD.rowid AND conversion_rowid<>0
 AND conversion_before_sha256=storage_conversion_digest(OLD.record_json,OLD.payload_object_key)
 AND conversion_after_sha256=storage_conversion_digest(NEW.record_json,NEW.payload_object_key)))
BEGIN SELECT RAISE(ABORT, 'change evidence are immutable'); END;
-- +goose StatementEnd

-- +goose Down
CREATE TEMP TABLE payload_rollback_guard(value INTEGER CHECK(value=0));
INSERT INTO payload_rollback_guard SELECT EXISTS(SELECT 1 FROM native_attempts WHERE payload_object_key<>'' OR checkpoint_object_key<>'') OR EXISTS(SELECT 1 FROM change_versions WHERE payload_object_key<>'') OR EXISTS(SELECT 1 FROM change_evidence WHERE payload_object_key<>'');
DROP TABLE payload_rollback_guard;
DROP INDEX storage_conversion_legacy_objects;
DROP INDEX change_evidence_payload_change;
DROP INDEX change_versions_payload_change;
DROP INDEX native_attempts_checkpoint_scope;
DROP INDEX native_attempts_payload_scope;
DROP INDEX storage_conversion_pending_work;
DROP INDEX change_content_objects_key;
DROP INDEX native_attempts_checkpoint_pending;
DROP INDEX native_attempts_payload_pending;
DROP INDEX change_versions_payload_pending;
DROP INDEX change_evidence_payload_pending;
ALTER TABLE native_attempts DROP COLUMN checkpoint_summary_ready;
ALTER TABLE native_attempts DROP COLUMN checkpoint_resume;
ALTER TABLE native_attempts DROP COLUMN checkpoint_availability;
ALTER TABLE native_attempts DROP COLUMN checkpoint_storage;
ALTER TABLE native_attempts DROP COLUMN checkpoint_worktree_state;
ALTER TABLE native_attempts DROP COLUMN checkpoint_head_sha;
ALTER TABLE native_attempts DROP COLUMN checkpoint_workspace_digest;
ALTER TABLE native_attempts DROP COLUMN checkpoint_expected_head_sha;
ALTER TABLE native_attempts DROP COLUMN checkpoint_effect_id;
ALTER TABLE native_attempts DROP COLUMN checkpoint_ref;
ALTER TABLE native_attempts DROP COLUMN checkpoint_commit_sha;
ALTER TABLE native_attempts DROP COLUMN checkpoint_base_sha;
ALTER TABLE native_attempts DROP COLUMN checkpoint_tree_sha;
ALTER TABLE native_attempts DROP COLUMN checkpoint_change_id_summary;
ALTER TABLE native_attempts DROP COLUMN checkpoint_version_id_summary;
ALTER TABLE native_attempts DROP COLUMN checkpoint_change_head_sha;
ALTER TABLE native_attempts DROP COLUMN checkpoint_publication_version_id;
ALTER TABLE native_attempts DROP COLUMN checkpoint_object_key;
DROP TRIGGER change_versions_immutable;
DROP TRIGGER change_evidence_immutable;
ALTER TABLE storage_conversion_progress DROP COLUMN conversion_policy_revision_id;
ALTER TABLE native_attempts DROP COLUMN payload_object_key;
ALTER TABLE change_versions DROP COLUMN payload_object_key;
ALTER TABLE change_evidence DROP COLUMN payload_object_key;
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

-- +goose Up
ALTER TABLE collaboration_versions ADD COLUMN payload_object_key TEXT NOT NULL DEFAULT '';
ALTER TABLE collaboration_versions ADD COLUMN base_revision INTEGER NOT NULL DEFAULT 0;
CREATE INDEX storage_keyset_collaboration_versions ON collaboration_versions(payload_object_key) WHERE payload_object_key='';
CREATE INDEX collaboration_versions_payload_scope ON collaboration_versions(organization_id,project_id,work_item_id) WHERE payload_object_key='';
DROP TRIGGER collaboration_versions_no_update;
-- +goose StatementBegin
CREATE TRIGGER collaboration_versions_no_update BEFORE UPDATE ON collaboration_versions
WHEN NEW.rowid IS NOT OLD.rowid
 OR NEW.organization_id IS NOT OLD.organization_id
 OR NEW.project_id IS NOT OLD.project_id
 OR NEW.work_item_id IS NOT OLD.work_item_id
 OR NEW.record_id IS NOT OLD.record_id
 OR NEW.revision IS NOT OLD.revision
 OR NEW.encoding IS NOT OLD.encoding
 OR NOT (OLD.payload_object_key='' AND NEW.payload_object_key<>'' AND NEW.record_json='{}'
 AND NEW.base_revision=CASE WHEN OLD.encoding='patch' THEN OLD.revision-1 ELSE 0 END
 AND EXISTS (SELECT 1 FROM storage_conversion_progress
 WHERE organization_id=OLD.organization_id AND project_id=OLD.project_id AND aggregate_id=OLD.work_item_id
 AND family='collaboration_versions' AND format_version=2 AND conversion_rowid=OLD.rowid AND conversion_rowid<>0
 AND conversion_before_sha256=storage_conversion_digest(OLD.record_json,OLD.payload_object_key,OLD.base_revision)
 AND conversion_after_sha256=storage_conversion_digest(NEW.record_json,NEW.payload_object_key,NEW.base_revision)))
BEGIN SELECT RAISE(ABORT, 'collaboration history is append-only'); END;
-- +goose StatementEnd

-- +goose Down
CREATE TEMP TABLE collaboration_version_rollback_check (marker INTEGER);
-- +goose StatementBegin
CREATE TEMP TRIGGER collaboration_version_rollback_check BEFORE INSERT ON collaboration_version_rollback_check
WHEN EXISTS(SELECT 1 FROM collaboration_versions WHERE payload_object_key<>'' LIMIT 1)
BEGIN SELECT RAISE(ABORT, 'archived collaboration versions require a forward migration'); END;
-- +goose StatementEnd
INSERT INTO collaboration_version_rollback_check VALUES(1);
DROP TABLE collaboration_version_rollback_check;
DROP TRIGGER collaboration_versions_no_update;
DROP INDEX storage_keyset_collaboration_versions;
DROP INDEX collaboration_versions_payload_scope;
ALTER TABLE collaboration_versions DROP COLUMN base_revision;
ALTER TABLE collaboration_versions DROP COLUMN payload_object_key;
-- +goose StatementBegin
CREATE TRIGGER collaboration_versions_no_update BEFORE UPDATE ON collaboration_versions
BEGIN SELECT RAISE(ABORT, 'collaboration history is append-only'); END;
-- +goose StatementEnd

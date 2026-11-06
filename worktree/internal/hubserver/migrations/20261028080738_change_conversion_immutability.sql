-- +goose Up
DROP TRIGGER change_versions_immutable;
-- +goose StatementBegin
CREATE TRIGGER change_versions_immutable BEFORE UPDATE ON change_versions
WHEN NEW.rowid IS NOT OLD.rowid
 OR NEW.id IS NOT OLD.id
 OR NEW.change_id IS NOT OLD.change_id
 OR NEW.number IS NOT OLD.number
 OR (NEW.policy_revision_id IS NOT OLD.policy_revision_id AND NOT (
 OLD.policy_revision_id='' AND OLD.head_sha IS NULL AND NEW.head_sha IS NOT NULL
 AND NEW.record_json IS OLD.record_json))
 OR (NEW.record_json IS NOT OLD.record_json AND NOT EXISTS (SELECT 1 FROM storage_conversion_progress
 WHERE family='versions' AND conversion_rowid=OLD.rowid AND conversion_rowid<>0
 AND conversion_before_sha256=storage_conversion_digest(OLD.record_json)
 AND conversion_after_sha256=storage_conversion_digest(NEW.record_json)))
BEGIN SELECT RAISE(ABORT, 'change versions are immutable'); END;
-- +goose StatementEnd

DROP TRIGGER change_evidence_immutable;
-- +goose StatementBegin
CREATE TRIGGER change_evidence_immutable BEFORE UPDATE ON change_evidence
WHEN NEW.rowid IS NOT OLD.rowid
 OR NEW.sequence IS NOT OLD.sequence
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

-- +goose Down
DROP TRIGGER change_versions_immutable;
-- +goose StatementBegin
CREATE TRIGGER change_versions_immutable BEFORE UPDATE OF id,change_id,number,record_json ON change_versions
BEGIN SELECT RAISE(ABORT, 'change versions are immutable'); END;
-- +goose StatementEnd

DROP TRIGGER change_evidence_immutable;
-- +goose StatementBegin
CREATE TRIGGER change_evidence_immutable BEFORE UPDATE OF sequence,change_id,version_id,kind,source_key,record_json ON change_evidence
BEGIN SELECT RAISE(ABORT, 'change evidence is immutable'); END;
-- +goose StatementEnd

-- +goose Up
DROP TRIGGER change_versions_session_immutable;
DROP INDEX change_versions_session_unknown;
CREATE INDEX change_versions_session_unknown ON change_versions(id) WHERE implementing_session_id IS NULL OR implementing_session_id=-1;
-- +goose StatementBegin
CREATE TRIGGER change_versions_session_immutable BEFORE UPDATE OF implementing_session_id,implementing_machine_id ON change_versions
WHEN OLD.implementing_session_id IS NOT NULL
 AND (NEW.implementing_session_id IS NOT OLD.implementing_session_id OR NEW.implementing_machine_id IS NOT OLD.implementing_machine_id)
 AND NOT (OLD.implementing_session_id=-1 AND NEW.implementing_session_id>0 AND NEW.implementing_machine_id IS NOT NULL AND NEW.implementing_machine_id!=''
   AND EXISTS(SELECT 1 FROM native_attempts WHERE id=OLD.attempt_id AND implementing_session_id=NEW.implementing_session_id))
BEGIN
 SELECT RAISE(ABORT, 'implementing session identity is immutable');
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER change_versions_session_immutable;
DROP INDEX change_versions_session_unknown;
CREATE INDEX change_versions_session_unknown ON change_versions(id) WHERE implementing_session_id IS NULL;
-- +goose StatementBegin
CREATE TRIGGER change_versions_session_immutable BEFORE UPDATE OF implementing_session_id,implementing_machine_id ON change_versions
WHEN OLD.implementing_session_id IS NOT NULL AND (NEW.implementing_session_id IS NOT OLD.implementing_session_id OR NEW.implementing_machine_id IS NOT OLD.implementing_machine_id)
BEGIN
 SELECT RAISE(ABORT, 'implementing session identity is immutable');
END;
-- +goose StatementEnd

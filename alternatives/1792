-- +goose Up
ALTER TABLE native_attempts ADD COLUMN implementing_session_id INTEGER;
ALTER TABLE change_versions ADD COLUMN implementing_session_id INTEGER;
ALTER TABLE change_versions ADD COLUMN implementing_machine_id TEXT;
ALTER TABLE change_versions ADD COLUMN session_history_sequence INTEGER NOT NULL DEFAULT 0;
ALTER TABLE change_versions ADD COLUMN session_history_candidate INTEGER NOT NULL DEFAULT 0;
ALTER TABLE change_versions ADD COLUMN session_history_state INTEGER NOT NULL DEFAULT 0;
ALTER TABLE change_versions ADD COLUMN session_history_machine_id TEXT NOT NULL DEFAULT '';
CREATE INDEX change_versions_session_unknown ON change_versions(id) WHERE implementing_session_id IS NULL;
-- +goose StatementBegin
CREATE TRIGGER change_versions_session_immutable BEFORE UPDATE OF implementing_session_id,implementing_machine_id ON change_versions
WHEN OLD.implementing_session_id IS NOT NULL AND (NEW.implementing_session_id IS NOT OLD.implementing_session_id OR NEW.implementing_machine_id IS NOT OLD.implementing_machine_id)
BEGIN
 SELECT RAISE(ABORT, 'implementing session identity is immutable');
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER change_versions_session_immutable;
DROP INDEX change_versions_session_unknown;
ALTER TABLE change_versions DROP COLUMN session_history_machine_id;
ALTER TABLE change_versions DROP COLUMN session_history_state;
ALTER TABLE change_versions DROP COLUMN session_history_candidate;
ALTER TABLE change_versions DROP COLUMN session_history_sequence;
ALTER TABLE change_versions DROP COLUMN implementing_machine_id;
ALTER TABLE change_versions DROP COLUMN implementing_session_id;
ALTER TABLE native_attempts DROP COLUMN implementing_session_id;

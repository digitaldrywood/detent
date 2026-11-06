-- +goose Up
DROP TRIGGER activity_attempt_durations_insert;
DROP TRIGGER activity_attempt_durations_update;
DROP VIEW activity_attempt_duration_projection;
DROP INDEX change_requests_activity_latest;
ALTER TABLE native_attempts DROP COLUMN role;
ALTER TABLE native_attempts ADD COLUMN role TEXT;
ALTER TABLE native_attempts ADD COLUMN activity_projection_version INTEGER;
ALTER TABLE native_attempts ADD COLUMN activity_phases_dropped INTEGER;
ALTER TABLE native_attempts ADD COLUMN activity_checkpoint_change_id TEXT;
ALTER TABLE native_attempts ADD COLUMN activity_publication_url TEXT;
CREATE INDEX native_attempts_activity_conversion ON native_attempts(id) WHERE activity_projection_version IS NULL;
CREATE INDEX native_attempts_activity_legacy_window ON native_attempts(organization_id,project_id,activity_updated_at,activity_started_at,id) WHERE activity_projection_version IS NULL;
CREATE TABLE activity_attempt_phases (
 attempt_id TEXT NOT NULL REFERENCES native_attempts(id) ON DELETE CASCADE,
 position INTEGER NOT NULL,
 name TEXT NOT NULL,
 started_at TEXT NOT NULL,
 finished_at TEXT NOT NULL,
 PRIMARY KEY(attempt_id,position)
);
ALTER TABLE issues ADD COLUMN activity_actor_principal_id TEXT;
CREATE INDEX issues_activity_reporter ON issues(organization_id,activity_actor_principal_id,native_id);
CREATE INDEX issues_activity_actor_conversion ON issues(id) WHERE activity_actor_principal_id IS NULL;
ALTER TABLE change_requests ADD COLUMN activity_created_at REAL;
ALTER TABLE change_requests ADD COLUMN activity_current_version_id TEXT;
CREATE INDEX change_requests_activity_latest ON change_requests(organization_id,project_id,work_item_id,activity_created_at DESC,id DESC);
CREATE INDEX change_requests_activity_legacy ON change_requests(organization_id,project_id,work_item_id,id) WHERE activity_current_version_id IS NULL;
CREATE INDEX change_requests_activity_conversion ON change_requests(id) WHERE activity_current_version_id IS NULL;
ALTER TABLE change_versions ADD COLUMN activity_external_url TEXT;
CREATE INDEX change_versions_activity_conversion ON change_versions(id) WHERE activity_external_url IS NULL;
DROP TRIGGER change_versions_immutable;
-- +goose StatementBegin
CREATE TRIGGER change_versions_immutable BEFORE UPDATE OF id,change_id,number,record_json,policy_revision_id ON change_versions
BEGIN
 SELECT RAISE(ABORT, 'change versions are immutable');
END;
-- +goose StatementEnd

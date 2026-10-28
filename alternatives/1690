-- +goose Up
ALTER TABLE change_requests ADD COLUMN archive_landed_version_id TEXT;
ALTER TABLE change_requests ADD COLUMN archive_landed_at TEXT;
ALTER TABLE change_requests ADD COLUMN archive_landed_merge_sha TEXT;
ALTER TABLE change_requests ADD COLUMN archive_actor_kind TEXT;
ALTER TABLE change_requests ADD COLUMN archive_actor_principal_id TEXT;
ALTER TABLE change_requests ADD COLUMN archive_actor_via_key TEXT;
ALTER TABLE change_requests ADD COLUMN archive_actor_display_name TEXT;
ALTER TABLE change_requests ADD COLUMN archive_actor_agent TEXT;
ALTER TABLE collaboration_events ADD COLUMN archive_operation TEXT;
ALTER TABLE collaboration_events ADD COLUMN archive_from_state TEXT;
ALTER TABLE collaboration_events ADD COLUMN archive_to_state TEXT;
ALTER TABLE collaboration_events ADD COLUMN archive_attempt_id TEXT;
ALTER TABLE collaboration_events ADD COLUMN archive_outcome TEXT;
ALTER TABLE collaboration_events ADD COLUMN archive_landed_commit TEXT;
CREATE INDEX conversations_archive_unknown ON conversations(id) WHERE execution_status IS NULL OR execution_lease_id IS NULL;
CREATE INDEX conversations_project_execution ON conversations(organization_id,project_id,execution_status,id);
CREATE INDEX change_requests_archive_unknown ON change_requests(id) WHERE archive_landed_version_id IS NULL;
CREATE INDEX collaboration_events_archive_unknown ON collaboration_events(id) WHERE archive_operation IS NULL;
CREATE INDEX collaboration_events_restore ON collaboration_events(organization_id,project_id,work_item_id,archive_operation,sequence DESC) WHERE type='issue.edited';
CREATE INDEX change_requests_archive_landed ON change_requests(organization_id,project_id,archive_landed_at,id);
CREATE INDEX change_requests_archive_merge ON change_requests(organization_id,project_id,archive_landed_merge_sha);

DROP TRIGGER collaboration_events_no_update;
-- +goose StatementBegin
CREATE TRIGGER collaboration_events_no_update BEFORE UPDATE OF id,organization_id,project_id,work_item_id,sequence,type,schema_version,actor_json,data_json,recorded_at ON collaboration_events
BEGIN
 SELECT RAISE(ABORT, 'collaboration events are immutable');
END;
-- +goose StatementEnd

-- +goose Down
DROP INDEX change_requests_archive_merge;
DROP INDEX change_requests_archive_landed;
DROP INDEX collaboration_events_restore;
DROP INDEX collaboration_events_archive_unknown;
DROP INDEX change_requests_archive_unknown;
DROP INDEX conversations_project_execution;
DROP INDEX conversations_archive_unknown;
ALTER TABLE collaboration_events DROP COLUMN archive_landed_commit;
ALTER TABLE collaboration_events DROP COLUMN archive_outcome;
ALTER TABLE collaboration_events DROP COLUMN archive_attempt_id;
ALTER TABLE collaboration_events DROP COLUMN archive_to_state;
ALTER TABLE collaboration_events DROP COLUMN archive_from_state;
ALTER TABLE collaboration_events DROP COLUMN archive_operation;
ALTER TABLE change_requests DROP COLUMN archive_actor_agent;
ALTER TABLE change_requests DROP COLUMN archive_actor_display_name;
ALTER TABLE change_requests DROP COLUMN archive_actor_via_key;
ALTER TABLE change_requests DROP COLUMN archive_actor_principal_id;
ALTER TABLE change_requests DROP COLUMN archive_actor_kind;
ALTER TABLE change_requests DROP COLUMN archive_landed_merge_sha;
ALTER TABLE change_requests DROP COLUMN archive_landed_at;
ALTER TABLE change_requests DROP COLUMN archive_landed_version_id;

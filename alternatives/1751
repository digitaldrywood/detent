-- +goose Up
ALTER TABLE issues ADD COLUMN actor_kind TEXT;
ALTER TABLE issues ADD COLUMN actor_principal_id TEXT;
CREATE INDEX issues_actor_unknown ON issues(organization_id,project_id,id) WHERE actor_principal_id IS NULL;
CREATE INDEX issues_actor_principal ON issues(organization_id,project_id,actor_principal_id,native_id);
ALTER TABLE collaboration_events ADD COLUMN page_projection_ready INTEGER;
ALTER TABLE collaboration_events ADD COLUMN actor_kind TEXT;
ALTER TABLE collaboration_events ADD COLUMN actor_principal_id TEXT;
ALTER TABLE collaboration_events ADD COLUMN event_revision INTEGER;
ALTER TABLE collaboration_events ADD COLUMN from_state TEXT;
ALTER TABLE collaboration_events ADD COLUMN to_state TEXT;
ALTER TABLE collaboration_events ADD COLUMN reason TEXT;
ALTER TABLE collaboration_events ADD COLUMN run_present INTEGER;
ALTER TABLE collaboration_events ADD COLUMN edited_title INTEGER;
ALTER TABLE collaboration_events ADD COLUMN edited_body INTEGER;
CREATE INDEX collaboration_events_page_unknown ON collaboration_events(organization_id,project_id,work_item_id,sequence) WHERE page_projection_ready IS NULL;
CREATE INDEX collaboration_events_transition_unknown ON collaboration_events(organization_id,project_id,work_item_id,sequence) WHERE type='workflow.transitioned' AND page_projection_ready IS NULL;
CREATE INDEX collaboration_events_terminal_transition ON collaboration_events(organization_id,project_id,work_item_id,sequence DESC,from_state,to_state) WHERE type='workflow.transitioned';
CREATE INDEX collaboration_events_comment_actor ON collaboration_events(organization_id,project_id,work_item_id,actor_kind,actor_principal_id,sequence) WHERE type='comment.created';
ALTER TABLE native_board_items ADD COLUMN actor_principal_id TEXT;
ALTER TABLE native_board_items ADD COLUMN issue_type TEXT;
ALTER TABLE native_board_items ADD COLUMN verification_state TEXT;
ALTER TABLE native_board_items ADD COLUMN priority INTEGER;
CREATE INDEX native_board_actor_unknown ON native_board_items(organization_id,project_id,work_item_id) WHERE actor_principal_id IS NULL;
CREATE TABLE native_board_filter_values (
 work_item_id TEXT NOT NULL REFERENCES native_board_items(work_item_id) ON DELETE CASCADE,
 kind TEXT NOT NULL CHECK(kind IN ('label','assignee')),
 value TEXT NOT NULL,
 PRIMARY KEY(work_item_id,kind,value)
) WITHOUT ROWID;
ALTER TABLE change_versions ADD COLUMN external_url TEXT;
DROP TRIGGER change_versions_immutable;
-- +goose StatementBegin
CREATE TRIGGER change_versions_immutable BEFORE UPDATE OF id,change_id,number,record_json,policy_revision_id ON change_versions
BEGIN
 SELECT RAISE(ABORT, 'change versions are immutable');
END;
-- +goose StatementEnd
CREATE INDEX change_versions_external_url ON change_versions(external_url,change_id);
CREATE INDEX change_versions_external_unknown ON change_versions(id) WHERE external_url IS NULL;
CREATE VIEW native_issue_actors AS SELECT id,actor_kind,actor_principal_id FROM issues;
CREATE VIEW native_transition_facts AS SELECT id,organization_id,project_id,work_item_id,sequence,recorded_at,from_state,to_state FROM collaboration_events WHERE type='workflow.transitioned';
CREATE VIEW native_workflow_ranks AS SELECT id,board_rank FROM workflow_states;
CREATE VIEW native_board_filter_facts AS SELECT work_item_id,actor_principal_id,issue_type,verification_state,priority FROM native_board_items;
DROP TRIGGER native_board_projects_update;
-- +goose StatementBegin
CREATE TRIGGER native_board_projects_update AFTER UPDATE ON projects
BEGIN
 INSERT INTO native_board_dirty SELECT native_id FROM issues
 WHERE project_id=NEW.id AND native_id IS NOT NULL
 AND (NEW.profile IS NOT OLD.profile OR NEW.require_dependencies IS NOT OLD.require_dependencies OR NEW.repository_id IS NOT OLD.repository_id)
 ON CONFLICT(work_item_id) DO NOTHING;
 UPDATE native_board_items SET lane_rank=COALESCE(
  (SELECT board_rank FROM workflow_states WHERE project_id=NEW.id AND detent_state=native_board_items.state),2147483647)
 WHERE project_id=NEW.id AND NEW.states_json IS NOT OLD.states_json
 AND lane_rank IS NOT COALESCE(
  (SELECT board_rank FROM workflow_states WHERE project_id=NEW.id AND detent_state=native_board_items.state),2147483647);
END;
-- +goose StatementEnd
DROP TRIGGER pull_requests_activity_insert;
-- +goose StatementBegin
CREATE TRIGGER pull_requests_activity_insert AFTER INSERT ON pull_requests
BEGIN
  UPDATE issues SET last_activity_at = CASE WHEN rtrim(NEW.synchronized_at, 'Z') > rtrim(NEW.updated_at, 'Z') THEN NEW.synchronized_at ELSE NEW.updated_at END
  WHERE (id = NEW.issue_id OR native_id IN (
    SELECT l.work_item_id FROM change_issue_links l JOIN change_versions v ON v.change_id = l.change_id
    JOIN projects p ON p.organization_id = l.organization_id AND p.id = l.project_id
    WHERE p.repository_id = NEW.repository_id AND v.external_url = NEW.url
  )) AND rtrim(last_activity_at, 'Z') < rtrim(CASE WHEN rtrim(NEW.synchronized_at, 'Z') > rtrim(NEW.updated_at, 'Z') THEN NEW.synchronized_at ELSE NEW.updated_at END, 'Z');
END;
-- +goose StatementEnd
DROP TRIGGER pull_requests_activity_update;
-- +goose StatementBegin
CREATE TRIGGER pull_requests_activity_update AFTER UPDATE OF updated_at ON pull_requests
BEGIN
  UPDATE issues SET last_activity_at = CASE WHEN rtrim(NEW.synchronized_at, 'Z') > rtrim(NEW.updated_at, 'Z') THEN NEW.synchronized_at ELSE NEW.updated_at END
  WHERE (id = NEW.issue_id OR native_id IN (
    SELECT l.work_item_id FROM change_issue_links l JOIN change_versions v ON v.change_id = l.change_id
    JOIN projects p ON p.organization_id = l.organization_id AND p.id = l.project_id
    WHERE p.repository_id = NEW.repository_id AND v.external_url = NEW.url
  )) AND rtrim(last_activity_at, 'Z') < rtrim(CASE WHEN rtrim(NEW.synchronized_at, 'Z') > rtrim(NEW.updated_at, 'Z') THEN NEW.synchronized_at ELSE NEW.updated_at END, 'Z');
END;
-- +goose StatementEnd
DROP TRIGGER native_board_pull_requests_insert;
-- +goose StatementBegin
CREATE TRIGGER native_board_pull_requests_insert AFTER INSERT ON pull_requests
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE id=NEW.issue_id AND native_id IS NOT NULL
 UNION SELECT l.work_item_id FROM change_issue_links l JOIN change_versions v ON v.change_id=l.change_id
 JOIN projects p ON p.id=l.project_id WHERE p.repository_id=NEW.repository_id AND v.external_url=NEW.url) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd
DROP TRIGGER native_board_pull_requests_update;
-- +goose StatementBegin
CREATE TRIGGER native_board_pull_requests_update AFTER UPDATE ON pull_requests
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE id=NEW.issue_id AND native_id IS NOT NULL
 UNION SELECT l.work_item_id FROM change_issue_links l JOIN change_versions v ON v.change_id=l.change_id
 JOIN projects p ON p.id=l.project_id WHERE p.repository_id=NEW.repository_id AND v.external_url=NEW.url) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd
DROP TRIGGER native_board_pull_requests_delete;
-- +goose StatementBegin
CREATE TRIGGER native_board_pull_requests_delete AFTER DELETE ON pull_requests
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE id=OLD.issue_id AND native_id IS NOT NULL
 UNION SELECT l.work_item_id FROM change_issue_links l JOIN change_versions v ON v.change_id=l.change_id
 JOIN projects p ON p.id=l.project_id WHERE p.repository_id=OLD.repository_id AND v.external_url=OLD.url) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose Down
SELECT 1;

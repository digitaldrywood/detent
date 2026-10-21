-- +goose Up
DROP TRIGGER native_board_projects_update;
DROP TRIGGER native_board_workflow_states_update;
DROP TRIGGER native_board_change_review_policies_insert;
DROP TRIGGER native_board_change_review_policies_update;
DROP TRIGGER native_board_change_review_policies_delete;
DROP TRIGGER native_board_project_policies_insert;
DROP TRIGGER native_board_project_policies_update;
DROP TRIGGER native_board_project_policies_delete;

-- +goose StatementBegin
CREATE TRIGGER native_board_projects_update AFTER UPDATE ON projects
BEGIN
 INSERT INTO native_board_dirty SELECT native_id FROM issues
 WHERE project_id=NEW.id AND native_id IS NOT NULL
 AND (NEW.profile IS NOT OLD.profile OR NEW.require_dependencies IS NOT OLD.require_dependencies OR NEW.repository_id IS NOT OLD.repository_id)
 ON CONFLICT(work_item_id) DO NOTHING;
 UPDATE native_board_items SET lane_rank=COALESCE(
  (SELECT CAST(key AS INTEGER) FROM json_each(NEW.states_json) WHERE json_extract(value,'$.name')=native_board_items.state),2147483647)
 WHERE project_id=NEW.id AND NEW.states_json IS NOT OLD.states_json
 AND lane_rank IS NOT COALESCE(
  (SELECT CAST(key AS INTEGER) FROM json_each(NEW.states_json) WHERE json_extract(value,'$.name')=native_board_items.state),2147483647);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_workflow_states_update AFTER UPDATE ON workflow_states
WHEN NEW.detent_state IS NOT OLD.detent_state OR NEW.terminal IS NOT OLD.terminal
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (
  SELECT native_id FROM issues WHERE workflow_state_id=NEW.id AND native_id IS NOT NULL
  UNION SELECT i.native_id FROM issues i JOIN issue_dependencies d ON d.dependent_issue_id=i.id
  JOIN issues b ON b.id=d.blocker_issue_id WHERE b.workflow_state_id=NEW.id AND i.native_id IS NOT NULL
 ) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_change_review_policies_insert AFTER INSERT ON change_review_policies
BEGIN
 INSERT INTO native_board_dirty SELECT DISTINCT l.work_item_id FROM change_issue_links l
 WHERE l.organization_id=NEW.organization_id AND l.project_id=NEW.project_id
 ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_change_review_policies_update AFTER UPDATE ON change_review_policies
WHEN NEW.policy_json IS NOT OLD.policy_json
BEGIN
 INSERT INTO native_board_dirty SELECT DISTINCT l.work_item_id FROM change_issue_links l
 WHERE l.organization_id=NEW.organization_id AND l.project_id=NEW.project_id
 ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_change_review_policies_delete AFTER DELETE ON change_review_policies
BEGIN
 INSERT INTO native_board_dirty SELECT DISTINCT l.work_item_id FROM change_issue_links l
 WHERE l.organization_id=OLD.organization_id AND l.project_id=OLD.project_id
 ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_project_policies_insert AFTER INSERT ON project_policies
BEGIN
 INSERT INTO native_board_dirty SELECT DISTINCT l.work_item_id FROM change_issue_links l
 WHERE l.organization_id||'/'||l.project_id=NEW.scope
 ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_project_policies_update AFTER UPDATE ON project_policies
WHEN NEW.policy_id IS NOT OLD.policy_id
BEGIN
 INSERT INTO native_board_dirty SELECT DISTINCT l.work_item_id FROM change_issue_links l
 WHERE l.organization_id||'/'||l.project_id=NEW.scope
 ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_project_policies_delete AFTER DELETE ON project_policies
BEGIN
 INSERT INTO native_board_dirty SELECT DISTINCT l.work_item_id FROM change_issue_links l
 WHERE l.organization_id||'/'||l.project_id=OLD.scope
 ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER native_board_projects_update;
DROP TRIGGER native_board_workflow_states_update;
DROP TRIGGER native_board_change_review_policies_insert;
DROP TRIGGER native_board_change_review_policies_update;
DROP TRIGGER native_board_change_review_policies_delete;
DROP TRIGGER native_board_project_policies_insert;
DROP TRIGGER native_board_project_policies_update;
DROP TRIGGER native_board_project_policies_delete;

-- +goose StatementBegin
CREATE TRIGGER native_board_projects_update AFTER UPDATE ON projects
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE project_id=NEW.id AND native_id IS NOT NULL) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_workflow_states_update AFTER UPDATE ON workflow_states
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE project_id=NEW.project_id AND native_id IS NOT NULL) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_change_review_policies_insert AFTER INSERT ON change_review_policies
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE project_id=NEW.project_id AND native_id IS NOT NULL) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_change_review_policies_update AFTER UPDATE ON change_review_policies
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE project_id=NEW.project_id AND native_id IS NOT NULL) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_change_review_policies_delete AFTER DELETE ON change_review_policies
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE project_id=OLD.project_id AND native_id IS NOT NULL) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_project_policies_insert AFTER INSERT ON project_policies
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE organization_id||'/'||project_id=NEW.scope AND native_id IS NOT NULL) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_project_policies_update AFTER UPDATE ON project_policies
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE organization_id||'/'||project_id=NEW.scope AND native_id IS NOT NULL) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_project_policies_delete AFTER DELETE ON project_policies
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE organization_id||'/'||project_id=OLD.scope AND native_id IS NOT NULL) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose Up
DROP TRIGGER native_board_change_review_policies_insert;
DROP TRIGGER native_board_change_review_policies_update;
DROP TRIGGER native_board_change_review_policies_delete;
DROP TRIGGER native_board_project_policies_insert;
DROP TRIGGER native_board_project_policies_update;
DROP TRIGGER native_board_project_policies_delete;

-- +goose Down
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


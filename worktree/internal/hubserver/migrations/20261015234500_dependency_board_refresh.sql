-- +goose Up
DROP TRIGGER native_board_issues_update;
DROP TRIGGER native_board_dependency_insert;
DROP TRIGGER native_board_dependency_delete;

-- +goose StatementBegin
CREATE TRIGGER native_board_issues_update AFTER UPDATE ON issues
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT NEW.native_id WHERE NEW.native_id IS NOT NULL) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
 INSERT INTO native_board_dirty SELECT * FROM (SELECT i.native_id FROM issues i JOIN issue_dependencies d ON d.dependent_issue_id=i.id WHERE d.blocker_issue_id=NEW.id AND NEW.blocked_reason_revision=OLD.blocked_reason_revision AND NEW.unblocks_reason_revision=OLD.unblocks_reason_revision) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
 INSERT INTO native_board_dirty SELECT * FROM (SELECT i.native_id FROM issues i JOIN issue_dependencies d ON d.blocker_issue_id=i.id WHERE d.dependent_issue_id=NEW.id AND NEW.blocked_reason_revision=OLD.blocked_reason_revision AND NEW.unblocks_reason_revision=OLD.unblocks_reason_revision) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_dependency_insert AFTER INSERT ON issue_dependencies
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE id IN (NEW.dependent_issue_id,NEW.blocker_issue_id) AND native_id IS NOT NULL) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_dependency_delete AFTER DELETE ON issue_dependencies
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE id IN (OLD.dependent_issue_id,OLD.blocker_issue_id) AND native_id IS NOT NULL) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER native_board_issues_update;
DROP TRIGGER native_board_dependency_insert;
DROP TRIGGER native_board_dependency_delete;

-- +goose StatementBegin
CREATE TRIGGER native_board_issues_update AFTER UPDATE ON issues
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT NEW.native_id WHERE NEW.native_id IS NOT NULL) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
 INSERT INTO native_board_dirty SELECT * FROM (SELECT i.native_id FROM issues i JOIN issue_dependencies d ON d.dependent_issue_id=i.id WHERE d.blocker_issue_id=NEW.id) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_dependency_insert AFTER INSERT ON issue_dependencies
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE id=NEW.dependent_issue_id AND native_id IS NOT NULL) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER native_board_dependency_delete AFTER DELETE ON issue_dependencies
BEGIN
 INSERT INTO native_board_dirty SELECT * FROM (SELECT native_id FROM issues WHERE id=OLD.dependent_issue_id AND native_id IS NOT NULL) WHERE true ON CONFLICT(work_item_id) DO NOTHING;
END;
-- +goose StatementEnd

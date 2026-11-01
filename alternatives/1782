-- +goose Up
CREATE TABLE attachment_references (
  attachment_id TEXT NOT NULL REFERENCES attachments(id) ON DELETE CASCADE,
  work_item_id TEXT NOT NULL,
  comment_id TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (attachment_id, work_item_id, comment_id)
);
CREATE INDEX attachment_references_source ON attachment_references(work_item_id, comment_id);
INSERT INTO attachment_references(attachment_id,work_item_id,comment_id)
SELECT id,work_item_id,coalesce(comment_id,'') FROM attachments WHERE work_item_id IS NOT NULL AND deleted_at IS NULL;

-- +goose StatementBegin
CREATE TRIGGER attachment_reference_added AFTER INSERT ON attachment_references BEGIN
  UPDATE attachments SET
    comment_id = CASE WHEN work_item_id IS NULL THEN nullif(NEW.comment_id,'') ELSE comment_id END,
    work_item_id = coalesce(work_item_id,NEW.work_item_id)
  WHERE id = NEW.attachment_id;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER attachment_reference_removed AFTER DELETE ON attachment_references BEGIN
  UPDATE attachments SET
    work_item_id = (SELECT work_item_id FROM attachment_references WHERE attachment_id = OLD.attachment_id ORDER BY work_item_id,comment_id LIMIT 1),
    comment_id = (SELECT nullif(comment_id,'') FROM attachment_references WHERE attachment_id = OLD.attachment_id ORDER BY work_item_id,comment_id LIMIT 1)
  WHERE id = OLD.attachment_id;
END;
-- +goose StatementEnd

DROP TRIGGER attachments_issue_deleted;
DROP TRIGGER attachments_comment_deleted;
-- +goose StatementBegin
CREATE TRIGGER attachments_issue_deleted AFTER DELETE ON issues BEGIN
  UPDATE attachments SET deleted_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
  WHERE organization_id = OLD.organization_id AND project_id = OLD.project_id AND deleted_at IS NULL
    AND (work_item_id = OLD.native_id OR id IN (SELECT attachment_id FROM attachment_references WHERE work_item_id = OLD.native_id))
    AND NOT EXISTS (SELECT 1 FROM attachment_references WHERE attachment_id = attachments.id AND work_item_id != OLD.native_id);
  DELETE FROM attachment_references WHERE work_item_id = OLD.native_id
    AND attachment_id IN (SELECT id FROM attachments WHERE organization_id = OLD.organization_id AND project_id = OLD.project_id);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER attachments_comment_deleted AFTER DELETE ON native_comments BEGIN
  UPDATE attachments SET deleted_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
  WHERE organization_id = OLD.organization_id AND project_id = OLD.project_id AND deleted_at IS NULL
    AND (comment_id = OLD.id OR id IN (SELECT attachment_id FROM attachment_references WHERE work_item_id = OLD.work_item_id AND comment_id = OLD.id))
    AND NOT EXISTS (SELECT 1 FROM attachment_references WHERE attachment_id = attachments.id AND (work_item_id != OLD.work_item_id OR comment_id != OLD.id));
  DELETE FROM attachment_references WHERE work_item_id = OLD.work_item_id AND comment_id = OLD.id
    AND attachment_id IN (SELECT id FROM attachments WHERE organization_id = OLD.organization_id AND project_id = OLD.project_id);
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER attachments_comment_deleted;
DROP TRIGGER attachments_issue_deleted;
DROP TRIGGER attachment_reference_removed;
DROP TRIGGER attachment_reference_added;
DROP TABLE attachment_references;
-- +goose StatementBegin
CREATE TRIGGER attachments_issue_deleted AFTER DELETE ON issues BEGIN
  UPDATE attachments SET deleted_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
  WHERE organization_id = OLD.organization_id AND project_id = OLD.project_id AND work_item_id = OLD.native_id AND deleted_at IS NULL;
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER attachments_comment_deleted AFTER DELETE ON native_comments BEGIN
  UPDATE attachments SET deleted_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
  WHERE organization_id = OLD.organization_id AND project_id = OLD.project_id AND comment_id = OLD.id AND deleted_at IS NULL;
END;
-- +goose StatementEnd

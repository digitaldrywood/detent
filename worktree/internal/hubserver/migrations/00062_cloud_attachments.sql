-- +goose Up
CREATE TABLE attachments (
  id TEXT PRIMARY KEY,
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL REFERENCES projects(id),
  uploader TEXT NOT NULL REFERENCES api_tokens(id),
  name TEXT NOT NULL,
  content_type TEXT NOT NULL,
  size INTEGER NOT NULL CHECK (size > 0 AND size <= 20971520),
  sha256 TEXT NOT NULL,
  width INTEGER NOT NULL DEFAULT 0,
  height INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  work_item_id TEXT,
  comment_id TEXT,
  deleted_at TEXT,
  object_deleted_at TEXT,
  CHECK (comment_id IS NULL OR work_item_id IS NOT NULL)
);
CREATE INDEX attachments_retention ON attachments(created_at) WHERE object_deleted_at IS NULL;

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

-- +goose Down
DROP TRIGGER attachments_comment_deleted;
DROP TRIGGER attachments_issue_deleted;
DROP TABLE attachments;

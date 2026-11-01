-- +goose Up
ALTER TABLE change_content_objects ADD COLUMN pending_content BLOB;
ALTER TABLE attachments ADD COLUMN pending_content BLOB;
CREATE INDEX change_content_pending ON change_content_objects(organization_id, object_key) WHERE pending_content IS NOT NULL;
CREATE INDEX attachment_content_pending ON attachments(organization_id, object_key) WHERE pending_content IS NOT NULL;
DROP TRIGGER immutable_attachment_archive;
-- +goose StatementBegin
CREATE TRIGGER immutable_attachment_archive BEFORE UPDATE OF
id,organization_id,project_id,uploader,name,content_type,size,sha256,width,height,created_at,
work_item_id,comment_id,deleted_at,object_deleted_at,evidence_attempt_id,evidence_caption,
kind,retention_class,object_key,uncompressed_size,codec,schema_version,storage_binding_id,customer_owned
ON attachments WHEN OLD.kind='archive' BEGIN SELECT RAISE(ABORT,'immutable archive'); END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER immutable_attachment_pending BEFORE UPDATE OF pending_content ON attachments
WHEN OLD.kind='archive' AND (OLD.pending_content IS NULL OR NEW.pending_content IS NOT NULL)
BEGIN SELECT RAISE(ABORT,'immutable archive publication'); END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER immutable_attachment_pending;
DROP TRIGGER immutable_attachment_archive;
-- +goose StatementBegin
CREATE TRIGGER immutable_attachment_archive BEFORE UPDATE ON attachments
WHEN OLD.kind='archive' BEGIN SELECT RAISE(ABORT,'immutable archive'); END;
-- +goose StatementEnd
DROP INDEX attachment_content_pending;
DROP INDEX change_content_pending;
ALTER TABLE attachments DROP COLUMN pending_content;
ALTER TABLE change_content_objects DROP COLUMN pending_content;

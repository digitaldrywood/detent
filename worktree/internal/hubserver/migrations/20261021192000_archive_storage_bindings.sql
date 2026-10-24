-- +goose Up
ALTER TABLE attachments ADD COLUMN storage_binding_id TEXT NOT NULL DEFAULT '';
ALTER TABLE attachments ADD COLUMN customer_owned INTEGER NOT NULL DEFAULT 0 CHECK(customer_owned IN (0,1));
DROP INDEX attachment_archive_blob;
CREATE UNIQUE INDEX attachment_archive_blob ON attachments(organization_id,storage_binding_id,object_key) WHERE kind='archive';
DROP TRIGGER hosted_usage_attachments_insert;
DROP TRIGGER hosted_usage_attachments_update;
DROP TRIGGER hosted_usage_attachments_delete;
-- +goose StatementBegin
CREATE TRIGGER hosted_usage_attachments_insert AFTER INSERT ON attachments BEGIN
  UPDATE hosted_usage_counters SET value = value + CASE WHEN NEW.object_deleted_at IS NULL AND NEW.customer_owned=0 THEN coalesce(NEW.size,0) ELSE 0 END WHERE name = 'collaboration_bytes';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER hosted_usage_attachments_update AFTER UPDATE OF size, object_deleted_at ON attachments BEGIN
  UPDATE hosted_usage_counters SET value = value - CASE WHEN OLD.object_deleted_at IS NULL AND OLD.customer_owned=0 THEN coalesce(OLD.size,0) ELSE 0 END + CASE WHEN NEW.object_deleted_at IS NULL AND NEW.customer_owned=0 THEN coalesce(NEW.size,0) ELSE 0 END WHERE name = 'collaboration_bytes';
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER hosted_usage_attachments_delete AFTER DELETE ON attachments BEGIN
  UPDATE hosted_usage_counters SET value = value - CASE WHEN OLD.object_deleted_at IS NULL AND OLD.customer_owned=0 THEN coalesce(OLD.size,0) ELSE 0 END WHERE name = 'collaboration_bytes';
END;
-- +goose StatementEnd

-- +goose Down
CREATE TABLE archive_storage_downgrade_guard (value INTEGER CHECK(value=0));
INSERT INTO archive_storage_downgrade_guard VALUES(1);

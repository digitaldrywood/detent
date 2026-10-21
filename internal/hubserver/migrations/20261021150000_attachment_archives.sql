-- +goose Up
ALTER TABLE attachments ADD COLUMN kind TEXT NOT NULL DEFAULT 'attachment' CHECK(kind IN ('attachment','archive'));
ALTER TABLE attachments ADD COLUMN retention_class TEXT NOT NULL DEFAULT 'expiring' CHECK(retention_class IN ('expiring','organization'));
ALTER TABLE attachments ADD COLUMN object_key TEXT NOT NULL DEFAULT '';
ALTER TABLE attachments ADD COLUMN uncompressed_size INTEGER NOT NULL DEFAULT 0 CHECK(uncompressed_size >= 0);
ALTER TABLE attachments ADD COLUMN codec TEXT NOT NULL DEFAULT '';
ALTER TABLE attachments ADD COLUMN schema_version INTEGER NOT NULL DEFAULT 0 CHECK(schema_version >= 0);
CREATE UNIQUE INDEX attachment_archive_blob ON attachments(organization_id,object_key) WHERE kind='archive';
CREATE TABLE attachment_archive_references (
  organization_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  work_item_id TEXT NOT NULL,
  attachment_id TEXT NOT NULL REFERENCES attachments(id),
  PRIMARY KEY(organization_id,project_id,work_item_id,attachment_id)
);

-- +goose StatementBegin
CREATE TRIGGER immutable_attachment_archive BEFORE UPDATE ON attachments
WHEN OLD.kind='archive' BEGIN SELECT RAISE(ABORT,'immutable archive'); END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER immutable_attachment_archive;
DROP TABLE attachment_archive_references;
DROP INDEX attachment_archive_blob;
ALTER TABLE attachments DROP COLUMN schema_version;
ALTER TABLE attachments DROP COLUMN codec;
ALTER TABLE attachments DROP COLUMN uncompressed_size;
ALTER TABLE attachments DROP COLUMN object_key;
ALTER TABLE attachments DROP COLUMN retention_class;
ALTER TABLE attachments DROP COLUMN kind;

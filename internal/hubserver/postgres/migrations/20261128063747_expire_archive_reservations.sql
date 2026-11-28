-- +goose NO TRANSACTION
-- +goose Up
ALTER TABLE attachments ADD COLUMN IF NOT EXISTS archive_upload_expires_at timestamptz;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION immutable_attachment_archive() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
 stable attachments%ROWTYPE;
BEGIN
 IF OLD.kind<>'archive' THEN RETURN NEW; END IF;
 stable := NEW;
 stable.deleted_at := OLD.deleted_at;
 stable.object_deleted_at := OLD.object_deleted_at;
 stable.archive_upload_expires_at := OLD.archive_upload_expires_at;
 IF stable IS DISTINCT FROM OLD
 OR OLD.object_key !~ '^orgs/[^/]+/blobs/sha256/[0-9a-f]{2}/[0-9a-f]{64}/[0-9a-f]{64}$'
 OR NOT (
  (NEW.archive_upload_expires_at IS NOT DISTINCT FROM OLD.archive_upload_expires_at AND (
   (OLD.deleted_at IS NULL AND NEW.deleted_at IS NOT NULL AND NEW.object_deleted_at IS NULL)
   OR (OLD.deleted_at IS NOT NULL AND NEW.deleted_at IS NOT DISTINCT FROM OLD.deleted_at
       AND OLD.object_deleted_at IS NULL AND NEW.object_deleted_at IS NOT NULL)
  ))
  OR (OLD.deleted_at IS NULL AND NEW.deleted_at IS NULL
      AND OLD.object_deleted_at IS NULL AND NEW.object_deleted_at IS NULL
      AND OLD.archive_upload_expires_at IS NOT NULL
      AND (NEW.archive_upload_expires_at IS NULL OR NEW.archive_upload_expires_at > OLD.archive_upload_expires_at))
 ) THEN
  RAISE EXCEPTION 'immutable archive' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE INDEX CONCURRENTLY IF NOT EXISTS attachments_archive_upload_expiry ON attachments (organization_id,archive_upload_expires_at,id)
WHERE kind='archive' AND pending_content IS NULL AND deleted_at IS NULL AND object_deleted_at IS NULL AND archive_upload_expires_at IS NOT NULL;
CREATE INDEX CONCURRENTLY IF NOT EXISTS attachment_archive_references_attachment ON attachment_archive_references (organization_id,attachment_id);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS attachment_archive_references_attachment;
DROP INDEX CONCURRENTLY IF EXISTS attachments_archive_upload_expiry;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION immutable_attachment_archive() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
 stable attachments%ROWTYPE;
BEGIN
 IF OLD.kind<>'archive' THEN RETURN NEW; END IF;
 stable := NEW;
 stable.deleted_at := OLD.deleted_at;
 stable.object_deleted_at := OLD.object_deleted_at;
 IF stable IS DISTINCT FROM OLD
 OR OLD.object_key !~ '^orgs/[^/]+/blobs/sha256/[0-9a-f]{2}/[0-9a-f]{64}/[0-9a-f]{64}$'
 OR NOT (
  (OLD.deleted_at IS NULL AND NEW.deleted_at IS NOT NULL AND NEW.object_deleted_at IS NULL)
  OR (OLD.deleted_at IS NOT NULL AND NEW.deleted_at IS NOT DISTINCT FROM OLD.deleted_at
      AND OLD.object_deleted_at IS NULL AND NEW.object_deleted_at IS NOT NULL)
 ) THEN
  RAISE EXCEPTION 'immutable archive' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER TABLE attachments DROP COLUMN IF EXISTS archive_upload_expires_at;
